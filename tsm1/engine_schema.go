package tsm1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/influxdata/influxdb"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/tsdb"
	"github.com/influxdata/influxdb/tsdb/cursors"
	"github.com/influxdata/influxql"
)

// TagValues returns an iterator which enumerates the values for the specific
// tagKey in the given bucket matching the predicate within the
// time range (start, end].
func (e *Engine) TagValues(ctx context.Context, orgID, bucketID influxdb.ID, tagKey string, start, end int64, predicate influxql.Expr) (cursors.StringIterator, error) {
	encoded := tsdb.EncodeName(orgID, bucketID)

	if predicate == nil {
		return e.tagValuesNoPredicate(ctx, encoded[:], []byte(tagKey), start, end)
	}

	return e.tagValuesPredicate(ctx, encoded[:], []byte(tagKey), start, end, predicate)
}

func (e *Engine) tagValuesNoPredicate(ctx context.Context, orgBucket, tagKeyBytes []byte, start, end int64) (cursors.StringIterator, error) {
	tsmValues := make(map[string]struct{})
	var tags models.Tags

	// TODO(edd): we need to clean up how we're encoding the prefix so that we
	// don't have to remember to get it right everywhere we need to touch TSM data.
	prefix := models.EscapeMeasurement(orgBucket)

	// TODO(sgc): extend prefix when filtering by \x00 == <measurement>

	e.FileStore.ForEachFile(func(f TSMFile) bool {
		if f.OverlapsTimeRange(start, end) && f.OverlapsKeyPrefixRange(prefix, prefix) {
			// TODO(sgc): create f.TimeRangeIterator(minKey, maxKey, start, end)
			iter := f.TimeRangeIterator(prefix, start, end)
			for i := 0; iter.Next(); i++ {
				sfkey := iter.Key()
				if !bytes.HasPrefix(sfkey, prefix) {
					// end of org+bucket
					break
				}

				key, _ := SeriesAndFieldFromCompositeKey(sfkey)
				tags = models.ParseTagsWithTags(key, tags[:0])
				curVal := tags.Get(tagKeyBytes)
				if len(curVal) == 0 {
					continue
				}

				if _, ok := tsmValues[string(curVal)]; ok {
					continue
				}

				if iter.HasData() {
					tsmValues[string(curVal)] = struct{}{}
				}
			}
		}
		return true
	})

	_ = e.Cache.ApplyEntryFn(func(sfkey []byte, entry *entry) error {
		if !bytes.HasPrefix(sfkey, prefix) {
			return nil
		}

		key, _ := SeriesAndFieldFromCompositeKey(sfkey)
		tags = models.ParseTagsWithTags(key, tags[:0])
		curVal := tags.Get(tagKeyBytes)
		if len(curVal) == 0 {
			return nil
		}

		if _, ok := tsmValues[string(curVal)]; ok {
			return nil
		}

		if entry.values.Contains(start, end) {
			tsmValues[string(curVal)] = struct{}{}
		}
		return nil
	})

	vals := make([]string, 0, len(tsmValues))
	for val := range tsmValues {
		vals = append(vals, val)
	}
	sort.Strings(vals)

	return cursors.NewStringSliceIterator(vals), nil
}

