package tstorage

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// diskWAL contains multiple segment files. One segment is responsible for one partition.
// They can be easily sorted because they are named using the created timestamp.
// Macro layout is like:
/*
  .wal/
  ├── 1635299332
  └── 1635299333
*/
type diskWAL struct {
	dir string
	// Buffered-writer to the active segment
	w *bufio.Writer
	// File descriptor to the active segment
	fd           *os.File
	bufferedSize int
	mu           sync.Mutex
}

func newDiskWAL(dir string, bufferedSize int) (wal, error) {
	if err := os.MkdirAll(dir, fs.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to make WAL dir: %w", err)
	}
	f, err := createSegmentFile(dir)
	if err != nil {
		return nil, err
	}

	return &diskWAL{
		dir:          dir,
		w:            bufio.NewWriterSize(f, bufferedSize),
		fd:           f,
		bufferedSize: bufferedSize,
	}, nil
}

// append appends the given entry to the end of a file via the file descriptor it has.
func (w *diskWAL) append(op walOperation, rows []Row) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	switch op {
	case operationInsert:
		for _, row := range rows {
			// Write the operation type
			if err := w.w.WriteByte(byte(op)); err != nil {
				return fmt.Errorf("failed to write operation: %w", err)
			}
			name := marshalMetricName(row.Metric, row.Labels)
			// Write the length of the metric name
			lBuf := make([]byte, binary.MaxVarintLen64)
			n := binary.PutUvarint(lBuf, uint64(len(name)))
			if _, err := w.w.Write(lBuf[:n]); err != nil {
				return fmt.Errorf("failed to write the length of the metric name: %w", err)
			}
			// Write the metric name
			if _, err := w.w.WriteString(name); err != nil {
				return fmt.Errorf("failed to write the metric name: %w", err)
			}
			// Write the timestamp
			tsBuf := make([]byte, binary.MaxVarintLen64)
			n = binary.PutVarint(tsBuf, row.DataPoint.Timestamp)
			if _, err := w.w.Write(tsBuf[:n]); err != nil {
				return fmt.Errorf("failed to write the timestamp: %w", err)
			}
			// Write the value
			vBuf := make([]byte, binary.MaxVarintLen64)
			n = binary.PutUvarint(vBuf, math.Float64bits(row.DataPoint.Value))
			if _, err := w.w.Write(vBuf[:n]); err != nil {
				return fmt.Errorf("failed to write the value: %w", err)
			}
		}
	default:
		return fmt.Errorf("unknown operation %v given", op)
	}
	if w.bufferedSize == 0 {
		return w.flush()
	}

	return nil
}

// truncateOldest removes only the oldest segment.
func (w *diskWAL) truncateOldest() error {
	// FIXME: Find the oldest segment and remove it
	return nil
}

// flush flushes all buffered entries to the underlying file.
func (w *diskWAL) flush() error {
	if err := w.w.Flush(); err != nil {
		return fmt.Errorf("failed to flush buffered-data into the underlying WAL file: %w", err)
	}
	return nil
}

// punctuate set boundary and creates a new segment.
func (w *diskWAL) punctuate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.flush(); err != nil {
		return err
	}
	if err := w.fd.Close(); err != nil {
		return nil
	}
	f, err := createSegmentFile(w.dir)
	if err != nil {
		return err
	}
	w.fd = f
	w.w = bufio.NewWriterSize(f, w.bufferedSize)
	return nil
}

// removeAll removes all segments.
func (w *diskWAL) removeAll() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.fd.Close(); err != nil {
		return err
	}
	return os.RemoveAll(w.dir)
}

// createSegmentFile creates a new file with the name of the current timestamp.
func createSegmentFile(dir string) (*os.File, error) {
	now := int(time.Now().Unix())
	name := strconv.Itoa(now)
	_, err := os.Stat(filepath.Join(dir, name))
	if !errors.Is(err, os.ErrNotExist) {
		name = strconv.Itoa(now + 1)
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to create segment file: %w", err)
	}
	return f, nil
}

type walRecord struct {
	op  walOperation
	row Row
}

type diskWALReader struct {
	dir          string
	files        []os.DirEntry
	rowsToInsert []Row
}

func newDiskWALReader(dir string) (*diskWALReader, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read the WAL dir: %w", err)
	}

	return &diskWALReader{
		dir:          dir,
		files:        files,
		rowsToInsert: make([]Row, 0),
	}, nil
}

// readAll reads all segment files and caches the result for each operation.
func (f *diskWALReader) readAll() error {
	for _, file := range f.files {
		if file.IsDir() {
			return fmt.Errorf("unexpected directory found under the WAL directory: %s", file.Name())
		}
		fd, err := os.Open(filepath.Join(f.dir, file.Name()))
		if err != nil {
			return fmt.Errorf("failed to open WAL segment file: %w", err)
		}
		segment := &segment{
			file: fd,
			r:    bufio.NewReader(fd),
		}
		for segment.next() {
			rec := segment.record()
			switch rec.op {
			case operationInsert:
				f.rowsToInsert = append(f.rowsToInsert, rec.row)
			}
		}
		if err := segment.close(); err != nil {
			return err
		}
		if segment.error() != nil {
			return fmt.Errorf("encounter an error while reading WAL segment file %q: %w", file.Name(), segment.error())
		}
	}
	return nil
}

// segment represents a segment file.
type segment struct {
	file    *os.File
	r       *bufio.Reader
	current walRecord
	err     error
}

func (f *segment) next() bool {
	op, err := f.r.ReadByte()
	if errors.Is(err, io.EOF) {
		return false
	}
	if err != nil {
		f.err = err
		return false
	}
	switch walOperation(op) {
	case operationInsert:
		// Read the length of metric name.
		metricLen, err := binary.ReadUvarint(f.r)
		if err != nil {
			f.err = fmt.Errorf("failed to read the length of metric name: %w", err)
			return false
		}
		// Read the metric name.
		metric := make([]byte, int(metricLen))
		if _, err := io.ReadFull(f.r, metric); err != nil {
			f.err = fmt.Errorf("failed to read the metric name: %w", err)
			return false
		}
		// Read timestamp.
		ts, err := binary.ReadVarint(f.r)
		if err != nil {
			f.err = fmt.Errorf("failed to read timestamp: %w", err)
			return false
		}
		// Read value.
		val, err := binary.ReadUvarint(f.r)
		if err != nil {
			f.err = fmt.Errorf("failed to read value: %w", err)
			return false
		}
		f.current = walRecord{
			op: walOperation(op),
			row: Row{
				Metric: string(metric),
				DataPoint: DataPoint{
					Timestamp: ts,
					Value:     math.Float64frombits(val),
				},
			},
		}
	default:
		f.err = fmt.Errorf("unknown operation %v found", op)
		return false
	}

	return true
}

// error gives back an error if it has been facing an error while reading.
func (f *segment) error() error {
	return f.err
}

func (f *segment) record() *walRecord {
	return &f.current
}

func (f *segment) close() error {
	return f.file.Close()
}
