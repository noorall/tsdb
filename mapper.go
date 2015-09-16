package tsdb

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/influxdb/influxdb/influxql"
)

// Mapper is the interface all Mapper types must implement.
type Mapper interface {
	Open() error
	TagSets() []string
	Fields() []string
	NextChunk() (interface{}, error)
	Close()
}

// StatefulMapper encapsulates a Mapper and some state that the executor needs to
// track for that mapper.
type StatefulMapper struct {
	Mapper
	bufferedChunk *MapperOutput // Last read chunk.
	drained       bool
}

// NextChunk wraps a RawMapper and some state.
func (sm *StatefulMapper) NextChunk() (*MapperOutput, error) {
	c, err := sm.Mapper.NextChunk()
	if err != nil {
		return nil, err
	}
	chunk, ok := c.(*MapperOutput)
	if !ok {
		if chunk == interface{}(nil) {
			return nil, nil
		}
	}
	return chunk, nil
}

// MapperValue is a complex type, which can encapsulate data from both raw and aggregate
// mappers. This currently allows marshalling and network system to remain simpler. For
// aggregate output Time is ignored, and actual Time-Value pairs are contained soley
// within the Value field.
type MapperValue struct {
	Time  int64             `json:"time,omitempty"`  // Ignored for aggregate output.
	Value interface{}       `json:"value,omitempty"` // For aggregate, contains interval time multiple values.
	Tags  map[string]string `json:"tags,omitempty"`  // Meta tags for results
}

// MapperValueJSON is the JSON-encoded representation of MapperValue. Because MapperValue is
// a complex type, custom JSON encoding is required so that none of the types contained within
// a MapperValue are "lost", and so the data are encoded as byte slices where necessary.
type MapperValueJSON struct {
	Time    int64             `json:"time,omitempty"`
	RawData []byte            `json:"rdata,omitempty"`
	AggData [][]byte          `json:"adata,omitempty"`
	Tags    map[string]string `json:"tags,omitempty"`
}

// MarshalJSON returns the JSON-encoded representation of a MapperValue.
func (mv *MapperValue) MarshalJSON() ([]byte, error) {
	o := &MapperValueJSON{
		Time:    mv.Time,
		AggData: make([][]byte, 0),
		Tags:    mv.Tags,
	}

	o.Time = mv.Time
	o.Tags = mv.Tags
	if values, ok := mv.Value.([]interface{}); ok {
		// Value contain a slice of more values. This happens only with
		// aggregate output.
		for _, v := range values {
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			o.AggData = append(o.AggData, b)
		}
	} else {
		// If must be raw output, so just marshal the single value.
		b, err := json.Marshal(mv.Value)
		if err != nil {
			return nil, err
		}
		o.RawData = b
	}
	return json.Marshal(o)
}

type MapperValues []*MapperValue

func (a MapperValues) Len() int           { return len(a) }
func (a MapperValues) Less(i, j int) bool { return a[i].Time < a[j].Time }
func (a MapperValues) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

