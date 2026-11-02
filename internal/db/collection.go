package db

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adeoluo/vectordb-go/internal/hnsw"
	"github.com/adeoluo/vectordb-go/internal/wal"
)

// CollectionConfig is the user-visible configuration of a collection. It is
// fixed at creation time.
type CollectionConfig struct {
	Dim            int    `json:"dim"`
	Metric         string `json:"metric"`
	M              int    `json:"m"`
	EfConstruction int    `json:"ef_construction"`
}

type collectionMeta struct {
	Version   int              `json:"version"`
	Name      string           `json:"name"`
	Config    CollectionConfig `json:"config"`
	CreatedAt time.Time        `json:"created_at"`
}

// RecoveryInfo describes how a collection was restored at startup.
type RecoveryInfo struct {
	SnapshotLSN     uint64  `json:"snapshot_lsn"`
	SnapshotsTried  int     `json:"snapshots_tried"`
	ReplayedRecords int     `json:"replayed_records"`
	TruncatedBytes  int64   `json:"truncated_bytes"`
	DurationMS      float64 `json:"duration_ms"`
}

// Hit is one search result.
type Hit struct {
	ID       string  `json:"id"`
	Distance float32 `json:"distance"`
}

// Limits on request sizes.
const (
	MaxBatch = 10000
	MaxK     = 1000
	MaxEf    = 10000
)

const (
	snapMagic       = "VDBSNAP1"
	snapshotsToKeep = 2
)

// Collection is a named HNSW index with its own write-ahead log and
// snapshots.
//
// Writes are serialized by wmu so the order of records in the log is the
// order in which ids are assigned in the index. Only the cheap part of a
// write (log append, id allocation, id map update) runs under wmu; linking
// the new nodes into the graph, which dominates insert cost, happens after
// wmu is released and can proceed in parallel with other writes.
type Collection struct {
	name   string
	dir    string
	cfg    CollectionConfig
	logger *slog.Logger

	index *hnsw.Index
	log   *wal.Log

	wmu      sync.Mutex
	closed   bool
	failed   atomic.Pointer[error] // set when an fsync fails
	inflight sync.WaitGroup
	lastLSN  atomic.Uint64

	idsMu sync.RWMutex
	ids   map[string]uint32

	snapMu         sync.Mutex
	snapLSN        atomic.Uint64
	lastSnapAt     atomic.Int64
	walBytesAtSnap atomic.Uint64
	snapshots      atomic.Uint64

	upserts  atomic.Uint64
	deletes  atomic.Uint64
	searches atomic.Uint64

	recovery RecoveryInfo
}

func (cfg *CollectionConfig) hnswConfig() (hnsw.Config, error) {
	metric, err := hnsw.ParseMetric(cfg.Metric)
	if err != nil {
		return hnsw.Config{}, err
	}
	hc := hnsw.Config{Dim: cfg.Dim, M: cfg.M, EfConstruction: cfg.EfConstruction, Metric: metric}
	if err := hc.Validate(); err != nil {
		return hnsw.Config{}, err
	}
	cfg.Metric = metric.String()
	cfg.M = hc.M
	cfg.EfConstruction = hc.EfConstruction
	return hc, nil
}

func snapshotName(lsn uint64) string { return fmt.Sprintf("snapshot-%020d.snap", lsn) }

// listSnapshots returns snapshot LSNs in descending order.
func listSnapshots(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			os.Remove(filepath.Join(dir, name))
			continue
		}
		if !strings.HasPrefix(name, "snapshot-") || !strings.HasSuffix(name, ".snap") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "snapshot-"), ".snap"), 10, 64)
		if err == nil {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out, nil
}

func loadSnapshot(path string) (*hnsw.Index, uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(data) < len(snapMagic)+8+4 || string(data[:len(snapMagic)]) != snapMagic {
		return nil, 0, errors.New("bad snapshot header")
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)) != sum {
		return nil, 0, errors.New("snapshot checksum mismatch")
	}
	lsn := binary.LittleEndian.Uint64(body[len(snapMagic):])
	ix, err := hnsw.Read(bytes.NewReader(body[len(snapMagic)+8:]))
	if err != nil {
		return nil, 0, err
	}
	return ix, lsn, nil
}

