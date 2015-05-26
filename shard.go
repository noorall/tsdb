package tsdb

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/influxdb/influxdb/influxql"

	"github.com/boltdb/bolt"
)

// Shard represents a self-contained time series database. An inverted index of the measurement and tag data is
// kept along with the raw time series data. Data can be split across many shards. The query engine in TSDB
// is responsible for combining the output of many shards into a single query result.
type Shard struct {
	db    *bolt.DB // underlying data store
	index *DatabaseIndex
	path  string

	mu                sync.RWMutex
	measurementFields map[string]*measurementFields // measurement name to their fields
}

// NewShard returns a new initialized Shard
func NewShard(index *DatabaseIndex, path string) *Shard {
	return &Shard{
		index:             index,
		path:              path,
		measurementFields: make(map[string]*measurementFields),
	}
}

// open initializes and opens the shard's store.
func (s *Shard) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Return if the shard is already open
	if s.db != nil {
		return nil
	}

	// Open store on shard.
	store, err := bolt.Open(s.path, 0666, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return err
	}
	s.db = store

	// Initialize store.
	if err := s.db.Update(func(tx *bolt.Tx) error {
		_, _ = tx.CreateBucketIfNotExists([]byte("series"))
		_, _ = tx.CreateBucketIfNotExists([]byte("fields"))
		_, _ = tx.CreateBucketIfNotExists([]byte("values"))

		return nil
	}); err != nil {
		_ = s.Close()
		return fmt.Errorf("init: %s", err)
	}

	return s.loadMetadataIndex()
}

// close shuts down the shard's store.
func (s *Shard) Close() error {
	if s.db != nil {
		_ = s.db.Close()
	}
	return nil
}

// struct to hold information for a field to create on a measurement
type fieldCreate struct {
	measurement string
	field       *field
}

// struct to hold information for a series to create
type seriesCreate struct {
	measurement string
	series      *Series
}