func (e *Engine) tagValuesPredicate(ctx context.Context, orgBucket, tagKeyBytes []byte, start, end int64, predicate influxql.Expr) (cursors.StringIterator, error) {
	if err := ValidateTagPredicate(predicate); err != nil {
		return nil, err
	}

	keys, err := e.findCandidateKeys(ctx, orgBucket, predicate)
	if err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return nil, nil
	}

	var files []TSMFile
	defer func() {
		for _, f := range files {
			f.Unref()
		}
	}()
	var iters []*TimeRangeIterator

	// TODO(edd): we need to clean up how we're encoding the prefix so that we
	// don't have to remember to get it right everywhere we need to touch TSM data.
	prefix := models.EscapeMeasurement(orgBucket)

	e.FileStore.ForEachFile(func(f TSMFile) bool {
		if f.OverlapsTimeRange(start, end) && f.OverlapsKeyPrefixRange(prefix, prefix) {
			f.Ref()
			files = append(files, f)
			iters = append(iters, f.TimeRangeIterator(prefix, start, end))
		}
		return true
	})

	tsmValues := make(map[string]struct{})

	// reusable buffers
	var (
		tags   models.Tags
		keybuf []byte
		sfkey  []byte
	)

	for i := range keys {
		_, tags = tsdb.ParseSeriesKeyInto(keys[i], tags[:0])
		curVal := tags.Get(tagKeyBytes)
		if len(curVal) == 0 {
			continue
		}

		if _, ok := tsmValues[string(curVal)]; ok {
			continue
		}

		keybuf = models.AppendMakeKey(keybuf[:0], prefix, tags)
		sfkey = AppendSeriesFieldKeyBytes(sfkey[:0], keybuf, tags.Get(models.FieldKeyTagKeyBytes))

		if e.Cache.Values(sfkey).Contains(start, end) {
			tsmValues[string(curVal)] = struct{}{}
			continue
		}

		for _, iter := range iters {
			if exact, _ := iter.Seek(sfkey); !exact {
				continue
			}

			if iter.HasData() {
				tsmValues[string(curVal)] = struct{}{}
				break
			}
		}
	}

	vals := make([]string, 0, len(tsmValues))
	for val := range tsmValues {
		vals = append(vals, val)
	}
	sort.Strings(vals)

	return cursors.NewStringSliceIterator(vals), nil
}

func (e *Engine) findCandidateKeys(ctx context.Context, orgBucket []byte, predicate influxql.Expr) ([][]byte, error) {
	// determine candidate series keys
	sitr, err := e.index.MeasurementSeriesByExprIterator(orgBucket, predicate)
	if err != nil {
		return nil, err
	} else if sitr == nil {
		return nil, nil
	}
	defer sitr.Close()

	var keys [][]byte
	for {
		elem, err := sitr.Next()
		if err != nil {
			return nil, err
		} else if elem.SeriesID.IsZero() {
			break
		}

		key := e.sfile.SeriesKey(elem.SeriesID)
		if len(key) == 0 {
			continue
		}
		keys = append(keys, key)
	}

	return keys, nil
}

func (e *Engine) TagKeys(ctx context.Context, orgID, bucketID influxdb.ID, start, end int64, predicate influxql.Expr) (cursors.StringIterator, error) {
	encoded := tsdb.EncodeName(orgID, bucketID)

	if predicate == nil {
		return e.tagKeysNoPredicate(ctx, encoded[:], start, end)
	}

	return e.tagKeysPredicate(ctx, encoded[:], start, end, predicate)
}

func (e *Engine) tagKeysNoPredicate(ctx context.Context, orgBucket []byte, start, end int64) (cursors.StringIterator, error) {
	var tags models.Tags

	// TODO(edd): we need to clean up how we're encoding the prefix so that we
	// don't have to remember to get it right everywhere we need to touch TSM data.
	prefix := models.EscapeMeasurement(orgBucket)

	var keyset models.TagKeysSet

	// TODO(sgc): extend prefix when filtering by \x00 == <measurement>

	e.FileStore.ForEachFile(func(f TSMFile) bool {
		if f.OverlapsTimeRange(start, end) && f.OverlapsKeyPrefixRange(prefix, prefix) {
			// TODO(sgc): create f.TimeRangeIterator(minKey, maxKey, start, end)
			iter := f.TimeRangeIterator(prefix, start, end)
			for i := 0; iter.Next(); i++ {
				sfkey := iter.Key()
				if !bytes.HasPrefix(sfkey, prefix) {
					// end of org+bucket
					break
				}

				key, _ := SeriesAndFieldFromCompositeKey(sfkey)
				tags = models.ParseTagsWithTags(key, tags[:0])
				if keyset.IsSupersetKeys(tags) {
					continue
				}

				if iter.HasData() {
					keyset.UnionKeys(tags)
				}
			}
		}
		return true
	})

	_ = e.Cache.ApplyEntryFn(func(sfkey []byte, entry *entry) error {
		if !bytes.HasPrefix(sfkey, prefix) {
			return nil
		}

		key, _ := SeriesAndFieldFromCompositeKey(sfkey)
		tags = models.ParseTagsWithTags(key, tags[:0])
		if keyset.IsSupersetKeys(tags) {
			return nil
		}

		if entry.values.Contains(start, end) {
			keyset.UnionKeys(tags)
		}
		return nil
	})

	return cursors.NewStringSliceIterator(keyset.Keys()), nil
}