func openCollection(dir string, opts Options) (*Collection, error) {
	start := time.Now()
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta collectionMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("%s: meta.json: %w", dir, err)
	}
	hc, err := meta.Config.hnswConfig()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	c := &Collection{
		name:   meta.Name,
		dir:    dir,
		cfg:    meta.Config,
		logger: opts.Logger.With("collection", meta.Name),
		ids:    make(map[string]uint32),
	}

	snaps, err := listSnapshots(dir)
	if err != nil {
		return nil, err
	}
	var snapLSN uint64
	for _, s := range snaps {
		c.recovery.SnapshotsTried++
		path := filepath.Join(dir, snapshotName(s))
		ix, lsn, err := loadSnapshot(path)
		if err != nil {
			// Move it aside so it is neither retried nor counted as one of
			// the retained snapshots. The log still covers the older one.
			c.logger.Warn("skipping unreadable snapshot", "snapshot", snapshotName(s), "err", err)
			os.Rename(path, path+".corrupt")
			continue
		}
		c.index, snapLSN = ix, lsn
		break
	}
	if c.index == nil {
		if c.index, err = hnsw.New(hc); err != nil {
			return nil, err
		}
	}
	c.index.ForEach(func(id uint32, key string, deleted bool) {
		if !deleted {
			c.ids[key] = id
		}
	})

	log, st, err := wal.Open(wal.Options{
		Dir:          filepath.Join(dir, "wal"),
		Sync:         opts.Sync,
		SyncInterval: opts.SyncInterval,
		SegmentSize:  opts.SegmentSize,
	}, snapLSN, func(_ uint64, data []byte) error {
		op, items, ids, err := decodeRecord(data, c.cfg.Dim)
		if err != nil {
			return err
		}
		if op == opUpsert {
			c.link(c.allocate(items))
		} else {
			c.remove(ids)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collection %s: %w", meta.Name, err)
	}
	c.log = log
	c.lastLSN.Store(st.LastLSN)
	c.snapLSN.Store(snapLSN)
	c.lastSnapAt.Store(time.Now().UnixNano())
	c.recovery.SnapshotLSN = snapLSN
	c.recovery.ReplayedRecords = st.Replayed
	c.recovery.TruncatedBytes = st.TruncatedBytes
	c.recovery.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	if st.TruncatedBytes > 0 {
		c.logger.Warn("truncated torn or corrupt WAL tail", "file", st.TruncatedFile, "bytes", st.TruncatedBytes)
	}
	c.logger.Info("collection opened", "vectors", c.index.Live(), "snapshot_lsn", snapLSN,
		"replayed", st.Replayed, "last_lsn", st.LastLSN, "duration_ms", c.recovery.DurationMS)
	return c, nil
}

// Name returns the collection name.
func (c *Collection) Name() string { return c.name }

// Config returns the collection configuration.
func (c *Collection) Config() CollectionConfig { return c.cfg }

func validVector(v []float32) bool {
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
	}
	return true
}

func validID(id string) error {
	if id == "" || len(id) > MaxIDLength {
		return fmt.Errorf("%w: id length must be in [1, %d]", ErrInvalid, MaxIDLength)
	}
	return nil
}

// allocate assigns index ids for items in order and updates the id map,
// tombstoning any previous version of each key.
func (c *Collection) allocate(items []Item) []uint32 {
	out := make([]uint32, len(items))
	for i, it := range items {
		id, err := c.index.Allocate(it.ID, it.Vector)
		if err != nil {
			// Inputs are validated before they reach the log, so this is
			// unreachable unless the log itself is inconsistent.
			panic(fmt.Sprintf("allocate %q: %v", it.ID, err))
		}
		c.idsMu.Lock()
		if old, ok := c.ids[it.ID]; ok {
			c.index.Delete(old)
		}
		c.ids[it.ID] = id
		c.idsMu.Unlock()
		out[i] = id
	}
	return out
}