type MapperOutput struct {
	Name      string            `json:"name,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	Fields    []string          `json:"fields,omitempty"` // Field names of returned data.
	Values    []*MapperValue    `json:"values,omitempty"` // For aggregates contains a single value at [0]
	cursorKey string            // Tagset-based key for the source cursor. Cached for performance reasons.
}

// MapperOutputJSON is the JSON-encoded representation of MapperOutput. The query data is represented
// as a raw JSON message, so decode is delayed, and can proceed in a custom manner.
type MapperOutputJSON struct {
	Name   string            `json:"name,omitempty"`
	Tags   map[string]string `json:"tags,omitempty"`
	Fields []string          `json:"fields,omitempty"` // Field names of returned data.
	Values json.RawMessage   `json:"values,omitempty"`
}

// MarshalJSON returns the JSON-encoded representation of a MapperOutput.
func (mo *MapperOutput) MarshalJSON() ([]byte, error) {
	o := &MapperOutputJSON{
		Name:   mo.Name,
		Tags:   mo.Tags,
		Fields: mo.Fields,
	}
	data, err := json.Marshal(mo.Values)
	if err != nil {
		return nil, err
	}
	o.Values = data

	return json.Marshal(o)
}

func (mo *MapperOutput) key() string {
	return mo.cursorKey
}

// RawMapper runs the map phase for non-aggregate, raw SELECT queries.
type RawMapper struct {
	shard      *Shard
	stmt       *influxql.SelectStatement
	qmin, qmax int64 // query time range

	tx          Tx
	cursors     []*TagSetCursor
	cursorIndex int

	selectFields []string
	selectTags   []string
	whereFields  []string

	ChunkSize int
}

// NewRawMapper returns a new instance of RawMapper.
func NewRawMapper(sh *Shard, stmt *influxql.SelectStatement) *RawMapper {
	return &RawMapper{
		shard: sh,
		stmt:  stmt,
	}
}

// Open opens and initializes the mapper.
func (m *RawMapper) Open() error {
	// Ignore if node has the shard but hasn't written to it yet.
	if m.shard == nil {
		return nil
	}

	// Rewrite statement.
	stmt, err := m.shard.index.RewriteSelectStatement(m.stmt)
	if err != nil {
		return err
	}
	m.stmt = stmt

	// Set all time-related parameters on the mapper.
	m.qmin, m.qmax = influxql.TimeRangeAsEpochNano(m.stmt.Condition)

	// Get a read-only transaction.
	tx, err := m.shard.engine.Begin(false)
	if err != nil {
		return err
	}
	m.tx = tx

	selectFields := newStringSet()
	selectTags := newStringSet()
	whereFields := newStringSet()

	// Open cursors for each measurement.
	for _, src := range m.stmt.Sources {
		// Retrieve measurement index reference. Ignore if not exists.
		mm := m.shard.index.Measurement(src.(*influxql.Measurement).Name)
		if mm == nil {
			continue
		}

		// Open cursors for measurement.
		info, err := m.openMeasurement(mm)
		if err != nil {
			return err
		}

		// Append fields & tags.
		selectFields.add(info.SelectFields...)
		selectTags.add(info.SelectTags...)
		whereFields.add(info.WhereFields...)
	}

	m.selectFields = selectFields.list()
	m.selectTags = selectTags.list()
	m.whereFields = whereFields.list()

	// Remove cursors if no SELECT fields are present.
	if len(m.selectFields) == 0 {
		m.cursors = nil
	}

	return nil
}

func (m *RawMapper) openMeasurement(mm *Measurement) (SelectInfo, error) {
	// Validate and return selection info.
	info, err := mm.ValidateSelectStatement(m.stmt)
	if err != nil {
		return info, err
	}

	// Create all cursors for reading the data from this shard.
	direction := Direction(m.stmt.TimeAscending())
	for _, t := range info.TagSets {
		cursors := []*seriesCursor{}

		for i, key := range t.SeriesKeys {
			c := m.tx.Cursor(key, direction)
			if c == nil {
				continue
			}

			seriesTags := m.shard.index.TagsForSeries(key)
			cm := newSeriesCursor(c, t.Filters[i], seriesTags)
			cursors = append(cursors, cm)
		}

		tsc := NewTagSetCursor(mm.Name, t.Tags, cursors, m.shard.FieldCodec(mm.Name))
		if direction.Forward() {
			tsc.SeekTo(m.qmin)
		} else {
			tsc.SeekTo(m.qmax)
		}
		m.cursors = append(m.cursors, tsc)
	}

	sort.Sort(TagSetCursors(m.cursors))

	return info, nil
}

// Close closes the mapper.
func (m *RawMapper) Close() {
	if m != nil && m.tx != nil {
		m.tx.Rollback()
	}
}

// TagSets returns the list of tag sets for which this mapper has data.
func (m *RawMapper) TagSets() []string { return TagSetCursors(m.cursors).Keys() }

// Fields returns all SELECT fields.
func (m *RawMapper) Fields() []string { return append(m.selectFields, m.selectTags...) }

// NextChunk returns the next chunk of data.
// Data is ordered the same as TagSets. Each chunk contains one tag set.
// If there is no more data for any tagset, nil will be returned.
func (m *RawMapper) NextChunk() (interface{}, error) {
	var output *MapperOutput
	for {
		// All tagset cursors processed. NextChunk'ing complete.
		if m.cursorIndex == len(m.cursors) {
			return nil, nil
		}

		cursor := m.cursors[m.cursorIndex]

		k, v := cursor.Next(m.qmin, m.qmax, m.selectFields, m.whereFields)
		if v == nil {
			// Tagset cursor is empty, move to next one.
			m.cursorIndex++
			if output != nil {
				// There is data, so return it and continue when next called.
				return output, nil
			} else {
				// Just go straight to the next cursor.
				continue
			}
		}

		if output == nil {
			output = &MapperOutput{
				Name:      cursor.measurement,
				Tags:      cursor.tags,
				Fields:    m.selectFields,
				cursorKey: cursor.key(),
			}
		}

		output.Values = append(output.Values, &MapperValue{
			Time:  k,
			Value: v,
			Tags:  cursor.Tags(),
		})

		if len(output.Values) == m.ChunkSize {
			return output, nil
		}
	}
}

// AggregateMapper runs the map phase for aggregate SELECT queries.
type AggregateMapper struct {
	shard      *Shard
	stmt       *influxql.SelectStatement
	qmin, qmax int64 // query time range

	tx          Tx
	cursors     []*TagSetCursor
	cursorIndex int

	interval     int   // Current interval for which data is being fetched.
	intervalN    int   // Maximum number of intervals to return.
	intervalSize int64 // Size of each interval.
	qminWindow   int64 // Minimum time of the query floored to start of interval.

	mapFuncs   []mapFunc // The mapping functions.
	fieldNames []string  // the field name being read for mapping.

	selectFields []string
	selectTags   []string
	whereFields  []string
}

// NewAggregateMapper returns a new instance of AggregateMapper.
func NewAggregateMapper(sh *Shard, stmt *influxql.SelectStatement) *AggregateMapper {
	return &AggregateMapper{
		shard: sh,
		stmt:  stmt,
	}
}

// Open opens and initializes the mapper.
func (m *AggregateMapper) Open() error {
	// Ignore if node has the shard but hasn't written to it yet.
	if m.shard == nil {
		return nil
	}

	// Rewrite statement.
	stmt, err := m.shard.index.RewriteSelectStatement(m.stmt)
	if err != nil {
		return err
	}
	m.stmt = stmt

	// Set all time-related parameters on the mapper.
	m.qmin, m.qmax = influxql.TimeRangeAsEpochNano(m.stmt.Condition)

	if err := m.initializeMapFunctions(); err != nil {
		return err
	}

	// For GROUP BY time queries, limit the number of data points returned by the limit and offset
	d, err := m.stmt.GroupByInterval()
	if err != nil {
		return err
	}

	m.intervalSize = d.Nanoseconds()
	if m.qmin == 0 || m.intervalSize == 0 {
		m.intervalN = 1
		m.intervalSize = m.qmax - m.qmin
	} else {
		intervalTop := m.qmax/m.intervalSize*m.intervalSize + m.intervalSize
		intervalBottom := m.qmin / m.intervalSize * m.intervalSize
		m.intervalN = int((intervalTop - intervalBottom) / m.intervalSize)
	}

	if m.stmt.Limit > 0 || m.stmt.Offset > 0 {
		// ensure that the offset isn't higher than the number of points we'd get
		if m.stmt.Offset > m.intervalN {
			return nil
		}

		// Take the lesser of either the pre computed number of GROUP BY buckets that
		// will be in the result or the limit passed in by the user
		if m.stmt.Limit < m.intervalN {
			m.intervalN = m.stmt.Limit
		}
	}

	// If we are exceeding our MaxGroupByPoints error out
	if m.intervalN > MaxGroupByPoints {
		return errors.New("too many points in the group by interval. maybe you forgot to specify a where time clause?")
	}

	// Ensure that the start time for the results is on the start of the window.
	m.qminWindow = m.qmin
	if m.intervalSize > 0 && m.intervalN > 1 {
		m.qminWindow = m.qminWindow / m.intervalSize * m.intervalSize
	}

	// Get a read-only transaction.
	tx, err := m.shard.engine.Begin(false)
	if err != nil {
		return err
	}
	m.tx = tx

	selectFields := newStringSet()
	selectTags := newStringSet()
	whereFields := newStringSet()

	// Open cursors for each measurement.
	for _, src := range m.stmt.Sources {
		// Retrieve measurement index reference. Ignore if not exists.
		mm := m.shard.index.Measurement(src.(*influxql.Measurement).Name)
		if mm == nil {
			continue
		}

		// Open cursors for measurement.
		info, err := m.openMeasurement(mm)
		if err != nil {
			return err
		}

		// Append fields & tags.
		selectFields.add(info.SelectFields...)
		selectTags.add(info.SelectTags...)
		whereFields.add(info.WhereFields...)
	}

	m.selectFields = selectFields.list()
	m.selectTags = selectTags.list()
	m.whereFields = whereFields.list()

	return nil
}

func (m *AggregateMapper) openMeasurement(mm *Measurement) (SelectInfo, error) {
	// Validate and return selection info.
	info, err := mm.ValidateSelectStatement(m.stmt)
	if err != nil {
		return info, err
	}

	// Create all cursors for reading the data from this shard.
	for _, t := range info.TagSets {
		cursors := []*seriesCursor{}

		for i, key := range t.SeriesKeys {
			c := m.tx.Cursor(key, Forward)
			if c == nil {
				continue
			}

			seriesTags := m.shard.index.TagsForSeries(key)
			cursors = append(cursors, newSeriesCursor(c, t.Filters[i], seriesTags))
		}

		tsc := NewTagSetCursor(mm.Name, t.Tags, cursors, m.shard.FieldCodec(mm.Name))
		tsc.SeekTo(m.qmin)
		m.cursors = append(m.cursors, tsc)
	}

	sort.Sort(TagSetCursors(m.cursors))

	return info, nil
}

// initializeMapFunctions initialize the mapping functions for the mapper.
func (m *AggregateMapper) initializeMapFunctions() error {
	// Set up each mapping function for this statement.
	aggregates := m.stmt.FunctionCalls()
	m.mapFuncs = make([]mapFunc, len(aggregates))
	m.fieldNames = make([]string, len(m.mapFuncs))

	for i, c := range aggregates {
		mfn, err := initializeMapFunc(c)
		if err != nil {
			return err
		}
		m.mapFuncs[i] = mfn

		// Check for calls like `derivative(lmean(value), 1d)`
		var nested *influxql.Call = c
		if fn, ok := c.Args[0].(*influxql.Call); ok {
			nested = fn
		}
		switch lit := nested.Args[0].(type) {
		case *influxql.VarRef:
			m.fieldNames[i] = lit.Val
		case *influxql.Distinct:
			if c.Name != "count" {
				return fmt.Errorf("aggregate call didn't contain a field %s", c.String())
			}
			m.fieldNames[i] = lit.Val
		default:
			return fmt.Errorf("aggregate call didn't contain a field %s", c.String())
		}
	}

	return nil
}

// Close closes the mapper.
func (m *AggregateMapper) Close() {
	if m != nil && m.tx != nil {
		m.tx.Rollback()
	}
	return
}

// TagSets returns the list of tag sets for which this mapper has data.
func (m *AggregateMapper) TagSets() []string { return TagSetCursors(m.cursors).Keys() }

// Fields returns all SELECT fields.
func (m *AggregateMapper) Fields() []string { return append(m.selectFields, m.selectTags...) }

// NextChunk returns the next interval of data.
// Tagsets are always processed in the same order as AvailTagsSets().
// When there is no more data for any tagset nil is returned.
func (m *AggregateMapper) NextChunk() (interface{}, error) {
	var tmin, tmax int64
	for {
		// All tagset cursors processed. NextChunk'ing complete.
		if m.cursorIndex == len(m.cursors) {
			return nil, nil
		}

		// All intervals complete for this tagset. Move to the next tagset.
		tmin, tmax = m.nextInterval()
		if tmin < 0 {
			m.interval = 0
			m.cursorIndex++
			continue
		}
		break
	}

	// Prep the return data for this tagset.
	// This will hold data for a single interval for a single tagset.
	tsc := m.cursors[m.cursorIndex]
	output := &MapperOutput{
		Name:      tsc.measurement,
		Tags:      tsc.tags,
		Fields:    m.selectFields,
		Values:    make([]*MapperValue, 1),
		cursorKey: tsc.key(),
	}

	// Aggregate values only use the first entry in the Values field.
	// Set the time to the start of the interval.
	output.Values[0] = &MapperValue{
		Time:  tmin,
		Value: make([]interface{}, 0),
	}

	// Always clamp tmin and tmax. This can happen as bucket-times are bucketed to the nearest
	// interval. This is necessary to grab the "partial" buckets at the beginning and end of the time range
	qmin, qmax := tmin, tmax
	if qmin < m.qmin {
		qmin = m.qmin
	}
	if qmax > m.qmax {
		qmax = m.qmax + 1
	}

	tsc.pointHeap = newPointHeap()
	for i := range m.mapFuncs {
		// Prime the tagset cursor for the start of the interval. This is not ideal, as
		// it should really calculate the values all in 1 pass, but that would require
		// changes to the mapper functions, which can come later.
		// Prime the buffers.
		for i := 0; i < len(tsc.cursors); i++ {
			k, v := tsc.cursors[i].SeekTo(qmin)
			if k == -1 || k > tmax {
				continue
			}
			p := &pointHeapItem{
				timestamp: k,
				value:     v,
				cursor:    tsc.cursors[i],
			}
			heap.Push(tsc.pointHeap, p)
		}

		// Execute the map function which walks the entire interval, and aggregates the result.
		output.Values[0].Value = append(
			output.Values[0].Value.([]interface{}),
			m.mapFuncs[i](&AggregateTagSetCursor{
				cursor: tsc,
				tmin:   tmin,
				stmt:   m.stmt,

				qmin: qmin,
				qmax: qmax,

				selectFields: []string{m.fieldNames[i]},
				whereFields:  m.whereFields,
			}),
		)
	}

	return output, nil
}

// nextInterval returns the next interval for which to return data.
// If start is less than 0 there are no more intervals.
func (m *AggregateMapper) nextInterval() (start, end int64) {
	t := m.qminWindow + int64(m.interval+m.stmt.Offset)*m.intervalSize

	// On to next interval.
	m.interval++
	if t > m.qmax || m.interval > m.intervalN {
		start, end = -1, 1
	} else {
		start, end = t, t+m.intervalSize
	}
	return
}

// AggregateTagSetCursor wraps a standard tagSetCursor, such that the values it emits are aggregated by intervals.
type AggregateTagSetCursor struct {
	cursor *TagSetCursor

	tmin int64
	stmt *influxql.SelectStatement

	qmin, qmax   int64
	selectFields []string
	whereFields  []string
}

// Next returns the next aggregate value for the cursor.
func (a *AggregateTagSetCursor) Next() (time int64, value interface{}) {
	return a.cursor.Next(a.qmin, a.qmax, a.selectFields, a.whereFields)
}

// Tags returns the current tags for the cursor
func (a *AggregateTagSetCursor) Tags() map[string]string { return a.cursor.Tags() }

// TMin returns the current floor time for the bucket being worked on
func (a *AggregateTagSetCursor) TMin() int64 {
	if len(a.stmt.Dimensions) == 0 {
		return -1
	}
	if !a.stmt.HasTimeFieldSpecified() {
		return a.tmin
	}
	return -1
}