// WritePoints will write the raw data points and any new metadata to the index in the shard
func (s *Shard) WritePoints(points []Point) error {
	seriesToCreate, fieldsToCreate, err := s.validateSeriesAndFields(points)
	if err != nil {
		return err
	}

	// add any new series to the in-memory index
	if len(seriesToCreate) > 0 {
		s.index.mu.Lock()
		for _, ss := range seriesToCreate {
			s.index.createSeriesIndexIfNotExists(ss.measurement, ss.series)
		}
		s.index.mu.Unlock()
	}

	// add any new fields and keep track of what needs to be saved
	measurementFieldsToSave, err := s.createFieldsAndMeasurements(fieldsToCreate)
	if err != nil {
		return err
	}

	// make sure all data is encoded before attempting to save to bolt
	for _, p := range points {
		// marshal the raw data if it hasn't been marshaled already
		if p.Data() == nil {
			// this was populated earlier, don't need to validate that it's there.
			data, err := s.measurementFields[p.Name()].codec.EncodeFields(p.Fields())
			if err != nil {
				return err
			}
			p.SetData(data)
		}
	}

	// save to the underlying bolt instance
	if err := s.db.Update(func(tx *bolt.Tx) error {
		// save any new metadata
		if len(seriesToCreate) > 0 {
			b := tx.Bucket([]byte("series"))
			for _, sc := range seriesToCreate {
				if err := b.Put([]byte(sc.series.Key), mustMarshalJSON(sc.series)); err != nil {
					return err
				}
			}
		}
		if len(measurementFieldsToSave) > 0 {
			b := tx.Bucket([]byte("fields"))
			for name, m := range measurementFieldsToSave {
				if err := b.Put([]byte(name), mustMarshalJSON(m)); err != nil {
					return err
				}
			}
		}

		// save the raw point data
		b := tx.Bucket([]byte("values"))
		for _, p := range points {
			bp, err := b.CreateBucketIfNotExists([]byte(p.Key()))
			if err != nil {
				return err
			}
			if err := bp.Put(u64tob(p.UnixNano()), p.Data()); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		_ = s.Close()
		return err
	}

	return nil
}

func (s *Shard) createFieldsAndMeasurements(fieldsToCreate []*fieldCreate) (map[string]*measurementFields, error) {
	if len(fieldsToCreate) == 0 {
		return nil, nil
	}

	s.index.mu.Lock()
	s.mu.Lock()
	defer s.index.mu.Unlock()
	defer s.mu.Unlock()

	// add fields
	measurementsToSave := make(map[string]*measurementFields)
	for _, f := range fieldsToCreate {

		m := s.measurementFields[f.measurement]
		if m == nil {
			m = measurementsToSave[f.measurement]
			if m == nil {
				m = &measurementFields{Fields: make(map[string]*field)}
			}
			s.measurementFields[f.measurement] = m
		}

		measurementsToSave[f.measurement] = m

		// add the field to the in memory index
		if err := m.createFieldIfNotExists(f.field.Name, f.field.Type); err != nil {
			return nil, err
		}

		// ensure the measurement is in the index and the field is there
		measurement := s.index.createMeasurementIndexIfNotExists(f.measurement)
		measurement.FieldNames[f.field.Name] = struct{}{}
	}

	return measurementsToSave, nil
}

// validateSeriesAndFields checks which series and fields are new and whose metadata should be saved and indexed
func (s *Shard) validateSeriesAndFields(points []Point) ([]*seriesCreate, []*fieldCreate, error) {
	var seriesToCreate []*seriesCreate
	var fieldsToCreate []*fieldCreate

	// get the mutex for the in memory index, which is shared across shards
	s.index.mu.RLock()
	defer s.index.mu.RUnlock()

	// get the shard mutex for locally defined fields
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, p := range points {
		// see if the series should be added to the index
		if ss := s.index.series[p.Key()]; ss == nil {
			series := &Series{Key: p.Key(), Tags: p.Tags()}
			seriesToCreate = append(seriesToCreate, &seriesCreate{p.Name(), series})
		}

		// see if the field definitions need to be saved to the shard
		mf := s.measurementFields[p.Name()]
		if mf == nil {
			for name, value := range p.Fields() {
				fieldsToCreate = append(fieldsToCreate, &fieldCreate{p.Name(), &field{Name: name, Type: influxql.InspectDataType(value)}})
			}
			continue // skip validation since all fields are new
		}

		// validate field types and encode data
		for name, value := range p.Fields() {
			if f := mf.Fields[name]; f != nil {
				// Field present in Metastore, make sure there is no type conflict.
				if f.Type != influxql.InspectDataType(value) {
					return nil, nil, fmt.Errorf("input field \"%s\" is type %T, already exists as type %s", name, value, f.Type)
				}

				data, err := mf.codec.EncodeFields(p.Fields())
				if err != nil {
					return nil, nil, err
				}
				p.SetData(data)
				continue // Field is present, and it's of the same type. Nothing more to do.
			}

			fieldsToCreate = append(fieldsToCreate, &fieldCreate{p.Name(), &field{Name: name, Type: influxql.InspectDataType(value)}})
		}
	}

	return seriesToCreate, fieldsToCreate, nil
}

// loadsMetadataIndex loads the shard metadata into memory. This should only be called by Open
func (s *Shard) loadMetadataIndex() error {
	return s.db.View(func(tx *bolt.Tx) error {
		s.index.mu.Lock()
		defer s.index.mu.Unlock()

		// load measurement metadata
		meta := tx.Bucket([]byte("fields"))
		c := meta.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			m := s.index.createMeasurementIndexIfNotExists(string(k))
			mf := &measurementFields{}
			mustUnmarshalJSON(v, mf)
			for name, _ := range mf.Fields {
				m.FieldNames[name] = struct{}{}
			}
			mf.codec = newFieldCodec(mf.Fields)
		}

		// load series metadata
		meta = tx.Bucket([]byte("series"))
		c = meta.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var series *Series
			mustUnmarshalJSON(v, &series)
			s.index.createSeriesIndexIfNotExists(measurementFromSeriesKey(string(k)), series)
		}
		return nil
	})
}

type measurementFields struct {
	Fields map[string]*field `json:"fields"`
	codec  *fieldCodec
}

// createFieldIfNotExists creates a new field with an autoincrementing ID.
// Returns an error if 255 fields have already been created on the measurement or
// the fields already exists with a different type.
func (m *measurementFields) createFieldIfNotExists(name string, typ influxql.DataType) error {
	// Ignore if the field already exists.
	if f := m.Fields[name]; f != nil {
		if f.Type != typ {
			return ErrFieldTypeConflict
		}
		return nil
	}

	// Only 255 fields are allowed. If we go over that then return an error.
	if len(m.Fields)+1 > math.MaxUint8 {
		return ErrFieldOverflow
	}

	// Create and append a new field.
	f := &field{
		ID:   uint8(len(m.Fields) + 1),
		Name: name,
		Type: typ,
	}
	m.Fields[name] = f
	m.codec = newFieldCodec(m.Fields)

	return nil
}

