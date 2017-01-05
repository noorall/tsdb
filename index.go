package tsdb

import (
	"fmt"
	"os"
	"regexp"
	"sort"

	"github.com/influxdata/influxdb/influxql"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/pkg/estimator"
)

type Index interface {
	Open() error
	Close() error

	Measurement(name []byte) (*Measurement, error)
	Measurements() (Measurements, error)
	MeasurementsByExpr(expr influxql.Expr) (Measurements, bool, error)
	MeasurementsByName(names [][]byte) ([]*Measurement, error)
	MeasurementNamesByRegex(re *regexp.Regexp) ([][]byte, error)
	DropMeasurement(name []byte) error

	CreateSeriesIfNotExists(name []byte, tags models.Tags) error
	DropSeries(keys [][]byte) error

	SeriesN() (uint64, error)
	SeriesSketches() (estimator.Sketch, estimator.Sketch, error)
	MeasurementsSketches() (estimator.Sketch, estimator.Sketch, error)

	Dereference(b []byte)

	TagSets(name []byte, dimensions []string, condition influxql.Expr) ([]*influxql.TagSet, error)
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
type NewIndexFunc func(id uint64, path string, options IndexOptions) Index

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
func NewIndex(id uint64, path string, options IndexOptions) (Index, error) {
	// Create a new index.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return newIndexFuncs[options.IndexVersion](id, path, options), nil
	}

	// Use default format.
	format := DefaultIndex

	// Lookup index by format.
	fn := newIndexFuncs[format]
	if fn == nil {
		return nil, fmt.Errorf("invalid index format: %q", format)
	}

	return fn(id, path, options), nil
}

// IndexOptions represents the options used to initialize the index.
type IndexOptions struct {
	IndexVersion string
	ShardID      uint64
	InmemIndex   interface{} // shared in-memory index

	Config Config
}

// NewIndexOptions returns the default options.
func NewIndexOptions() IndexOptions {
	return IndexOptions{
		IndexVersion: DefaultIndex,
		Config:       NewConfig(),
	}
}

// NewInmemIndex returns a new "inmem" index type.
var NewInmemIndex func(name string) (interface{}, error)
