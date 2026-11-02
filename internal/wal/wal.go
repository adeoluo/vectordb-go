// Package wal implements a segmented, append-only write-ahead log.
//
// # Record format
//
// Every record is framed as
//
//	+-----------+-----------+--------------------------------+
//	| len (u32) | crc (u32) | payload: lsn (u64) || data     |
//	+-----------+-----------+--------------------------------+
//
// All integers are little endian. len is the payload length and crc is the
// CRC-32C (Castagnoli) of the payload. LSNs are dense and strictly
// increasing: record n+1 has LSN n+1. Segments are named by the LSN of their
// first record (wal-00000000000000000001.log), so the set of segment names
// alone is enough to know which files a snapshot makes obsolete.
//
// # Recovery
//
// Open scans every segment. A record whose header is short, whose length is
// implausible, whose checksum does not match, or whose LSN breaks the
// sequence marks the end of the valid log. If that happens in the final
// segment, it is a torn write from a crash: the file is truncated back to the
// last valid record and fsynced. If it happens in an earlier segment, data
// that was once durable is damaged and Open fails rather than silently
// dropping records. See docs/adr/0002-wal-format.md.
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SyncPolicy controls when appended records are flushed to stable storage.
type SyncPolicy int

const (
	// SyncAlways fsyncs before Append returns. An acknowledged write survives
	// power loss. Concurrent writers share fsyncs (group commit). This is the
	// default.
	SyncAlways SyncPolicy = iota
	// SyncInterval fsyncs from a background goroutine every SyncInterval.
	// Acknowledged writes survive a process crash (the data is already in
	// the OS page cache) but up to one interval can be lost on power loss.
	SyncInterval
	// SyncNever leaves flushing to the operating system.
	SyncNever
)

// String returns the flag spelling of the policy.
func (p SyncPolicy) String() string {
	switch p {
	case SyncAlways:
		return "always"
	case SyncInterval:
		return "interval"
	case SyncNever:
		return "never"
	}
	return "unknown"
}

// ParseSyncPolicy parses "always", "interval" or "never".
func ParseSyncPolicy(s string) (SyncPolicy, error) {
	switch s {
	case "always":
		return SyncAlways, nil
	case "interval":
		return SyncInterval, nil
	case "never":
		return SyncNever, nil
	}
	return 0, fmt.Errorf("unknown fsync policy %q (want always, interval or never)", s)
}

const (
	headerSize = 8
	lsnSize    = 8
	// MaxRecordSize bounds a single payload. A length field larger than this
	// is treated as corruption instead of triggering a huge allocation.
	MaxRecordSize = 256 << 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt is returned when a non-final segment fails validation.
var ErrCorrupt = errors.New("wal: corrupt segment")

// ErrClosed is returned by operations on a closed log.
var ErrClosed = errors.New("wal: closed")

// Options configures a Log.
type Options struct {
	Dir          string
	Sync         SyncPolicy
	SyncInterval time.Duration
	// SegmentSize is the size at which the active segment is rotated.
	SegmentSize int64
}

// ReplayStats describes what Open found on disk.
type ReplayStats struct {
	Segments       int
	Records        int
	Replayed       int
	LastLSN        uint64
	TruncatedBytes int64
	TruncatedFile  string
}

// Log is a write-ahead log. Append, Sync, Rotate and Close are safe for
// concurrent use.
type Log struct {
	opts Options

	mu       sync.Mutex
	f        *os.File
	segFirst uint64
	segSize  int64
	nextLSN  uint64
	closed   bool
	dirty    bool
	segments []uint64 // first LSN of each segment, ascending

	// syncMu elects one goroutine at a time to fsync on behalf of every
	// writer waiting in WaitDurable (group commit). durable is the highest
	// LSN known to be on stable storage.
	syncMu  sync.Mutex
	durable atomic.Uint64

	bytesWritten atomic.Uint64
	fsyncs       atomic.Uint64
	stop         chan struct{}
	done         chan struct{}
	stopOnce     sync.Once
}

func segmentName(first uint64) string { return fmt.Sprintf("wal-%020d.log", first) }

func listSegments(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "wal-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "wal-"), ".log"), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Open validates the log in opts.Dir, truncates a torn tail, and calls apply
// for every record with LSN greater than after, in order. It then opens the
// final segment for appending. after is typically the LSN covered by the
// most recent snapshot.
func Open(opts Options, after uint64, apply func(lsn uint64, data []byte) error) (*Log, ReplayStats, error) {
	var st ReplayStats
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = 64 << 20
	}
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = 100 * time.Millisecond
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, st, err
	}
	segs, err := listSegments(opts.Dir)
	if err != nil {
		return nil, st, err
	}
	st.Segments = len(segs)
	l := &Log{opts: opts, segments: segs}

	expect := uint64(0) // next LSN expected; 0 means take it from the first record
	for i, first := range segs {
		last := i == len(segs)-1
		if expect != 0 && first != expect {
			return nil, st, fmt.Errorf("%w: segment %s starts at LSN %d, expected %d", ErrCorrupt, segmentName(first), first, expect)
		}
		if expect == 0 && first > after+1 {
			return nil, st, fmt.Errorf("%w: log starts at LSN %d but snapshot covers only up to %d", ErrCorrupt, first, after)
		}
		path := filepath.Join(opts.Dir, segmentName(first))
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, st, err
		}
		next := first
		off := 0
		for off < len(data) {
			payload, ok := parseRecord(data[off:])
			if !ok || binary.LittleEndian.Uint64(payload) != next {
				break
			}
			if next > after {
				if err := apply(next, payload[lsnSize:]); err != nil {
					return nil, st, fmt.Errorf("wal: apply LSN %d: %w", next, err)
				}
				st.Replayed++
			}
			st.Records++
			next++
			off += headerSize + len(payload)
		}
		if off < len(data) {
			if !last {
				return nil, st, fmt.Errorf("%w: %s has an invalid record at offset %d", ErrCorrupt, segmentName(first), off)
			}
			if err := truncate(path, int64(off)); err != nil {
				return nil, st, err
			}
			st.TruncatedBytes = int64(len(data) - off)
			st.TruncatedFile = path
		}
		expect = next
		if last {
			l.segSize = int64(off)
		}
	}
	if len(segs) == 0 {
		l.nextLSN = after + 1
		if err := l.openSegment(l.nextLSN); err != nil {
			return nil, st, err
		}
	} else {
		l.nextLSN = max(expect, after+1)
		if expect < after+1 {
			// The snapshot is ahead of the log (for example all segments
			// were already removed up to it). Start a fresh segment at the
			// snapshot boundary so LSNs stay continuous.
			if err := l.openSegment(l.nextLSN); err != nil {
				return nil, st, err
			}
		} else {
			first := segs[len(segs)-1]
			f, err := os.OpenFile(filepath.Join(opts.Dir, segmentName(first)), os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return nil, st, err
			}
			l.f, l.segFirst = f, first
		}
	}
	st.LastLSN = l.nextLSN - 1
	l.durable.Store(st.LastLSN)
	if opts.Sync == SyncInterval {
		l.stop = make(chan struct{})
		l.done = make(chan struct{})
		go l.syncLoop()
	}
	return l, st, nil
}

// parseRecord validates one frame at the start of b and returns its payload.
func parseRecord(b []byte) ([]byte, bool) {
	if len(b) < headerSize {
		return nil, false
	}
	n := binary.LittleEndian.Uint32(b[0:4])
	if n < lsnSize || n > MaxRecordSize || int(n) > len(b)-headerSize {
		return nil, false
	}
	payload := b[headerSize : headerSize+int(n)]
	if crc32.Checksum(payload, castagnoli) != binary.LittleEndian.Uint32(b[4:8]) {
		return nil, false
	}
	return payload, true
}

func truncate(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// SyncDir fsyncs a directory so that file creations, renames and removals
// inside it are durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

func (l *Log) openSegment(first uint64) error {
	path := filepath.Join(l.opts.Dir, segmentName(first))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := SyncDir(l.opts.Dir); err != nil {
		f.Close()
		return err
	}
	l.f, l.segFirst, l.segSize = f, first, 0
	if n := len(l.segments); n == 0 || l.segments[n-1] != first {
		l.segments = append(l.segments, first)
	}
	return nil
}

// EncodeRecord returns the framed bytes for a record. It is exported for
// tests that need to craft log files by hand.
func EncodeRecord(lsn uint64, data []byte) []byte {
	buf := make([]byte, headerSize+lsnSize+len(data))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(lsnSize+len(data)))
	binary.LittleEndian.PutUint64(buf[headerSize:], lsn)
	copy(buf[headerSize+lsnSize:], data)
	binary.LittleEndian.PutUint32(buf[4:8], crc32.Checksum(buf[headerSize:], castagnoli))
	return buf
}

// Append writes one record and returns its LSN. Under SyncAlways it returns
// only after the record is on stable storage; it is Write followed by
// WaitDurable.
func (l *Log) Append(data []byte) (uint64, error) {
	lsn, err := l.Write(data)
	if err != nil {
		return 0, err
	}
	return lsn, l.WaitDurable(lsn)
}

// Write appends one record with a single write(2) call and returns its LSN
// without waiting for fsync. Once Write returns, the record survives a
// process crash, because it is in the operating system page cache.
func (l *Log) Write(data []byte) (uint64, error) {
	if len(data)+lsnSize > MaxRecordSize {
		return 0, fmt.Errorf("wal: record of %d bytes exceeds limit", len(data))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, ErrClosed
	}
	if l.segSize >= l.opts.SegmentSize {
		if err := l.rotateLocked(); err != nil {
			return 0, err
		}
	}
	lsn := l.nextLSN
	rec := EncodeRecord(lsn, data)
	n, err := l.f.Write(rec)
	if err != nil {
		// A partial write leaves a torn record; truncate it away so later
		// appends do not follow garbage.
		if n > 0 {
			l.f.Truncate(l.segSize)
		}
		return 0, fmt.Errorf("wal: write: %w", err)
	}
	l.segSize += int64(n)
	l.bytesWritten.Add(uint64(n))
	l.nextLSN++
	l.dirty = true
	return lsn, nil
}

// WaitDurable blocks until the record with the given LSN has been fsynced.
// It is a no-op unless the policy is SyncAlways.
//
// This is group commit: the first waiter to arrive fsyncs everything written
// so far, and waiters whose records were covered by that fsync return
// without issuing their own. Writers are not blocked while an fsync is in
// progress, so the next fsync naturally covers every record written during
// the previous one.
func (l *Log) WaitDurable(lsn uint64) error {
	if l.opts.Sync != SyncAlways {
		return nil
	}
	for l.durable.Load() < lsn {
		l.syncMu.Lock()
		if l.durable.Load() >= lsn {
			l.syncMu.Unlock()
			return nil
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			l.syncMu.Unlock()
			if l.durable.Load() >= lsn {
				return nil
			}
			return ErrClosed
		}
		target, f := l.nextLSN-1, l.f
		l.mu.Unlock()
		err := f.Sync()
		if err == nil {
			l.fsyncs.Add(1)
			l.markDurable(target)
		}
		l.syncMu.Unlock()
		if err != nil && !errors.Is(err, os.ErrClosed) {
			return fmt.Errorf("wal: fsync: %w", err)
		}
		// os.ErrClosed means the segment was rotated or the log closed
		// concurrently; both fsync before closing, so loop and re-check.
	}
	return nil
}

func (l *Log) markDurable(lsn uint64) {
	for {
		cur := l.durable.Load()
		if cur >= lsn || l.durable.CompareAndSwap(cur, lsn) {
			return
		}
	}
}

// Sync flushes the active segment to stable storage.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.syncLocked()
}

func (l *Log) syncLocked() error {
	if l.closed || !l.dirty {
		return nil
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.fsyncs.Add(1)
	l.dirty = false
	l.markDurable(l.nextLSN - 1)
	return nil
}

func (l *Log) syncLoop() {
	defer close(l.done)
	t := time.NewTicker(l.opts.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.Sync()
		}
	}
}

// Rotate closes the active segment and starts a new one at the next LSN.
// It returns the last LSN contained in the closed segments.
func (l *Log) Rotate() (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, ErrClosed
	}
	if l.segSize > 0 {
		if err := l.rotateLocked(); err != nil {
			return 0, err
		}
	}
	return l.nextLSN - 1, nil
}

func (l *Log) rotateLocked() error {
	l.dirty = true
	if err := l.syncLocked(); err != nil {
		return err
	}
	if err := l.f.Close(); err != nil {
		return err
	}
	return l.openSegment(l.nextLSN)
}

// RemoveThrough deletes every segment whose records all have LSN <= lsn.
// The active segment is never removed.
func (l *Log) RemoveThrough(lsn uint64) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := 0
	keep := l.segments[:0]
	for i, first := range l.segments {
		isActive := first == l.segFirst
		if !isActive && i+1 < len(l.segments) && l.segments[i+1]-1 <= lsn {
			if err := os.Remove(filepath.Join(l.opts.Dir, segmentName(first))); err != nil && !os.IsNotExist(err) {
				keep = append(keep, l.segments[i:]...)
				l.segments = keep
				return removed, err
			}
			removed++
			continue
		}
		keep = append(keep, first)
	}
	l.segments = keep
	if removed > 0 {
		return removed, SyncDir(l.opts.Dir)
	}
	return 0, nil
}

// LastLSN returns the LSN of the most recently appended record.
func (l *Log) LastLSN() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextLSN - 1
}

// Segments returns the number of segment files.
func (l *Log) Segments() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.segments)
}

// BytesWritten returns the number of bytes appended since Open.
func (l *Log) BytesWritten() uint64 { return l.bytesWritten.Load() }

// Fsyncs returns the number of fsync calls on segment files since Open.
func (l *Log) Fsyncs() uint64 { return l.fsyncs.Load() }

// Close flushes and closes the log.
func (l *Log) Close() error {
	if l.stop != nil {
		l.stopOnce.Do(func() {
			close(l.stop)
			<-l.done
		})
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.dirty = true
	err := l.syncLocked()
	l.closed = true
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	return err
}