// Field represents a series field.
type field struct {
	ID   uint8             `json:"id,omitempty"`
	Name string            `json:"name,omitempty"`
	Type influxql.DataType `json:"type,omitempty"`
}

// FieldCodec providecs encoding and decoding functionality for the fields of a given
// Measurement. It is a distinct type to avoid locking writes on this node while
// potentially long-running queries are executing.
//
// It is not affected by changes to the Measurement object after codec creation.
type fieldCodec struct {
	fieldsByID   map[uint8]*field
	fieldsByName map[string]*field
}

// NewFieldCodec returns a FieldCodec for the given Measurement. Must be called with
// a RLock that protects the Measurement.
func newFieldCodec(fields map[string]*field) *fieldCodec {
	fieldsByID := make(map[uint8]*field, len(fields))
	fieldsByName := make(map[string]*field, len(fields))
	for _, f := range fields {
		fieldsByID[f.ID] = f
		fieldsByName[f.Name] = f
	}
	return &fieldCodec{fieldsByID: fieldsByID, fieldsByName: fieldsByName}
}

// EncodeFields converts a map of values with string keys to a byte slice of field
// IDs and values.
//
// If a field exists in the codec, but its type is different, an error is returned. If
// a field is not present in the codec, the system panics.
func (f *fieldCodec) EncodeFields(values map[string]interface{}) ([]byte, error) {
	// Allocate byte slice
	b := make([]byte, 0, 10)

	for k, v := range values {
		field := f.fieldsByName[k]
		if field == nil {
			panic(fmt.Sprintf("field does not exist for %s", k))
		} else if influxql.InspectDataType(v) != field.Type {
			return nil, fmt.Errorf("field \"%s\" is type %T, mapped as type %s", k, k, field.Type)
		}

		var buf []byte

		switch field.Type {
		case influxql.Float:
			value := v.(float64)
			buf = make([]byte, 9)
			binary.BigEndian.PutUint64(buf[1:9], math.Float64bits(value))
		case influxql.Integer:
			var value uint64
			switch v.(type) {
			case int:
				value = uint64(v.(int))
			case int32:
				value = uint64(v.(int32))
			case int64:
				value = uint64(v.(int64))
			default:
				panic(fmt.Sprintf("invalid integer type: %T", v))
			}
			buf = make([]byte, 9)
			binary.BigEndian.PutUint64(buf[1:9], value)
		case influxql.Boolean:
			value := v.(bool)

			// Only 1 byte need for a boolean.
			buf = make([]byte, 2)
			if value {
				buf[1] = byte(1)
			}
		case influxql.String:
			value := v.(string)
			if len(value) > maxStringLength {
				value = value[:maxStringLength]
			}
			// Make a buffer for field ID (1 bytes), the string length (2 bytes), and the string.
			buf = make([]byte, len(value)+3)

			// Set the string length, then copy the string itself.
			binary.BigEndian.PutUint16(buf[1:3], uint16(len(value)))
			for i, c := range []byte(value) {
				buf[i+3] = byte(c)
			}
		default:
			panic(fmt.Sprintf("unsupported value type during encode fields: %T", v))
		}

		// Always set the field ID as the leading byte.
		buf[0] = field.ID

		// Append temp buffer to the end.
		b = append(b, buf...)
	}

	return b, nil
}

// DecodeByID scans a byte slice for a field with the given ID, converts it to its
// expected type, and return that value.
func (f *fieldCodec) decodeByID(targetID uint8, b []byte) (interface{}, error) {
	if len(b) == 0 {
		return 0, ErrFieldNotFound
	}

	for {
		if len(b) < 1 {
			// No more bytes.
			break
		}
		field, ok := f.fieldsByID[b[0]]
		if !ok {
			// This can happen, though is very unlikely. If this node receives encoded data, to be written
			// to disk, and is queried for that data before its metastore is updated, there will be no field
			// mapping for the data during decode. All this can happen because data is encoded by the node
			// that first received the write request, not the node that actually writes the data to disk.
			// So if this happens, the read must be aborted.
			return 0, ErrFieldUnmappedID
		}

		var value interface{}
		switch field.Type {
		case influxql.Float:
			// Move bytes forward.
			value = math.Float64frombits(binary.BigEndian.Uint64(b[1:9]))
			b = b[9:]
		case influxql.Integer:
			value = int64(binary.BigEndian.Uint64(b[1:9]))
			b = b[9:]
		case influxql.Boolean:
			if b[1] == 1 {
				value = true
			} else {
				value = false
			}
			// Move bytes forward.
			b = b[2:]
		case influxql.String:
			size := binary.BigEndian.Uint16(b[1:3])
			value = string(b[3 : 3+size])
			// Move bytes forward.
			b = b[size+3:]
		default:
			panic(fmt.Sprintf("unsupported value type during decode by id: %T", field.Type))
		}

		if field.ID == targetID {
			return value, nil
		}
	}

	return 0, ErrFieldNotFound
}

