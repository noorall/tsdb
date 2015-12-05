package tsm1

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type TSMFile interface {
	// Path returns the underlying file path for the TSMFile.  If the file
	// has not be written or loaded from disk, the zero value is returne.
	Path() string

	// Read returns all the values in the block where time t resides
	Read(key string, t time.Time) ([]Value, error)

	// Read returns all the values in the block identified by entry.
	ReadAt(entry *IndexEntry, values []Value) ([]Value, error)

	// Entries returns the index entries for all blocks for the given key.
	Entries(key string) []*IndexEntry

	// Returns true if the TSMFile may contain a value with the specified
	// key and time
	ContainsValue(key string, t time.Time) bool

	// Contains returns true if the file contains any values for the given
	// key.
	Contains(key string) bool

	// TimeRange returns the min and max time across all keys in the file.
	TimeRange() (time.Time, time.Time)

	// KeyRange returns the min and max keys in the file.
	KeyRange() (string, string)

	// Keys returns all keys contained in the file.
	Keys() []string

	// Type returns the block type of the values stored for the key.  Returns one of
	// BlockFloat64, BlockInt64, BlockBool, BlockString.  If key does not exist,
	// an error is returned.
	Type(key string) (byte, error)

	// Delete removes the key from the set of keys available in this file.
	Delete(key string) error

	// HasTombstones returns true if file contains values that have been deleted.
	HasTombstones() bool

	// Close the underlying file resources
	Close() error

	// Size returns the size of the file on disk in bytes.
	Size() int

	// Remove deletes the file from the filesystem
	Remove() error

	// Stats returns summary information about the TSM file.
	Stats() FileStat
}

type FileStore struct {
	mu sync.RWMutex

	currentGeneration int
	dir               string

	files []TSMFile
}

type FileStat struct {
	Path             string
	HasTombstone     bool
	Size             int
	MinTime, MaxTime time.Time
	MinKey, MaxKey   string
}

func (f FileStat) OverlapsTimeRange(min, max time.Time) bool {
	return (f.MinTime.Equal(max) || f.MinTime.Before(max)) &&
		(f.MaxTime.Equal(min) || f.MaxTime.After(min))
}

func (f FileStat) OverlapsKeyRange(min, max string) bool {
	return min != "" && max != "" && f.MinKey <= max && f.MaxKey >= min
}

func (f FileStat) ContainsKey(key string) bool {
	return f.MinKey >= key || key <= f.MaxKey
}

func NewFileStore(dir string) *FileStore {
	return &FileStore{
		dir: dir,
	}
}

// Returns the number of TSM files currently loaded
func (f *FileStore) Count() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.files)
}

// CurrentGeneration returns the max file ID + 1
func (f *FileStore) CurrentGeneration() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.currentGeneration
}

// NextGeneration returns the max file ID + 1
func (f *FileStore) NextGeneration() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.currentGeneration++
	return f.currentGeneration
}

func (f *FileStore) Add(files ...TSMFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = append(f.files, files...)
}

// Remove removes the files with matching paths from the set of active files.  It does
// not remove the paths from disk.
func (f *FileStore) Remove(paths ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var active []TSMFile
	for _, file := range f.files {
		keep := true
		for _, remove := range paths {
			if remove == file.Path() {
				keep = false
				break
			}
		}

		if keep {
			active = append(active, file)
		}
	}
	f.files = active
}

func (f *FileStore) Keys() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	uniqueKeys := map[string]struct{}{}
	for _, f := range f.files {
		for _, key := range f.Keys() {
			uniqueKeys[key] = struct{}{}
		}
	}

	var keys []string
	for key := range uniqueKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (f *FileStore) Type(key string) (byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	for _, f := range f.files {
		if f.Contains(key) {
			return f.Type(key)
		}
	}
	return 0, fmt.Errorf("unknown type for %v", key)
}

func (f *FileStore) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, file := range f.files {
		if file.Contains(key) {
			if err := file.Delete(key); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *FileStore) Open() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Not loading files from disk so nothing to do
	if f.dir == "" {
		return nil
	}

	files, err := filepath.Glob(filepath.Join(f.dir, fmt.Sprintf("*.%s", "tsm1dev")))
	if err != nil {
		return err
	}

	for _, fn := range files {
		// Keep track of the latest ID
		generation, _, err := ParseTSMFileName(fn)
		if err != nil {
			return err
		}

		if generation >= f.currentGeneration {
			f.currentGeneration = generation + 1
		}

		file, err := os.OpenFile(fn, os.O_RDONLY, 0666)
		if err != nil {
			return fmt.Errorf("error opening file %s: %v", fn, err)
		}

		df, err := NewTSMReaderWithOptions(TSMReaderOptions{
			MMAPFile: file,
		})
		if err != nil {
			return fmt.Errorf("error opening memory map for file %s: %v", fn, err)
		}

		f.files = append(f.files, df)
	}
	return nil
}