// link connects allocated nodes into the graph, in parallel for batches.
func (c *Collection) link(ids []uint32) {
	workers := min(runtime.GOMAXPROCS(0), len(ids))
	if workers <= 1 {
		for _, id := range ids {
			c.index.Link(id)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= int64(len(ids)) {
					return
				}
				c.index.Link(ids[i])
			}
		}()
	}
	wg.Wait()
}

func (c *Collection) remove(ids []string) int {
	n := 0
	c.idsMu.Lock()
	defer c.idsMu.Unlock()
	for _, id := range ids {
		if nid, ok := c.ids[id]; ok {
			c.index.Delete(nid)
			delete(c.ids, id)
			n++
		}
	}
	return n
}

// Upsert durably logs a batch and then applies it. When Upsert returns
// without error the batch is in the log (and fsynced under the "always"
// policy) and visible to searches. Later items win over earlier items with
// the same id within one batch.
func (c *Collection) Upsert(items []Item) (uint64, error) {
	if len(items) == 0 || len(items) > MaxBatch {
		return 0, fmt.Errorf("%w: batch size must be in [1, %d]", ErrInvalid, MaxBatch)
	}
	last := make(map[string]int, len(items))
	for i, it := range items {
		if err := validID(it.ID); err != nil {
			return 0, err
		}
		if len(it.Vector) != c.cfg.Dim {
			return 0, fmt.Errorf("%w: vector %q has dimension %d, collection expects %d", ErrInvalid, it.ID, len(it.Vector), c.cfg.Dim)
		}
		if !validVector(it.Vector) {
			return 0, fmt.Errorf("%w: vector %q contains NaN or Inf", ErrInvalid, it.ID)
		}
		last[it.ID] = i
	}
	if len(last) != len(items) {
		dedup := make([]Item, 0, len(last))
		for i, it := range items {
			if last[it.ID] == i {
				dedup = append(dedup, it)
			}
		}
		items = dedup
	}
	payload := encodeUpsert(items, c.cfg.Dim)

	c.wmu.Lock()
	if err := c.writableLocked(); err != nil {
		c.wmu.Unlock()
		return 0, err
	}
	lsn, err := c.log.Write(payload)
	if err != nil {
		c.wmu.Unlock()
		return 0, err
	}
	ids := c.allocate(items)
	c.lastLSN.Store(lsn)
	c.inflight.Add(1)
	c.wmu.Unlock()
	defer c.inflight.Done()

	// Wait for fsync outside wmu so concurrent upserts share one fsync.
	// Allocated nodes are unreachable by searches until they are linked,
	// so nothing becomes searchable before it is durable.
	if err := c.log.WaitDurable(lsn); err != nil {
		c.fail(err)
		return 0, err
	}
	c.link(ids)
	c.upserts.Add(uint64(len(items)))
	return lsn, nil
}

// writableLocked reports why the collection cannot accept writes, if it
// cannot. wmu must be held.
func (c *Collection) writableLocked() error {
	if c.closed {
		return ErrClosed
	}
	if errp := c.failed.Load(); errp != nil {
		return fmt.Errorf("collection %s stopped accepting writes after a log failure (restart to recover): %w", c.name, *errp)
	}
	return nil
}

// fail records a durability failure. After a failed fsync the state of the
// page cache is unknown, so the only safe recovery is a restart that
// rebuilds state from what is actually on disk.
// It does not take wmu, because Snapshot may hold wmu while waiting for the
// failing write to finish.
func (c *Collection) fail(err error) {
	if c.failed.CompareAndSwap(nil, &err) {
		c.logger.Error("write-ahead log failure; rejecting further writes", "err", err)
	}
}