// DecodeFields decodes a byte slice into a set of field ids and values.
func (f *fieldCodec) DecodeFields(b []byte) (map[uint8]interface{}, error) {
	if len(b) == 0 {
		return nil, nil
	}

	// Create a map to hold the decoded data.
	values := make(map[uint8]interface{}, 0)

	for {
		if len(b) < 1 {
			// No more bytes.
			break
		}

		// First byte is the field identifier.
		fieldID := b[0]
		field := f.fieldsByID[fieldID]
		if field == nil {
			// See note in DecodeByID() regarding field-mapping failures.
			return nil, ErrFieldUnmappedID
		}

		var value interface{}
		switch field.Type {
		case influxql.Float:
			value = math.Float64frombits(binary.BigEndian.Uint64(b[1:9]))
			// Move bytes forward.
			b = b[9:]
		case influxql.Integer:
			value = int64(binary.BigEndian.Uint64(b[1:9]))
			// Move bytes forward.
			b = b[9:]
		case influxql.Boolean:
			if b[1] == 1 {
				value = true
			} else {
				value = false
			}
			// Move bytes forward.
			b = b[2:]
		case influxql.String:
			size := binary.BigEndian.Uint16(b[1:3])
			value = string(b[3 : size+3])
			// Move bytes forward.
			b = b[size+3:]
		default:
			panic(fmt.Sprintf("unsupported value type during decode fields: %T", f.fieldsByID[fieldID]))
		}

		values[fieldID] = value

	}

	return values, nil
}

// DecodeFieldsWithNames decodes a byte slice into a set of field names and values
func (f *fieldCodec) decodeFieldsWithNames(b []byte) (map[string]interface{}, error) {
	fields, err := f.DecodeFields(b)
	if err != nil {
		return nil, err
	}
	m := make(map[string]interface{})
	for id, v := range fields {
		field := f.fieldsByID[id]
		if field != nil {
			m[field.Name] = v
		}
	}
	return m, nil
}

// FieldByName returns the field by its name. It will return a nil if not found
func (f *fieldCodec) fieldByName(name string) *field {
	return f.fieldsByName[name]
}

// mustMarshal encodes a value to JSON.
// This will panic if an error occurs. This should only be used internally when
// an invalid marshal will cause corruption and a panic is appropriate.
func mustMarshalJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("marshal: " + err.Error())
	}
	return b
}

// mustUnmarshalJSON decodes a value from JSON.
// This will panic if an error occurs. This should only be used internally when
// an invalid unmarshal will cause corruption and a panic is appropriate.
func mustUnmarshalJSON(b []byte, v interface{}) {
	if err := json.Unmarshal(b, v); err != nil {
		panic("unmarshal: " + err.Error())
	}
}

func warn(v ...interface{})              { fmt.Fprintln(os.Stderr, v...) }
func warnf(msg string, v ...interface{}) { fmt.Fprintf(os.Stderr, msg+"\n", v...) }

// u64tob converts a uint64 into an 8-byte slice.
func u64tob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

var (
	// ErrFieldOverflow is returned when too many fields are created on a measurement.
	ErrFieldOverflow = errors.New("field overflow")

	// ErrFieldTypeConflict is returned when a new field already exists with a different type.
	ErrFieldTypeConflict = errors.New("field type conflict")

	// ErrFieldNotFound is returned when a field cannot be found.
	ErrFieldNotFound = errors.New("field not found")

	// ErrFieldUnmappedID is returned when the system is presented, during decode, with a field ID
	// there is no mapping for.
	ErrFieldUnmappedID = errors.New("field ID not mapped")
)
