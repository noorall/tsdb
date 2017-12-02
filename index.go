package tsdb

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync"

	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/pkg/bytesutil"
	"github.com/influxdata/influxdb/pkg/estimator"
	"github.com/influxdata/influxdb/query"
	"github.com/influxdata/influxql"
	"go.uber.org/zap"
)

type Index interface {
	Open() error
	Close() error
	WithLogger(*zap.Logger)

	Database() string
	MeasurementExists(name []byte) (bool, error)
	MeasurementNamesByExpr(expr influxql.Expr) ([][]byte, error)
	MeasurementNamesByRegex(re *regexp.Regexp) ([][]byte, error)
	DropMeasurement(name []byte) error
	ForEachMeasurementName(fn func(name []byte) error) error

	InitializeSeries(key, name []byte, tags models.Tags) error
	CreateSeriesIfNotExists(key, name []byte, tags models.Tags) error
	CreateSeriesListIfNotExists(keys, names [][]byte, tags []models.Tags) error
	DropSeries(key []byte, ts int64) error

	MeasurementsSketches() (estimator.Sketch, estimator.Sketch, error)
	SeriesN() int64

	HasTagKey(name, key []byte) (bool, error)
	// TagSets(name []byte, options query.IteratorOptions) ([]*query.TagSet, error)
	MeasurementTagKeysByExpr(name []byte, expr influxql.Expr) (map[string]struct{}, error)
	// MeasurementTagKeyValuesByExpr(auth query.Authorizer, name []byte, keys []string, expr influxql.Expr, keysSorted bool) ([][]string, error)

	ForEachMeasurementTagKey(name []byte, fn func(key []byte) error) error
	TagKeyCardinality(name, key []byte) int

	// InfluxQL system iterators
	MeasurementIterator() (MeasurementIterator, error)
	TagValueIterator(auth query.Authorizer, name, key []byte) (TagValueIterator, error)
	MeasurementSeriesIDIterator(name []byte) (SeriesIDIterator, error)
	TagKeySeriesIDIterator(name, key []byte) (SeriesIDIterator, error)
	TagValueSeriesIDIterator(name, key, value []byte) (SeriesIDIterator, error)
	// MeasurementSeriesKeysByExprIterator(name []byte, condition influxql.Expr) (SeriesIDIterator, error)
	// MeasurementSeriesKeysByExpr(name []byte, condition influxql.Expr) ([][]byte, error)
	// SeriesIDIterator(opt query.IteratorOptions) (SeriesIDIterator, error)

	// Sets a shared fieldset from the engine.
	FieldSet() *MeasurementFieldSet
	SetFieldSet(fs *MeasurementFieldSet)

	// Creates hard links inside path for snapshotting.
	SnapshotTo(path string) error

	// To be removed w/ tsi1.
	SetFieldName(measurement []byte, name string)
	AssignShard(k string, shardID uint64)
	UnassignShard(k string, shardID uint64, ts int64) error
	RemoveShard(shardID uint64)

	Type() string

	Rebuild()
}

// SeriesElem represents a generic series element.
type SeriesElem interface {
	Name() []byte
	Tags() models.Tags
	Deleted() bool

	// InfluxQL expression associated with series during filtering.
	Expr() influxql.Expr
}

// SeriesIterator represents a iterator over a list of series.
type SeriesIterator interface {
	Close() error
	Next() (SeriesElem, error)
}

// NewSeriesIteratorAdapter returns an adapter for converting series ids to series.
func NewSeriesIteratorAdapter(sfile *SeriesFile, itr SeriesIDIterator) SeriesIterator {
	return &seriesIteratorAdapter{
		sfile: sfile,
		itr:   itr,
	}
}

type seriesIteratorAdapter struct {
	sfile *SeriesFile
	itr   SeriesIDIterator
}

func (itr *seriesIteratorAdapter) Close() error { return itr.itr.Close() }

func (itr *seriesIteratorAdapter) Next() (SeriesElem, error) {
	elem, err := itr.itr.Next()
	if err != nil {
		return nil, err
	} else if elem.SeriesID == 0 {
		return nil, nil
	}

	name, tags := ParseSeriesKey(itr.sfile.SeriesKey(elem.SeriesID))
	deleted := itr.sfile.IsDeleted(elem.SeriesID)

	return &seriesElemAdapter{
		name:    name,
		tags:    tags,
		deleted: deleted,
		expr:    elem.Expr,
	}, nil
}

type seriesElemAdapter struct {
	name    []byte
	tags    models.Tags
	deleted bool
	expr    influxql.Expr
}

func (e *seriesElemAdapter) Name() []byte        { return e.name }
func (e *seriesElemAdapter) Tags() models.Tags   { return e.tags }
func (e *seriesElemAdapter) Deleted() bool       { return e.deleted }
func (e *seriesElemAdapter) Expr() influxql.Expr { return e.expr }

// SeriesIDElem represents a single series and optional expression.
type SeriesIDElem struct {
	SeriesID uint64
	Expr     influxql.Expr
}

// SeriesIDElems represents a list of series id elements.
type SeriesIDElems []SeriesIDElem

func (a SeriesIDElems) Len() int           { return len(a) }
func (a SeriesIDElems) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a SeriesIDElems) Less(i, j int) bool { return a[i].SeriesID < a[j].SeriesID }

// SeriesIDIterator represents a iterator over a list of series ids.
type SeriesIDIterator interface {
	Next() (SeriesIDElem, error)
	Close() error
}

// NewSeriesIDSliceIterator returns a SeriesIDIterator that iterates over a slice.
func NewSeriesIDSliceIterator(ids []uint64) *SeriesIDSliceIterator {
	return &SeriesIDSliceIterator{ids: ids}
}

// SeriesIDSliceIterator iterates over a slice of series ids.
type SeriesIDSliceIterator struct {
	ids []uint64
}

// Next returns the next series id in the slice.
func (itr *SeriesIDSliceIterator) Next() (SeriesIDElem, error) {
	if len(itr.ids) == 0 {
		return SeriesIDElem{}, nil
	}
	id := itr.ids[0]
	itr.ids = itr.ids[1:]
	return SeriesIDElem{SeriesID: id}, nil
}

func (itr *SeriesIDSliceIterator) Close() error { return nil }

type SeriesIDIterators []SeriesIDIterator