// Delete durably logs and applies tombstones for ids. It returns the LSN
// and how many ids were present.
func (c *Collection) Delete(ids []string) (uint64, int, error) {
	if len(ids) == 0 || len(ids) > MaxBatch {
		return 0, 0, fmt.Errorf("%w: batch size must be in [1, %d]", ErrInvalid, MaxBatch)
	}
	for _, id := range ids {
		if err := validID(id); err != nil {
			return 0, 0, err
		}
	}
	payload := encodeDelete(ids)
	// Deletes hold wmu through the fsync. They must be applied in log
	// order relative to upserts of the same id, and they are rare enough
	// that sharing fsyncs is not worth the extra bookkeeping.
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.writableLocked(); err != nil {
		return 0, 0, err
	}
	lsn, err := c.log.Write(payload)
	if err != nil {
		return 0, 0, err
	}
	if err := c.log.WaitDurable(lsn); err != nil {
		c.fail(err)
		return 0, 0, err
	}
	n := c.remove(ids)
	c.lastLSN.Store(lsn)
	c.deletes.Add(uint64(n))
	return lsn, n, nil
}

// Search returns the k approximate nearest neighbors of q.
func (c *Collection) Search(q []float32, k, ef int) ([]Hit, error) {
	if k < 1 || k > MaxK {
		return nil, fmt.Errorf("%w: k must be in [1, %d]", ErrInvalid, MaxK)
	}
	if ef < 0 || ef > MaxEf {
		return nil, fmt.Errorf("%w: ef must be in [0, %d]", ErrInvalid, MaxEf)
	}
	if ef == 0 {
		ef = max(64, k)
	}
	if len(q) != c.cfg.Dim {
		return nil, fmt.Errorf("%w: query has dimension %d, collection expects %d", ErrInvalid, len(q), c.cfg.Dim)
	}
	if !validVector(q) {
		return nil, fmt.Errorf("%w: query contains NaN or Inf", ErrInvalid)
	}
	res, err := c.index.Search(q, k, ef)
	if err != nil {
		return nil, err
	}
	c.searches.Add(1)
	hits := make([]Hit, len(res))
	for i, r := range res {
		hits[i] = Hit{ID: r.Key, Distance: r.Distance}
	}
	return hits, nil
}

// Get returns the stored vector for id. For the cosine metric the stored
// vector is the normalized input.
func (c *Collection) Get(id string) ([]float32, bool) {
	c.idsMu.RLock()
	nid, ok := c.ids[id]
	c.idsMu.RUnlock()
	if !ok {
		return nil, false
	}
	return c.index.Vector(nid), true
}

// Snapshot writes a consistent snapshot of the index and removes log
// segments that are no longer needed. It returns the LSN the snapshot
// covers. Searches continue during a snapshot; writes pause only while the
// graph is serialized into memory, not while it is written to disk.
func (c *Collection) Snapshot() (uint64, error) {
	c.snapMu.Lock()
	defer c.snapMu.Unlock()

	c.wmu.Lock()
	if c.closed {
		c.wmu.Unlock()
		return 0, ErrClosed
	}
	c.inflight.Wait()
	lsn := c.lastLSN.Load()
	if lsn == c.snapLSN.Load() {
		c.wmu.Unlock()
		return lsn, nil
	}
	if _, err := c.log.Rotate(); err != nil {
		c.wmu.Unlock()
		return 0, err
	}
	var buf bytes.Buffer
	buf.WriteString(snapMagic)
	buf.Write(binary.LittleEndian.AppendUint64(nil, lsn))
	_, err := c.index.WriteTo(&buf)
	walBytes := c.log.BytesWritten()
	c.wmu.Unlock()
	if err != nil {
		return 0, err
	}
	sum := crc32.Checksum(buf.Bytes(), crc32.MakeTable(crc32.Castagnoli))
	buf.Write(binary.LittleEndian.AppendUint32(nil, sum))

	final := filepath.Join(c.dir, snapshotName(lsn))
	if err := writeFileSync(final, buf.Bytes()); err != nil {
		return 0, err
	}
	c.snapLSN.Store(lsn)
	c.lastSnapAt.Store(time.Now().UnixNano())
	c.walBytesAtSnap.Store(walBytes)
	c.snapshots.Add(1)

	snaps, err := listSnapshots(c.dir)
	if err != nil {
		return lsn, err
	}
	for _, s := range snaps[min(snapshotsToKeep, len(snaps)):] {
		os.Remove(filepath.Join(c.dir, snapshotName(s)))
	}
	// Keep enough log to replay from the oldest retained snapshot, so a
	// damaged newest snapshot can fall back to the previous one.
	oldest := snaps[min(snapshotsToKeep, len(snaps))-1]
	if _, err := c.log.RemoveThrough(oldest); err != nil {
		return lsn, err
	}
	c.logger.Info("snapshot written", "lsn", lsn, "bytes", buf.Len(), "vectors", c.index.Live())
	return lsn, nil
}