func (f *FileStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, f := range f.files {
		f.Close()
	}

	f.files = nil
	return nil
}

func (f *FileStore) Read(key string, t time.Time) ([]Value, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	for _, f := range f.files {
		// Can this file possibly contain this key and timestamp?
		if !f.Contains(key) {
			continue
		}

		// May have the key and time we are looking for so try to find
		v, err := f.Read(key, t)
		if err != nil {
			return nil, err
		}

		if len(v) > 0 {
			return v, nil
		}
	}
	return nil, nil
}

func (f *FileStore) KeyCursor(key string) *KeyCursor {
	f.mu.RLock()
	defer f.mu.RUnlock()

	var locations []*location
	for _, fd := range f.files {
		for _, ie := range fd.Entries(key) {
			locations = append(locations, &location{
				r:     fd,
				entry: ie,
			})
		}
	}

	return &KeyCursor{seeks: locations, buf: make([]Value, 1000)}
}

func (f *FileStore) Stats() []FileStat {
	f.mu.RLock()
	defer f.mu.RUnlock()
	stats := make([]FileStat, len(f.files))
	for i, fd := range f.files {
		stats[i] = fd.Stats()
	}

	return stats
}

func (f *FileStore) Replace(oldFiles, newFiles []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Copy the current set of active files while we rename
	// and load the new files.  We copy the pointers here to minimize
	// the time that locks are held as well as to ensure that the replacement
	// is atomic.©
	var updated []TSMFile
	for _, t := range f.files {
		updated = append(updated, t)
	}

	// Rename all the new files to make them live on restart
	for _, file := range newFiles {
		var newName = file
		if strings.HasSuffix(file, ".tmp") {
			// The new TSM files have a tmp extension.  First rename them.
			newName = file[:len(file)-4]
			if err := os.Rename(file, newName); err != nil {
				return err
			}
		}

		fd, err := os.Open(newName)
		if err != nil {
			return err
		}

		tsm, err := NewTSMReaderWithOptions(TSMReaderOptions{
			MMAPFile: fd,
		})
		if err != nil {
			return err
		}
		updated = append(updated, tsm)
	}

	// We need to prune our set of active files now
	var active []TSMFile
	for _, file := range updated {
		keep := true
		for _, remove := range oldFiles {
			if remove == file.Path() {
				keep = false
				if err := file.Close(); err != nil {
					return err
				}

				if err := file.Remove(); err != nil {
					return err
				}
				break
			}
		}

		if keep {
			active = append(active, file)
		}
	}

	f.files = active

	return nil
}

// ParseTSMFileName parses the generation and sequence from a TSM file name.
func ParseTSMFileName(name string) (int, int, error) {
	base := filepath.Base(name)
	idx := strings.Index(base, ".")
	if idx == -1 {
		return 0, 0, fmt.Errorf("file %s is named incorrectly", name)
	}

	id := base[:idx]

	parts := strings.Split(id, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("file %s is named incorrectly", name)
	}

	generation, err := strconv.ParseUint(parts[0], 10, 32)
	sequence, err := strconv.ParseUint(parts[1], 10, 32)

	return int(generation), int(sequence), err
}

type KeyCursor struct {
	seeks     []*location
	current   *location
	buf       []Value
	pos       int
	ascending bool
}

type location struct {
	r     TSMFile
	entry *IndexEntry
}

func (c *KeyCursor) SeekTo(t time.Time, ascending bool) ([]Value, error) {
	if len(c.seeks) == 0 {
		return nil, nil
	}
	c.current = nil

	if ascending {
		for i, e := range c.seeks {
			if t.Before(e.entry.MinTime) || e.entry.Contains(t) {
				c.current = e
				c.pos = i
				break
			}
		}
	} else {
		for i := len(c.seeks) - 1; i >= 0; i-- {
			e := c.seeks[i]
			if t.After(e.entry.MaxTime) || e.entry.Contains(t) {
				c.current = e
				c.pos = i
				break
			}
		}
	}

	if c.current == nil {
		return nil, nil
	}
	return c.current.r.ReadAt(c.current.entry, c.buf[:0])
}

func (c *KeyCursor) Next(ascending bool) ([]Value, error) {
	if ascending {
		c.pos++
		if c.pos >= len(c.seeks) {
			return nil, nil
		}
		c.current = c.seeks[c.pos]
		return c.current.r.ReadAt(c.current.entry, c.buf[:0])
	} else {
		c.pos--
		if c.pos < 0 {
			return nil, nil
		}
		c.current = c.seeks[c.pos]
		return c.current.r.ReadAt(c.current.entry, c.buf[:0])
	}
}