func (a SeriesIDIterators) Close() (err error) {
	for i := range a {
		if e := a[i].Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// seriesQueryAdapterIterator adapts SeriesIDIterator to an influxql.Iterator.
type seriesQueryAdapterIterator struct {
	once     sync.Once
	sfile    *SeriesFile
	itr      SeriesIDIterator
	fieldset *MeasurementFieldSet
	opt      query.IteratorOptions

	point query.FloatPoint // reusable point
}

// NewSeriesQueryAdapterIterator returns a new instance of SeriesQueryAdapterIterator.
func NewSeriesQueryAdapterIterator(sfile *SeriesFile, itr SeriesIDIterator, fieldset *MeasurementFieldSet, opt query.IteratorOptions) query.Iterator {
	return &seriesQueryAdapterIterator{
		sfile:    sfile,
		itr:      itr,
		fieldset: fieldset,
		point: query.FloatPoint{
			Aux: make([]interface{}, len(opt.Aux)),
		},
		opt: opt,
	}
}

// Stats returns stats about the points processed.
func (itr *seriesQueryAdapterIterator) Stats() query.IteratorStats { return query.IteratorStats{} }

// Close closes the iterator.
func (itr *seriesQueryAdapterIterator) Close() error {
	itr.once.Do(func() {
		itr.itr.Close()
	})
	return nil
}

// Next emits the next point in the iterator.
func (itr *seriesQueryAdapterIterator) Next() (*query.FloatPoint, error) {
	for {
		// Read next series element.
		e, err := itr.itr.Next()
		if err != nil {
			return nil, err
		} else if e.SeriesID == 0 {
			return nil, nil
		}

		// Convert to a key.
		name, tags := ParseSeriesKey(itr.sfile.SeriesKey(e.SeriesID))
		key := string(models.MakeKey(name, tags))

		// Write auxiliary fields.
		for i, f := range itr.opt.Aux {
			switch f.Val {
			case "key":
				itr.point.Aux[i] = key
			}
		}
		return &itr.point, nil
	}
}

// filterUndeletedSeriesIDIterator returns all series which are not deleted.
type filterUndeletedSeriesIDIterator struct {
	sfile *SeriesFile
	itr   SeriesIDIterator
}

// FilterUndeletedSeriesIDIterator returns an iterator which filters all deleted series.
func FilterUndeletedSeriesIDIterator(sfile *SeriesFile, itr SeriesIDIterator) SeriesIDIterator {
	if itr == nil {
		return nil
	}
	return &filterUndeletedSeriesIDIterator{sfile: sfile, itr: itr}
}

func (itr *filterUndeletedSeriesIDIterator) Close() error {
	return itr.itr.Close()
}

func (itr *filterUndeletedSeriesIDIterator) Next() (SeriesIDElem, error) {
	for {
		e, err := itr.itr.Next()
		if err != nil {
			return SeriesIDElem{}, err
		} else if e.SeriesID == 0 {
			return SeriesIDElem{}, nil
		} else if itr.sfile.IsDeleted(e.SeriesID) {
			continue
		}
		return e, nil
	}
}

// seriesIDExprIterator is an iterator that attaches an associated expression.
type seriesIDExprIterator struct {
	itr  SeriesIDIterator
	expr influxql.Expr
}

// newSeriesIDExprIterator returns a new instance of seriesIDExprIterator.
func newSeriesIDExprIterator(itr SeriesIDIterator, expr influxql.Expr) SeriesIDIterator {
	if itr == nil {
		return nil
	}

	return &seriesIDExprIterator{
		itr:  itr,
		expr: expr,
	}
}

func (itr *seriesIDExprIterator) Close() error {
	return itr.itr.Close()
}

// Next returns the next element in the iterator.
func (itr *seriesIDExprIterator) Next() (SeriesIDElem, error) {
	elem, err := itr.itr.Next()
	if err != nil {
		return SeriesIDElem{}, err
	} else if elem.SeriesID == 0 {
		return SeriesIDElem{}, nil
	}
	elem.Expr = itr.expr
	return elem, nil
}

// MergeSeriesIDIterators returns an iterator that merges a set of iterators.
// Iterators that are first in the list take precendence and a deletion by those
// early iterators will invalidate elements by later iterators.
func MergeSeriesIDIterators(itrs ...SeriesIDIterator) SeriesIDIterator {
	if n := len(itrs); n == 0 {
		return nil
	} else if n == 1 {
		return itrs[0]
	}

	return &seriesIDMergeIterator{
		buf:  make([]SeriesIDElem, len(itrs)),
		itrs: itrs,
	}
}

// seriesIDMergeIterator is an iterator that merges multiple iterators together.
type seriesIDMergeIterator struct {
	buf  []SeriesIDElem
	itrs []SeriesIDIterator
}

func (itr *seriesIDMergeIterator) Close() error {
	SeriesIDIterators(itr.itrs).Close()
	return nil
}

// Next returns the element with the next lowest name/tags across the iterators.
func (itr *seriesIDMergeIterator) Next() (SeriesIDElem, error) {
	// Find next lowest id amongst the buffers.
	var elem SeriesIDElem
	for i := range itr.buf {
		buf := &itr.buf[i]

		// Fill buffer.
		if buf.SeriesID == 0 {
			elem, err := itr.itrs[i].Next()
			if err != nil {
				return SeriesIDElem{}, nil
			} else if elem.SeriesID == 0 {
				continue
			}
			itr.buf[i] = elem
		}

		if elem.SeriesID == 0 || buf.SeriesID < elem.SeriesID {
			elem = *buf
		}
	}

	// Return EOF if no elements remaining.
	if elem.SeriesID == 0 {
		return SeriesIDElem{}, nil
	}

	// Clear matching buffers.
	for i := range itr.buf {
		if itr.buf[i].SeriesID == elem.SeriesID {
			itr.buf[i].SeriesID = 0
		}
	}
	return elem, nil
}

// IntersectSeriesIDIterators returns an iterator that only returns series which
// occur in both iterators. If both series have associated expressions then
// they are combined together.
func IntersectSeriesIDIterators(itr0, itr1 SeriesIDIterator) SeriesIDIterator {
	if itr0 == nil || itr1 == nil {
		return nil
	}

	return &seriesIDIntersectIterator{itrs: [2]SeriesIDIterator{itr0, itr1}}
}

// seriesIDIntersectIterator is an iterator that merges two iterators together.
type seriesIDIntersectIterator struct {
	buf  [2]SeriesIDElem
	itrs [2]SeriesIDIterator
}

func (itr *seriesIDIntersectIterator) Close() (err error) {
	if e := itr.itrs[0].Close(); e != nil && err == nil {
		err = e
	}
	if e := itr.itrs[1].Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// Next returns the next element which occurs in both iterators.
func (itr *seriesIDIntersectIterator) Next() (_ SeriesIDElem, err error) {
	for {
		// Fill buffers.
		if itr.buf[0].SeriesID == 0 {
			if itr.buf[0], err = itr.itrs[0].Next(); err != nil {
				return SeriesIDElem{}, err
			}
		}
		if itr.buf[1].SeriesID == 0 {
			if itr.buf[1], err = itr.itrs[1].Next(); err != nil {
				return SeriesIDElem{}, err
			}
		}

		// Exit if either buffer is still empty.
		if itr.buf[0].SeriesID == 0 || itr.buf[1].SeriesID == 0 {
			return SeriesIDElem{}, nil
		}

		// Skip if both series are not equal.
		if a, b := itr.buf[0].SeriesID, itr.buf[1].SeriesID; a < b {
			itr.buf[0].SeriesID = 0
			continue
		} else if a > b {
			itr.buf[1].SeriesID = 0
			continue
		}

		// Merge series together if equal.
		elem := itr.buf[0]

		// Attach expression.
		expr0 := itr.buf[0].Expr
		expr1 := itr.buf[1].Expr
		if expr0 == nil {
			elem.Expr = expr1
		} else if expr1 == nil {
			elem.Expr = expr0
		} else {
			elem.Expr = influxql.Reduce(&influxql.BinaryExpr{
				Op:  influxql.AND,
				LHS: expr0,
				RHS: expr1,
			}, nil)
		}

		itr.buf[0].SeriesID, itr.buf[1].SeriesID = 0, 0
		return elem, nil
	}
}

// UnionSeriesIDIterators returns an iterator that returns series from both
// both iterators. If both series have associated expressions then they are
// combined together.
func UnionSeriesIDIterators(itr0, itr1 SeriesIDIterator) SeriesIDIterator {
	// Return other iterator if either one is nil.
	if itr0 == nil {
		return itr1
	} else if itr1 == nil {
		return itr0
	}

	return &seriesIDUnionIterator{itrs: [2]SeriesIDIterator{itr0, itr1}}
}

// seriesIDUnionIterator is an iterator that unions two iterators together.
type seriesIDUnionIterator struct {
	buf  [2]SeriesIDElem
	itrs [2]SeriesIDIterator
}

func (itr *seriesIDUnionIterator) Close() (err error) {
	if e := itr.itrs[0].Close(); e != nil && err == nil {
		err = e
	}
	if e := itr.itrs[1].Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// Next returns the next element which occurs in both iterators.
func (itr *seriesIDUnionIterator) Next() (_ SeriesIDElem, err error) {
	// Fill buffers.
	if itr.buf[0].SeriesID == 0 {
		if itr.buf[0], err = itr.itrs[0].Next(); err != nil {
			return SeriesIDElem{}, err
		}
	}
	if itr.buf[1].SeriesID == 0 {
		if itr.buf[1], err = itr.itrs[1].Next(); err != nil {
			return SeriesIDElem{}, err
		}
	}

	// Return non-zero or lesser series.
	if a, b := itr.buf[0].SeriesID, itr.buf[1].SeriesID; b == 0 || a < b {
		elem := itr.buf[0]
		itr.buf[0].SeriesID = 0
		return elem, nil
	} else if a == 0 || a > b {
		elem := itr.buf[1]
		itr.buf[1].SeriesID = 0
		return elem, nil
	}

	// Attach element.
	elem := itr.buf[0]

	// Attach expression.
	expr0 := itr.buf[0].Expr
	expr1 := itr.buf[1].Expr
	if expr0 != nil && expr1 != nil {
		elem.Expr = influxql.Reduce(&influxql.BinaryExpr{
			Op:  influxql.OR,
			LHS: expr0,
			RHS: expr1,
		}, nil)
	} else {
		elem.Expr = nil
	}

	itr.buf[0].SeriesID, itr.buf[1].SeriesID = 0, 0
	return elem, nil
}

// DifferenceSeriesIDIterators returns an iterator that only returns series which
// occur the first iterator but not the second iterator.
func DifferenceSeriesIDIterators(itr0, itr1 SeriesIDIterator) SeriesIDIterator {
	if itr0 != nil && itr1 == nil {
		return itr0
	} else if itr0 == nil {
		return nil
	}
	return &seriesIDDifferenceIterator{itrs: [2]SeriesIDIterator{itr0, itr1}}
}

// seriesIDDifferenceIterator is an iterator that merges two iterators together.
type seriesIDDifferenceIterator struct {
	buf  [2]SeriesIDElem
	itrs [2]SeriesIDIterator
}

func (itr *seriesIDDifferenceIterator) Close() (err error) {
	if e := itr.itrs[0].Close(); e != nil && err == nil {
		err = e
	}
	if e := itr.itrs[1].Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// Next returns the next element which occurs only in the first iterator.
func (itr *seriesIDDifferenceIterator) Next() (_ SeriesIDElem, err error) {
	for {
		// Fill buffers.
		if itr.buf[0].SeriesID == 0 {
			if itr.buf[0], err = itr.itrs[0].Next(); err != nil {
				return SeriesIDElem{}, err
			}
		}
		if itr.buf[1].SeriesID == 0 {
			if itr.buf[1], err = itr.itrs[1].Next(); err != nil {
				return SeriesIDElem{}, err
			}
		}

		// Exit if first buffer is still empty.
		if itr.buf[0].SeriesID == 0 {
			return SeriesIDElem{}, nil
		} else if itr.buf[1].SeriesID == 0 {
			elem := itr.buf[0]
			itr.buf[0].SeriesID = 0
			return elem, nil
		}

		// Return first series if it's less.
		// If second series is less then skip it.
		// If both series are equal then skip both.
		if a, b := itr.buf[0].SeriesID, itr.buf[1].SeriesID; a < b {
			elem := itr.buf[0]
			itr.buf[0].SeriesID = 0
			return elem, nil
		} else if a > b {
			itr.buf[1].SeriesID = 0
			continue
		} else {
			itr.buf[0].SeriesID, itr.buf[1].SeriesID = 0, 0
			continue
		}
	}
}

// seriesPointIterator adapts SeriesIterator to an influxql.Iterator.
type seriesPointIterator struct {
	once     sync.Once
	sfile    *SeriesFile
	indexSet IndexSet
	fieldset *MeasurementFieldSet
	mitr     MeasurementIterator
	sitr     SeriesIDIterator
	opt      query.IteratorOptions

	point query.FloatPoint // reusable point
}

// newSeriesPointIterator returns a new instance of seriesPointIterator.
func NewSeriesPointIterator(sfile *SeriesFile, indexSet IndexSet, fieldset *MeasurementFieldSet, opt query.IteratorOptions) (_ query.Iterator, err error) {
	// Only equality operators are allowed.
	influxql.WalkFunc(opt.Condition, func(n influxql.Node) {
		switch n := n.(type) {
		case *influxql.BinaryExpr:
			switch n.Op {
			case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX,
				influxql.OR, influxql.AND:
			default:
				err = errors.New("invalid tag comparison operator")
			}
		}
	})
	if err != nil {
		return nil, err
	}

	mitr, err := indexSet.MeasurementIterator()
	if err != nil {
		return nil, err
	}

	return &seriesPointIterator{
		indexSet: indexSet,
		fieldset: fieldset,
		mitr:     mitr,
		point: query.FloatPoint{
			Aux: make([]interface{}, len(opt.Aux)),
		},
		opt: opt,
	}, nil
}

// Stats returns stats about the points processed.
func (itr *seriesPointIterator) Stats() query.IteratorStats { return query.IteratorStats{} }

// Close closes the iterator.
func (itr *seriesPointIterator) Close() (err error) {
	itr.once.Do(func() {
		if itr.mitr != nil {
			if e := itr.mitr.Close(); e != nil && err == nil {
				err = e
			}
		}
		if itr.sitr != nil {
			if e := itr.sitr.Close(); e != nil && err == nil {
				err = e
			}
		}
	})
	return err
}

// Next emits the next point in the iterator.
func (itr *seriesPointIterator) Next() (*query.FloatPoint, error) {
	for {
		// Create new series iterator, if necessary.
		// Exit if there are no measurements remaining.
		if itr.sitr == nil {
			if itr.mitr == nil {
				return nil, nil
			}

			m, err := itr.mitr.Next()
			if err != nil {
				return nil, err
			} else if m == nil {
				return nil, nil
			}

			sitr, err := itr.indexSet.MeasurementSeriesByExprIterator(m, itr.opt.Condition)
			if err != nil {
				return nil, err
			} else if sitr == nil {
				continue
			}
			itr.sitr = sitr
		}

		// Read next series element.
		e, err := itr.sitr.Next()
		if err != nil {
			return nil, err
		} else if e.SeriesID == 0 {
			itr.sitr.Close()
			itr.sitr = nil
			continue
		}

		// Convert to a key.
		name, tags := ParseSeriesKey(itr.sfile.SeriesKey(e.SeriesID))
		key := string(models.MakeKey(name, tags))

		// Write auxiliary fields.
		for i, f := range itr.opt.Aux {
			switch f.Val {
			case "key":
				itr.point.Aux[i] = key
			}
		}
	}
}

// MeasurementIterator represents a iterator over a list of measurements.
type MeasurementIterator interface {
	Close() error
	Next() ([]byte, error)
}

type MeasurementIterators []MeasurementIterator

func (a MeasurementIterators) Close() (err error) {
	for i := range a {
		if e := a[i].Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

type measurementSliceIterator struct {
	names [][]byte
}

// NewMeasurementSliceIterator returns an iterator over a slice of in-memory measurement names.
func NewMeasurementSliceIterator(names [][]byte) *measurementSliceIterator {
	return &measurementSliceIterator{names: names}
}

func (itr *measurementSliceIterator) Close() (err error) { return nil }

func (itr *measurementSliceIterator) Next() (name []byte, err error) {
	if len(itr.names) == 0 {
		return nil, nil
	}
	name, itr.names = itr.names[0], itr.names[1:]
	return name, nil
}

// MergeMeasurementIterators returns an iterator that merges a set of iterators.
// Iterators that are first in the list take precendence and a deletion by those
// early iterators will invalidate elements by later iterators.
func MergeMeasurementIterators(itrs ...MeasurementIterator) MeasurementIterator {
	if len(itrs) == 0 {
		return nil
	} else if len(itrs) == 1 {
		return itrs[0]
	}

	return &measurementMergeIterator{
		buf:  make([][]byte, len(itrs)),
		itrs: itrs,
	}
}

type measurementMergeIterator struct {
	buf  [][]byte
	itrs []MeasurementIterator
}

func (itr *measurementMergeIterator) Close() (err error) {
	for i := range itr.itrs {
		if e := itr.itrs[i].Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// Next returns the element with the next lowest name across the iterators.
//
// If multiple iterators contain the same name then the first is returned
// and the remaining ones are skipped.
func (itr *measurementMergeIterator) Next() (_ []byte, err error) {
	// Find next lowest name amongst the buffers.
	var name []byte
	for i, buf := range itr.buf {
		// Fill buffer if empty.
		if buf == nil {
			if buf, err = itr.itrs[i].Next(); err != nil {
				return nil, err
			} else if buf != nil {
				itr.buf[i] = buf
			} else {
				continue
			}
		}

		// Find next lowest name.
		if name == nil || bytes.Compare(itr.buf[i], name) == -1 {
			name = itr.buf[i]
		}
	}

	// Return nil if no elements remaining.
	if name == nil {
		return nil, nil
	}

	// Merge all elements together and clear buffers.
	for i, buf := range itr.buf {
		if buf == nil || !bytes.Equal(buf, name) {
			continue
		}
		itr.buf[i] = nil
	}
	return name, nil
}

// TagValueIterator represents a iterator over a list of tag values.
type TagValueIterator interface {
	Close() error
	Next() ([]byte, error)
}

type TagValueIterators []TagValueIterator

func (a TagValueIterators) Close() (err error) {
	for i := range a {
		if e := a[i].Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// NewTagValueSliceIterator returns a TagValueIterator that iterates over a slice.
func NewTagValueSliceIterator(values [][]byte) *tagValueSliceIterator {
	return &tagValueSliceIterator{values: values}
}

// tagValueSliceIterator iterates over a slice of tag values.
type tagValueSliceIterator struct {
	values [][]byte
}

// Next returns the next tag value in the slice.
func (itr *tagValueSliceIterator) Next() ([]byte, error) {
	if len(itr.values) == 0 {
		return nil, nil
	}
	value := itr.values[0]
	itr.values = itr.values[1:]
	return value, nil
}

func (itr *tagValueSliceIterator) Close() error { return nil }

// MergeTagValueIterators returns an iterator that merges a set of iterators.
func MergeTagValueIterators(itrs ...TagValueIterator) TagValueIterator {
	if len(itrs) == 0 {
		return nil
	} else if len(itrs) == 1 {
		return itrs[0]
	}

	return &tagValueMergeIterator{
		buf:  make([][]byte, len(itrs)),
		itrs: itrs,
	}
}

type tagValueMergeIterator struct {
	buf  [][]byte
	itrs []TagValueIterator
}

func (itr *tagValueMergeIterator) Close() error {
	for i := range itr.itrs {
		itr.itrs[i].Close()
	}
	return nil
}

// Next returns the element with the next lowest value across the iterators.
//
// If multiple iterators contain the same value then the first is returned
// and the remaining ones are skipped.
func (itr *tagValueMergeIterator) Next() (_ []byte, err error) {
	// Find next lowest value amongst the buffers.
	var value []byte
	for i, buf := range itr.buf {
		// Fill buffer.
		if buf == nil {
			if buf, err = itr.itrs[i].Next(); err != nil {
				return nil, err
			} else if buf != nil {
				itr.buf[i] = buf
			} else {
				continue
			}
		}

		// Find next lowest value.
		if value == nil || bytes.Compare(buf, value) == -1 {
			value = buf
		}
	}

	// Return nil if no elements remaining.
	if value == nil {
		return nil, nil
	}

	// Merge elements and clear buffers.
	for i, buf := range itr.buf {
		if buf == nil || !bytes.Equal(buf, value) {
			continue
		}
		itr.buf[i] = nil
	}
	return value, nil
}

// IndexSet represents a list of indexes.
type IndexSet []Index

// Database returns the database name of the first index.
func (is IndexSet) Database() string {
	if len(is) == 0 {
		return ""
	}
	return is[0].Database()
}

// FieldSet returns the fieldset of the first index.
func (is IndexSet) FieldSet() *MeasurementFieldSet {
	if len(is) == 0 {
		return nil
	}
	return is[0].FieldSet()
}

// MeasurementIterator returns an iterator over all measurements in the index.
func (is IndexSet) MeasurementIterator() (MeasurementIterator, error) {
	a := make([]MeasurementIterator, 0, len(is))
	for _, idx := range is {
		itr, err := idx.MeasurementIterator()
		if err != nil {
			MeasurementIterators(a).Close()
			return nil, err
		} else if itr != nil {
			a = append(a, itr)
		}
	}
	return MergeMeasurementIterators(a...), nil
}

// TagValueIterator returns a value iterator for a tag key.
func (is IndexSet) TagValueIterator(auth query.Authorizer, name, key []byte) (TagValueIterator, error) {
	a := make([]TagValueIterator, 0, len(is))
	for _, idx := range is {
		itr, err := idx.TagValueIterator(auth, name, key)
		if err != nil {
			TagValueIterators(a).Close()
			return nil, err
		} else if itr != nil {
			a = append(a, itr)
		}
	}
	return MergeTagValueIterators(a...), nil
}

// MeasurementSeriesIDIterator returns an iterator over all non-tombstoned series
// for the provided measurement.
func (is IndexSet) MeasurementSeriesIDIterator(name []byte) (SeriesIDIterator, error) {
	a := make([]SeriesIDIterator, 0, len(is))
	for _, idx := range is {
		itr, err := idx.MeasurementSeriesIDIterator(name)
		if err != nil {
			SeriesIDIterators(a).Close()
			return nil, err
		} else if itr != nil {
			a = append(a, itr)
		}
	}
	return MergeSeriesIDIterators(a...), nil
}

// TagKeySeriesIDIterator returns a series iterator for all values across a single key.
func (is IndexSet) TagKeySeriesIDIterator(name, key []byte) (SeriesIDIterator, error) {
	a := make([]SeriesIDIterator, 0, len(is))
	for _, idx := range is {
		itr, err := idx.TagKeySeriesIDIterator(name, key)
		if err != nil {
			SeriesIDIterators(a).Close()
			return nil, err
		} else if itr != nil {
			a = append(a, itr)
		}
	}
	return MergeSeriesIDIterators(a...), nil
}

// TagValueSeriesIDIterator returns a series iterator for a single tag value.
func (is IndexSet) TagValueSeriesIDIterator(name, key, value []byte) (SeriesIDIterator, error) {
	a := make([]SeriesIDIterator, 0, len(is))
	for _, idx := range is {
		itr, err := idx.TagValueSeriesIDIterator(name, key, value)
		if err != nil {
			SeriesIDIterators(a).Close()
			return nil, err
		} else if itr != nil {
			a = append(a, itr)
		}
	}
	return MergeSeriesIDIterators(a...), nil
}

// MeasurementSeriesByExprIterator returns a series iterator for a measurement
// that is filtered by expr. If expr only contains time expressions then this
// call is equivalent to MeasurementSeriesIDIterator().
func (is IndexSet) MeasurementSeriesByExprIterator(name []byte, expr influxql.Expr) (SeriesIDIterator, error) {
	// Return all series for the measurement if there are no tag expressions.
	if expr == nil {
		return is.MeasurementSeriesIDIterator(name)
	}
	fieldset := is.FieldSet()
	return is.seriesByExprIterator(name, expr, fieldset.CreateFieldsIfNotExists(name))
}

// MeasurementSeriesKeysByExpr returns a list of series keys matching expr.
func (is IndexSet) MeasurementSeriesKeysByExpr(sfile *SeriesFile, name []byte, expr influxql.Expr) ([][]byte, error) {
	// Create iterator for all matching series.
	itr, err := is.MeasurementSeriesByExprIterator(name, expr)
	if err != nil {
		return nil, err
	} else if itr == nil {
		return nil, nil
	}
	defer itr.Close()

	// Iterate over all series and generate keys.
	var keys [][]byte
	for {
		e, err := itr.Next()
		if err != nil {
			return nil, err
		} else if e.SeriesID == 0 {
			break
		}

		// Check for unsupported field filters.
		// Any remaining filters means there were fields (e.g., `WHERE value = 1.2`).
		if e.Expr != nil {
			if v, ok := e.Expr.(*influxql.BooleanLiteral); !ok || !v.Val {
				return nil, errors.New("fields not supported in WHERE clause during deletion")
			}
		}

		seriesKey := sfile.SeriesKey(e.SeriesID)
		assert(seriesKey != nil, "series key not found")

		name, tags := ParseSeriesKey(seriesKey)
		keys = append(keys, models.MakeKey(name, tags))
	}

	bytesutil.Sort(keys)

	return keys, nil
}

func (is IndexSet) seriesByExprIterator(name []byte, expr influxql.Expr, mf *MeasurementFields) (SeriesIDIterator, error) {
	switch expr := expr.(type) {
	case *influxql.BinaryExpr:
		switch expr.Op {
		case influxql.AND, influxql.OR:
			// Get the series IDs and filter expressions for the LHS.
			litr, err := is.seriesByExprIterator(name, expr.LHS, mf)
			if err != nil {
				return nil, err
			}

			// Get the series IDs and filter expressions for the RHS.
			ritr, err := is.seriesByExprIterator(name, expr.RHS, mf)
			if err != nil {
				if litr != nil {
					litr.Close()
				}
				return nil, err
			}

			// Intersect iterators if expression is "AND".
			if expr.Op == influxql.AND {
				return IntersectSeriesIDIterators(litr, ritr), nil
			}

			// Union iterators if expression is "OR".
			return UnionSeriesIDIterators(litr, ritr), nil

		default:
			return is.seriesByBinaryExprIterator(name, expr, mf)
		}

	case *influxql.ParenExpr:
		return is.seriesByExprIterator(name, expr.Expr, mf)

	default:
		return nil, nil
	}
}

// seriesByBinaryExprIterator returns a series iterator and a filtering expression.
func (is IndexSet) seriesByBinaryExprIterator(name []byte, n *influxql.BinaryExpr, mf *MeasurementFields) (SeriesIDIterator, error) {
	// If this binary expression has another binary expression, then this
	// is some expression math and we should just pass it to the underlying query.
	if _, ok := n.LHS.(*influxql.BinaryExpr); ok {
		itr, err := is.MeasurementSeriesIDIterator(name)
		if err != nil {
			return nil, err
		}
		return newSeriesIDExprIterator(itr, n), nil
	} else if _, ok := n.RHS.(*influxql.BinaryExpr); ok {
		itr, err := is.MeasurementSeriesIDIterator(name)
		if err != nil {
			return nil, err
		}
		return newSeriesIDExprIterator(itr, n), nil
	}

	// Retrieve the variable reference from the correct side of the expression.
	key, ok := n.LHS.(*influxql.VarRef)
	value := n.RHS
	if !ok {
		key, ok = n.RHS.(*influxql.VarRef)
		if !ok {
			return nil, fmt.Errorf("invalid expression: %s", n.String())
		}
		value = n.LHS
	}

	// For fields, return all series from this measurement.
	if key.Val != "_name" && ((key.Type == influxql.Unknown && mf.HasField(key.Val)) || key.Type == influxql.AnyField || (key.Type != influxql.Tag && key.Type != influxql.Unknown)) {
		itr, err := is.MeasurementSeriesIDIterator(name)
		if err != nil {
			return nil, err
		}
		return newSeriesIDExprIterator(itr, n), nil
	} else if value, ok := value.(*influxql.VarRef); ok {
		// Check if the RHS is a variable and if it is a field.
		if value.Val != "_name" && ((value.Type == influxql.Unknown && mf.HasField(value.Val)) || key.Type == influxql.AnyField || (value.Type != influxql.Tag && value.Type != influxql.Unknown)) {
			itr, err := is.MeasurementSeriesIDIterator(name)
			if err != nil {
				return nil, err
			}
			return newSeriesIDExprIterator(itr, n), nil
		}
	}

	// Create iterator based on value type.
	switch value := value.(type) {
	case *influxql.StringLiteral:
		return is.seriesByBinaryExprStringIterator(name, []byte(key.Val), []byte(value.Val), n.Op)
	case *influxql.RegexLiteral:
		return is.seriesByBinaryExprRegexIterator(name, []byte(key.Val), value.Val, n.Op)
	case *influxql.VarRef:
		return is.seriesByBinaryExprVarRefIterator(name, []byte(key.Val), value, n.Op)
	default:
		if n.Op == influxql.NEQ || n.Op == influxql.NEQREGEX {
			return is.MeasurementSeriesIDIterator(name)
		}
		return nil, nil
	}
}

func (is IndexSet) seriesByBinaryExprStringIterator(name, key, value []byte, op influxql.Token) (SeriesIDIterator, error) {
	// Special handling for "_name" to match measurement name.
	if bytes.Equal(key, []byte("_name")) {
		if (op == influxql.EQ && bytes.Equal(value, name)) || (op == influxql.NEQ && !bytes.Equal(value, name)) {
			return is.MeasurementSeriesIDIterator(name)
		}
		return nil, nil
	}

	if op == influxql.EQ {
		// Match a specific value.
		if len(value) != 0 {
			return is.TagValueSeriesIDIterator(name, key, value)
		}

		mitr, err := is.MeasurementSeriesIDIterator(name)
		if err != nil {
			return nil, err
		}

		kitr, err := is.TagKeySeriesIDIterator(name, key)
		if err != nil {
			mitr.Close()
			return nil, err
		}

		// Return all measurement series that have no values from this tag key.
		return DifferenceSeriesIDIterators(mitr, kitr), nil
	}

	// Return all measurement series without this tag value.
	if len(value) != 0 {
		mitr, err := is.MeasurementSeriesIDIterator(name)
		if err != nil {
			return nil, err
		}

		vitr, err := is.TagValueSeriesIDIterator(name, key, value)
		if err != nil {
			mitr.Close()
			return nil, err
		}

		return DifferenceSeriesIDIterators(mitr, vitr), nil
	}

	// Return all series across all values of this tag key.
	return is.TagKeySeriesIDIterator(name, key)
}

func (is IndexSet) seriesByBinaryExprRegexIterator(name, key []byte, value *regexp.Regexp, op influxql.Token) (SeriesIDIterator, error) {
	// Special handling for "_name" to match measurement name.
	if bytes.Equal(key, []byte("_name")) {
		match := value.Match(name)
		if (op == influxql.EQREGEX && match) || (op == influxql.NEQREGEX && !match) {
			mitr, err := is.MeasurementSeriesIDIterator(name)
			if err != nil {
				return nil, err
			}
			return newSeriesIDExprIterator(mitr, &influxql.BooleanLiteral{Val: true}), nil
		}
		return nil, nil
	}
	return is.MatchTagValueSeriesIDIterator(name, key, value, op == influxql.EQREGEX)
}

func (is IndexSet) seriesByBinaryExprVarRefIterator(name, key []byte, value *influxql.VarRef, op influxql.Token) (SeriesIDIterator, error) {
	itr0, err := is.TagKeySeriesIDIterator(name, key)
	if err != nil {
		return nil, err
	}

	itr1, err := is.TagKeySeriesIDIterator(name, []byte(value.Val))
	if err != nil {
		itr0.Close()
		return nil, err
	}

	if op == influxql.EQ {
		return IntersectSeriesIDIterators(itr0, itr1), nil
	}
	return DifferenceSeriesIDIterators(itr0, itr1), nil
}

// MatchTagValueSeriesIDIterator returns a series iterator for tags which match value.
// If matches is false, returns iterators which do not match value.
func (is IndexSet) MatchTagValueSeriesIDIterator(name, key []byte, value *regexp.Regexp, matches bool) (SeriesIDIterator, error) {
	matchEmpty := value.MatchString("")

	if matches {
		if matchEmpty {
			return is.matchTagValueEqualEmptySeriesIDIterator(name, key, value)
		}
		return is.matchTagValueEqualNotEmptySeriesIDIterator(name, key, value)
	}

	if matchEmpty {
		return is.matchTagValueNotEqualEmptySeriesIDIterator(name, key, value)
	}
	return is.matchTagValueNotEqualNotEmptySeriesIDIterator(name, key, value)
}

func (is IndexSet) matchTagValueEqualEmptySeriesIDIterator(name, key []byte, value *regexp.Regexp) (SeriesIDIterator, error) {
	vitr, err := is.TagValueIterator(nil, name, key)
	if err != nil {
		return nil, err
	} else if vitr == nil {
		return is.MeasurementSeriesIDIterator(name)
	}
	defer vitr.Close()

	var itrs []SeriesIDIterator
	if err := func() error {
		for {
			e, err := vitr.Next()
			if err != nil {
				return err
			} else if e != nil {
				break
			}

			if !value.Match(e) {
				itr, err := is.TagValueSeriesIDIterator(name, key, e)
				if err != nil {
					return err
				}
				itrs = append(itrs, itr)
			}
		}
		return nil
	}(); err != nil {
		SeriesIDIterators(itrs).Close()
		return nil, err
	}

	mitr, err := is.MeasurementSeriesIDIterator(name)
	if err != nil {
		SeriesIDIterators(itrs).Close()
		return nil, err
	}

	return DifferenceSeriesIDIterators(mitr, MergeSeriesIDIterators(itrs...)), nil
}

func (is IndexSet) matchTagValueEqualNotEmptySeriesIDIterator(name, key []byte, value *regexp.Regexp) (SeriesIDIterator, error) {
	vitr, err := is.TagValueIterator(nil, name, key)
	if err != nil {
		return nil, err
	} else if vitr == nil {
		return nil, nil
	}
	defer vitr.Close()

	var itrs []SeriesIDIterator
	for {
		e, err := vitr.Next()
		if err != nil {
			SeriesIDIterators(itrs).Close()
			return nil, err
		} else if e != nil {
			break
		}

		if value.Match(e) {
			itr, err := is.TagValueSeriesIDIterator(name, key, e)
			if err != nil {
				SeriesIDIterators(itrs).Close()
				return nil, err
			}
			itrs = append(itrs, itr)
		}
	}
	return MergeSeriesIDIterators(itrs...), nil
}

func (is IndexSet) matchTagValueNotEqualEmptySeriesIDIterator(name, key []byte, value *regexp.Regexp) (SeriesIDIterator, error) {
	vitr, err := is.TagValueIterator(nil, name, key)
	if err != nil {
		return nil, err
	} else if vitr == nil {
		return nil, nil
	}
	defer vitr.Close()

	var itrs []SeriesIDIterator
	for {
		e, err := vitr.Next()
		if err != nil {
			SeriesIDIterators(itrs).Close()
			return nil, err
		} else if e != nil {
			break
		}

		if !value.Match(e) {
			itr, err := is.TagValueSeriesIDIterator(name, key, e)
			if err != nil {
				SeriesIDIterators(itrs).Close()
				return nil, err
			}
			itrs = append(itrs, itr)
		}
	}
	return MergeSeriesIDIterators(itrs...), nil
}

func (is IndexSet) matchTagValueNotEqualNotEmptySeriesIDIterator(name, key []byte, value *regexp.Regexp) (SeriesIDIterator, error) {
	vitr, err := is.TagValueIterator(nil, name, key)
	if err != nil {
		return nil, err
	} else if vitr == nil {
		return is.MeasurementSeriesIDIterator(name)
	}
	defer vitr.Close()

	var itrs []SeriesIDIterator
	for {
		e, err := vitr.Next()
		if err != nil {
			SeriesIDIterators(itrs).Close()
			return nil, err
		} else if e != nil {
			break
		}
		if value.Match(e) {
			itr, err := is.TagValueSeriesIDIterator(name, key, e)
			if err != nil {
				SeriesIDIterators(itrs).Close()
				return nil, err
			}
			itrs = append(itrs, itr)
		}
	}

	mitr, err := is.MeasurementSeriesIDIterator(name)
	if err != nil {
		SeriesIDIterators(itrs).Close()
		return nil, err
	}
	return DifferenceSeriesIDIterators(mitr, MergeSeriesIDIterators(itrs...)), nil
}

// TagValuesByKeyAndExpr retrieves tag values for the provided tag keys.
//
// TagValuesByKeyAndExpr returns sets of values for each key, indexable by the
// position of the tag key in the keys argument.
//
// N.B tagValuesByKeyAndExpr relies on keys being sorted in ascending
// lexicographic order.
func (is IndexSet) TagValuesByKeyAndExpr(auth query.Authorizer, sfile *SeriesFile, name []byte, keys []string, expr influxql.Expr, fieldset *MeasurementFieldSet, resultSet []map[string]struct{}) error {
	database := is.Database()

	itr, err := is.seriesByExprIterator(name, expr, fieldset.Fields(string(name)))
	if err != nil {
		return err
	} else if itr == nil {
		return nil
	}
	defer itr.Close()

	keyIdxs := make(map[string]int, len(keys))
	for ki, key := range keys {
		keyIdxs[key] = ki

		// Check that keys are in order.
		if ki > 0 && key < keys[ki-1] {
			return fmt.Errorf("keys %v are not in ascending order", keys)
		}
	}

	// Iterate all series to collect tag values.
	for {
		e, err := itr.Next()
		if err != nil {
			return err
		} else if e.SeriesID == 0 {
			break
		}

		buf := sfile.SeriesKey(e.SeriesID)
		if buf == nil {
			continue
		}

		if auth != nil {
			name, tags := ParseSeriesKey(buf)
			if !auth.AuthorizeSeriesRead(database, name, tags) {
				continue
			}
		}

		_, buf = ReadSeriesKeyLen(buf)
		_, buf = ReadSeriesKeyMeasurement(buf)
		tagN, buf := ReadSeriesKeyTagN(buf)
		for i := 0; i < tagN; i++ {
			var key, value []byte
			key, value, buf = ReadSeriesKeyTag(buf)

			if idx, ok := keyIdxs[string(key)]; ok {
				resultSet[idx][string(value)] = struct{}{}
			} else if string(key) > keys[len(keys)-1] {
				// The tag key is > the largest key we're interested in.
				break
			}
		}
	}
	return nil
}

// MeasurementTagKeyValuesByExpr returns a set of tag values filtered by an expression.
func (is IndexSet) MeasurementTagKeyValuesByExpr(auth query.Authorizer, sfile *SeriesFile, name []byte, keys []string, expr influxql.Expr, keysSorted bool) ([][]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	// If we haven't been provided sorted keys, then we need to sort them.
	if !keysSorted {
		sort.Sort(sort.StringSlice(keys))
	}

	resultSet := make([]map[string]struct{}, len(keys))
	for i := 0; i < len(resultSet); i++ {
		resultSet[i] = make(map[string]struct{})
	}

	// No expression means that the values shouldn't be filtered, so we can
	// fetch them all.
	database := is.Database()
	if expr == nil {
		for ki, key := range keys {
			if err := func() error {
				vitr, err := is.TagValueIterator(auth, name, []byte(key))
				if err != nil {
					return err
				} else if vitr == nil {
					return nil
				}
				defer vitr.Close()

				// If no authorizer present then return all values.
				if auth == nil {
					for {
						val, err := vitr.Next()
						if err != nil {
							return err
						} else if val == nil {
							break
						}

						resultSet[ki][string(val)] = struct{}{}
					}
					return nil
				}

				// If authorizer exists then check each value for a readable series.
				for {
					val, err := vitr.Next()
					if err != nil {
						return err
					} else if val == nil {
						break
					}

					if err := func() error {
						sitr, err := is.TagValueSeriesIDIterator(name, []byte(key), val)
						if err != nil {
							return err
						} else if sitr == nil {
							return nil
						}
						defer sitr.Close()

						for {
							se, err := sitr.Next()
							if err != nil {
								return err
							} else if se.SeriesID == 0 {
								break
							}

							name, tags := ParseSeriesKey(sfile.SeriesKey(se.SeriesID))
							if auth.AuthorizeSeriesRead(database, name, tags) {
								resultSet[ki][string(val)] = struct{}{}
								break
							}
						}
						return nil
					}(); err != nil {
						return err
					}
				}
				return nil
			}(); err != nil {
				return nil, err
			}
		}
	} else {
		// This is the case where we have filtered series by some WHERE condition.
		// We only care about the tag values for the keys given the
		// filtered set of series ids.
		if err := is.TagValuesByKeyAndExpr(auth, sfile, name, keys, expr, is.FieldSet(), resultSet); err != nil {
			return nil, err
		}
	}

	// Convert result sets into []string
	results := make([][]string, len(keys))
	for i, s := range resultSet {
		values := make([]string, 0, len(s))
		for v := range s {
			values = append(values, v)
		}
		sort.Sort(sort.StringSlice(values))
		results[i] = values
	}
	return results, nil
}

// TagSets returns an ordered list of tag sets for a measurement by dimension
// and filtered by an optional conditional expression.
func (is IndexSet) TagSets(sfile *SeriesFile, name []byte, opt query.IteratorOptions) ([]*query.TagSet, error) {
	itr, err := is.MeasurementSeriesByExprIterator(name, opt.Condition)
	if err != nil {
		return nil, err
	} else if itr != nil {
		defer itr.Close()
	}

	// For every series, get the tag values for the requested tag keys i.e.
	// dimensions. This is the TagSet for that series. Series with the same
	// TagSet are then grouped together, because for the purpose of GROUP BY
	// they are part of the same composite series.
	tagSets := make(map[string]*query.TagSet, 64)

	if itr != nil {
		for {
			e, err := itr.Next()
			if err != nil {
				return nil, err
			} else if e.SeriesID == 0 {
				break
			}

			_, tags := ParseSeriesKey(sfile.SeriesKey(e.SeriesID))
			if opt.Authorizer != nil && !opt.Authorizer.AuthorizeSeriesRead(is.Database(), name, tags) {
				continue
			}

			tagsMap := make(map[string]string, len(opt.Dimensions))

			// Build the TagSet for this series.
			for _, dim := range opt.Dimensions {
				tagsMap[dim] = tags.GetString(dim)
			}

			// Convert the TagSet to a string, so it can be added to a map
			// allowing TagSets to be handled as a set.
			tagsAsKey := MarshalTags(tagsMap)
			tagSet, ok := tagSets[string(tagsAsKey)]
			if !ok {
				// This TagSet is new, create a new entry for it.
				tagSet = &query.TagSet{
					Tags: tagsMap,
					Key:  tagsAsKey,
				}
			}

			// Associate the series and filter with the Tagset.
			tagSet.AddFilter(string(models.MakeKey(name, tags)), e.Expr)

			// Ensure it's back in the map.
			tagSets[string(tagsAsKey)] = tagSet
		}
	}

	// Sort the series in each tag set.
	for _, t := range tagSets {
		sort.Sort(t)
	}

	// The TagSets have been created, as a map of TagSets. Just send
	// the values back as a slice, sorting for consistency.
	sortedTagsSets := make([]*query.TagSet, 0, len(tagSets))
	for _, v := range tagSets {
		sortedTagsSets = append(sortedTagsSets, v)
	}
	sort.Sort(byTagKey(sortedTagsSets))

	return sortedTagsSets, nil
}

// IndexFormat represents the format for an index.
type IndexFormat int

const (
	// InMemFormat is the format used by the original in-memory shared index.
	InMemFormat IndexFormat = 1

	// TSI1Format is the format used by the tsi1 index.
	TSI1Format IndexFormat = 2
)

// NewIndexFunc creates a new index.
type NewIndexFunc func(id uint64, database, path string, sfile *SeriesFile, options EngineOptions) Index

// newIndexFuncs is a lookup of index constructors by name.
var newIndexFuncs = make(map[string]NewIndexFunc)

// RegisterIndex registers a storage index initializer by name.
func RegisterIndex(name string, fn NewIndexFunc) {
	if _, ok := newIndexFuncs[name]; ok {
		panic("index already registered: " + name)
	}
	newIndexFuncs[name] = fn
}

// RegisteredIndexs returns the slice of currently registered indexes.
func RegisteredIndexes() []string {
	a := make([]string, 0, len(newIndexFuncs))
	for k := range newIndexFuncs {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}

// NewIndex returns an instance of an index based on its format.
// If the path does not exist then the DefaultFormat is used.
func NewIndex(id uint64, database, path string, sfile *SeriesFile, options EngineOptions) (Index, error) {
	format := options.IndexVersion

	// Use default format unless existing directory exists.
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		// nop, use default
	} else if err != nil {
		return nil, err
	} else if err == nil {
		format = "tsi1"
	}

	// Lookup index by format.
	fn := newIndexFuncs[format]
	if fn == nil {
		return nil, fmt.Errorf("invalid index format: %q", format)
	}
	return fn(id, database, path, sfile, options), nil
}

func MustOpenIndex(id uint64, database, path string, sfile *SeriesFile, options EngineOptions) Index {
	idx, err := NewIndex(id, database, path, sfile, options)
	if err != nil {
		panic(err)
	} else if err := idx.Open(); err != nil {
		panic(err)
	}
	return idx
}

// assert will panic with a given formatted message if the given condition is false.
func assert(condition bool, msg string, v ...interface{}) {
	if !condition {
		panic(fmt.Sprintf("assert failed: "+msg, v...))
	}
}

type byTagKey []*query.TagSet

func (t byTagKey) Len() int           { return len(t) }
func (t byTagKey) Less(i, j int) bool { return bytes.Compare(t[i].Key, t[j].Key) < 0 }
func (t byTagKey) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }
