// Package db manages named collections, each an HNSW index made durable by
// a write-ahead log and periodic snapshots.
//
// On-disk layout:
//
//	<data-dir>/
//	  LOCK                              advisory lock held while open
//	  collections/<name>/meta.json      immutable collection config
//	  collections/<name>/snapshot-<lsn>.snap
//	  collections/<name>/wal/wal-<first-lsn>.log
package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/adeoluo/vectordb-go/internal/wal"
)

// Errors returned by DB and Collection methods.
var (
	ErrInvalid  = errors.New("invalid argument")
	ErrNotFound = errors.New("collection not found")
	ErrExists   = errors.New("collection already exists")
	ErrClosed   = errors.New("collection is closed")
)

// Options configures a DB.
type Options struct {
	DataDir      string
	Sync         wal.SyncPolicy
	SyncInterval time.Duration
	SegmentSize  int64
	// SnapshotInterval triggers a snapshot when this much time has passed
	// since the last one and there are new writes. Zero disables it.
	SnapshotInterval time.Duration
	// SnapshotWALBytes triggers a snapshot when this many log bytes have been
	// written since the last one. Zero disables it.
	SnapshotWALBytes int64
	// SnapshotOnClose writes a final snapshot during a clean shutdown so the
	// next start does not need to replay the log.
	SnapshotOnClose bool
	Logger          *slog.Logger
}

// DB is a set of collections rooted at a data directory.
type DB struct {
	opts   Options
	root   string
	unlock func()

	mu   sync.RWMutex
	cols map[string]*Collection

	stop chan struct{}
	done chan struct{}
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Open opens (or creates) the data directory, recovers every collection,
// and starts the background snapshot loop.
func Open(opts Options) (*DB, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("%w: data directory is required", ErrInvalid)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	root := filepath.Join(opts.DataDir, "collections")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	unlock, err := lockDir(filepath.Join(opts.DataDir, "LOCK"))
	if err != nil {
		return nil, err
	}
	d := &DB{opts: opts, root: root, unlock: unlock, cols: make(map[string]*Collection)}
	entries, err := os.ReadDir(root)
	if err != nil {
		unlock()
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			// Leftover from an interrupted create or drop.
			os.RemoveAll(filepath.Join(root, e.Name()))
			continue
		}
		c, err := openCollection(filepath.Join(root, e.Name()), opts)
		if err != nil {
			d.closeAll()
			unlock()
			return nil, err
		}
		d.cols[c.name] = c
	}
	d.stop = make(chan struct{})
	d.done = make(chan struct{})
	go d.snapshotLoop()
	return d, nil
}

func (d *DB) snapshotLoop() {
	defer close(d.done)
	if d.opts.SnapshotInterval <= 0 && d.opts.SnapshotWALBytes <= 0 {
		<-d.stop
		return
	}
	tick := 250 * time.Millisecond
	if d.opts.SnapshotInterval > 0 && d.opts.SnapshotInterval < 4*tick {
		tick = max(d.opts.SnapshotInterval/4, 10*time.Millisecond)
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case now := <-t.C:
			for _, c := range d.List() {
				if c.needsSnapshot(d.opts, now) {
					if _, err := c.Snapshot(); err != nil && !errors.Is(err, ErrClosed) {
						c.logger.Error("background snapshot failed", "err", err)
					}
				}
			}
		}
	}
}

// List returns all open collections sorted by name.
func (d *DB) List() []*Collection {
	d.mu.RLock()
	out := make([]*Collection, 0, len(d.cols))
	for _, c := range d.cols {
		out = append(out, c)
	}
	d.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Collection returns the named collection.
func (d *DB) Collection(name string) (*Collection, error) {
	d.mu.RLock()
	c, ok := d.cols[name]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return c, nil
}

// CreateCollection creates a collection. The directory is assembled under a
// temporary name and renamed into place, so a crash never leaves a
// half-created collection behind.
func (d *DB) CreateCollection(name string, cfg CollectionConfig) (*Collection, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: collection name must match %s", ErrInvalid, nameRE)
	}
	if _, err := cfg.hnswConfig(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.cols[name]; ok {
		return nil, fmt.Errorf("%w: %q", ErrExists, name)
	}
	tmp := filepath.Join(d.root, ".tmp-"+name)
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	meta, _ := json.MarshalIndent(collectionMeta{Version: 1, Name: name, Config: cfg, CreatedAt: time.Now().UTC()}, "", "  ")
	if err := writeFileSync(filepath.Join(tmp, "meta.json"), meta); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	final := filepath.Join(d.root, name)
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if err := wal.SyncDir(d.root); err != nil {
		return nil, err
	}
	c, err := openCollection(final, d.opts)
	if err != nil {
		return nil, err
	}
	d.cols[name] = c
	return c, nil
}

// DropCollection closes and deletes a collection and its data.
func (d *DB) DropCollection(name string) error {
	d.mu.Lock()
	c, ok := d.cols[name]
	if ok {
		delete(d.cols, name)
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	c.Close()
	trash := filepath.Join(d.root, fmt.Sprintf(".deleted-%s-%d", name, time.Now().UnixNano()))
	if err := os.Rename(c.dir, trash); err != nil {
		return err
	}
	if err := wal.SyncDir(d.root); err != nil {
		return err
	}
	return os.RemoveAll(trash)
}

func (d *DB) closeAll() error {
	var errs []error
	for _, c := range d.cols {
		if d.opts.SnapshotOnClose {
			if _, err := c.Snapshot(); err != nil && !errors.Is(err, ErrClosed) {
				errs = append(errs, fmt.Errorf("snapshot %s: %w", c.name, err))
			}
		}
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

// Close stops background work, optionally snapshots every collection, and
// closes all logs.
func (d *DB) Close() error {
	close(d.stop)
	<-d.done
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.closeAll()
	d.cols = map[string]*Collection{}
	d.unlock()
	return err
}