// writeFileSync writes data to path atomically: temp file, fsync, rename,
// fsync of the parent directory.
func writeFileSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return wal.SyncDir(filepath.Dir(path))
}

// needsSnapshot reports whether the background policy should snapshot now.
func (c *Collection) needsSnapshot(opts Options, now time.Time) bool {
	if c.lastLSN.Load() == c.snapLSN.Load() {
		return false
	}
	if opts.SnapshotInterval > 0 && now.Sub(time.Unix(0, c.lastSnapAt.Load())) >= opts.SnapshotInterval {
		return true
	}
	if opts.SnapshotWALBytes > 0 && c.log.BytesWritten()-c.walBytesAtSnap.Load() >= uint64(opts.SnapshotWALBytes) {
		return true
	}
	return false
}

// Close waits for in-flight writes and closes the log.
func (c *Collection) Close() error {
	c.wmu.Lock()
	if c.closed {
		c.wmu.Unlock()
		return nil
	}
	c.closed = true
	c.inflight.Wait()
	c.wmu.Unlock()
	return c.log.Close()
}

// Stats is a point-in-time view of a collection.
type Stats struct {
	Name            string       `json:"name"`
	Dim             int          `json:"dim"`
	Metric          string       `json:"metric"`
	M               int          `json:"m"`
	EfConstruction  int          `json:"ef_construction"`
	Vectors         int          `json:"vectors"`
	Nodes           int          `json:"nodes"`
	Tombstones      int          `json:"tombstones"`
	LastLSN         uint64       `json:"last_lsn"`
	SnapshotLSN     uint64       `json:"snapshot_lsn"`
	Snapshots       uint64       `json:"snapshots_written"`
	WALSegments     int          `json:"wal_segments"`
	WALBytesWritten uint64       `json:"wal_bytes_written"`
	WALFsyncs       uint64       `json:"wal_fsyncs"`
	Upserts         uint64       `json:"upserts"`
	Deletes         uint64       `json:"deletes"`
	Searches        uint64       `json:"searches"`
	Recovery        RecoveryInfo `json:"recovery"`
}

// Stats returns current statistics.
func (c *Collection) Stats() Stats {
	nodes, live := c.index.Len(), c.index.Live()
	return Stats{
		Name:            c.name,
		Dim:             c.cfg.Dim,
		Metric:          c.cfg.Metric,
		M:               c.cfg.M,
		EfConstruction:  c.cfg.EfConstruction,
		Vectors:         live,
		Nodes:           nodes,
		Tombstones:      nodes - live,
		LastLSN:         c.lastLSN.Load(),
		SnapshotLSN:     c.snapLSN.Load(),
		Snapshots:       c.snapshots.Load(),
		WALSegments:     c.log.Segments(),
		WALBytesWritten: c.log.BytesWritten(),
		WALFsyncs:       c.log.Fsyncs(),
		Upserts:         c.upserts.Load(),
		Deletes:         c.deletes.Load(),
		Searches:        c.searches.Load(),
		Recovery:        c.recovery,
	}
}