func (e *Engine) tagKeysPredicate(ctx context.Context, orgBucket []byte, start, end int64, predicate influxql.Expr) (cursors.StringIterator, error) {
	if err := ValidateTagPredicate(predicate); err != nil {
		return nil, err
	}

	keys, err := e.findCandidateKeys(ctx, orgBucket, predicate)
	if err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return nil, nil
	}

	var files []TSMFile
	defer func() {
		for _, f := range files {
			f.Unref()
		}
	}()
	var iters []*TimeRangeIterator

	// TODO(edd): we need to clean up how we're encoding the prefix so that we
	// don't have to remember to get it right everywhere we need to touch TSM data.
	prefix := models.EscapeMeasurement(orgBucket)

	e.FileStore.ForEachFile(func(f TSMFile) bool {
		if f.OverlapsTimeRange(start, end) && f.OverlapsKeyPrefixRange(prefix, prefix) {
			f.Ref()
			files = append(files, f)
			iters = append(iters, f.TimeRangeIterator(prefix, start, end))
		}
		return true
	})

	var keyset models.TagKeysSet

	// reusable buffers
	var (
		tags   models.Tags
		keybuf []byte
		sfkey  []byte
	)

	for i := range keys {
		_, tags = tsdb.ParseSeriesKeyInto(keys[i], tags[:0])
		if keyset.IsSupersetKeys(tags) {
			continue
		}

		keybuf = models.AppendMakeKey(keybuf[:0], prefix, tags)
		sfkey = AppendSeriesFieldKeyBytes(sfkey[:0], keybuf, tags.Get(models.FieldKeyTagKeyBytes))

		if e.Cache.Values(sfkey).Contains(start, end) {
			keyset.UnionKeys(tags)
			continue
		}

		for _, iter := range iters {
			if exact, _ := iter.Seek(sfkey); !exact {
				continue
			}

			if iter.HasData() {
				keyset.UnionKeys(tags)
				break
			}
		}
	}

	return cursors.NewStringSliceIterator(keyset.Keys()), nil
}

var errUnexpectedTagComparisonOperator = errors.New("unexpected tag comparison operator")

func ValidateTagPredicate(expr influxql.Expr) (err error) {
	influxql.WalkFunc(expr, func(node influxql.Node) {
		if err != nil {
			return
		}

		switch n := node.(type) {
		case *influxql.BinaryExpr:
			switch n.Op {
			case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX, influxql.OR, influxql.AND:
			default:
				err = errUnexpectedTagComparisonOperator
			}

			switch r := n.LHS.(type) {
			case *influxql.VarRef:
			case *influxql.BinaryExpr:
			default:
				err = fmt.Errorf("binary expression: LHS must be tag key reference, got: %T", r)
			}

			switch r := n.RHS.(type) {
			case *influxql.StringLiteral:
			case *influxql.RegexLiteral:
			case *influxql.BinaryExpr:
			default:
				err = fmt.Errorf("binary expression: RHS must be string or regex, got: %T", r)
			}
		}
	})
	return err
}
