package db

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/adeoluo/vectordb-go/internal/dataset"
	"github.com/adeoluo/vectordb-go/internal/wal"
)

func testOpts(dir string) Options {
	return Options{DataDir: dir, Sync: wal.SyncNever}
}

func mustOpen(t *testing.T, opts Options) *DB {
	t.Helper()
	d, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func items(vecs [][]float32, prefix string, from int) []Item {
	out := make([]Item, len(vecs))
	for i, v := range vecs {
		out[i] = Item{ID: fmt.Sprintf("%s%d", prefix, from+i), Vector: v}
	}
	return out
}

// contents returns every live id with its stored vector.
func contents(c *Collection) map[string][]float32 {
	out := map[string][]float32{}
	c.idsMu.RLock()
	defer c.idsMu.RUnlock()
	for id, nid := range c.ids {
		out[id] = c.index.Vector(nid)
	}
	return out
}

func TestRecordRoundTrip(t *testing.T) {
	in := []Item{{ID: "a", Vector: []float32{1, 2, 3}}, {ID: "bb", Vector: []float32{-1, 0, 0.5}}}
	op, got, _, err := decodeRecord(encodeUpsert(in, 3), 3)
	if err != nil || op != opUpsert || !reflect.DeepEqual(got, in) {
		t.Fatalf("upsert: %v %v %v", op, got, err)
	}
	op, _, ids, err := decodeRecord(encodeDelete([]string{"x", "yz"}), 3)
	if err != nil || op != opDelete || !reflect.DeepEqual(ids, []string{"x", "yz"}) {
		t.Fatalf("delete: %v %v %v", op, ids, err)
	}
	enc := encodeUpsert(in, 3)
	for cut := 0; cut < len(enc); cut++ {
		if _, _, _, err := decodeRecord(enc[:cut], 3); err == nil {
			t.Fatalf("truncated record of %d bytes decoded", cut)
		}
	}
}

func TestCollectionLifecycle(t *testing.T) {
	dir := t.TempDir()
	d := mustOpen(t, testOpts(dir))
	if _, err := d.CreateCollection("bad name", CollectionConfig{Dim: 4}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := d.CreateCollection("c", CollectionConfig{Dim: 0}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad dim: %v", err)
	}
	c, err := d.CreateCollection("c", CollectionConfig{Dim: 4, Metric: "cosine"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateCollection("c", CollectionConfig{Dim: 4}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := c.Upsert([]Item{{ID: "a", Vector: []float32{1, 2}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong dim: %v", err)
	}
	c.Upsert([]Item{{ID: "a", Vector: []float32{1, 0, 0, 0}}, {ID: "b", Vector: []float32{0, 1, 0, 0}}})
	c.Upsert([]Item{{ID: "a", Vector: []float32{0, 0, 3, 0}}})
	hits, _ := c.Search([]float32{0, 0, 1, 0}, 1, 0)
	if len(hits) != 1 || hits[0].ID != "a" || hits[0].Distance > 1e-6 {
		t.Fatalf("hits after overwrite: %+v", hits)
	}
	if _, n, _ := c.Delete([]string{"b", "missing"}); n != 1 {
		t.Fatalf("deleted %d", n)
	}
	st := c.Stats()
	if st.Vectors != 1 || st.Tombstones != 2 || st.LastLSN != 3 || st.Metric != "cosine" || st.M != 16 {
		t.Fatalf("stats %+v", st)
	}
	d.Close()

	d = mustOpen(t, testOpts(dir))
	c, err = d.Collection("c")
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(c); len(got) != 1 || got["a"][2] != 1 {
		t.Fatalf("after reopen: %v", got)
	}
	if err := d.DropCollection("c"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Collection("c"); !errors.Is(err, ErrNotFound) {
		t.Fatal("dropped collection still present")
	}
	d.Close()
	if entries, _ := os.ReadDir(filepath.Join(dir, "collections")); len(entries) != 0 {
		t.Fatalf("drop left files: %v", entries)
	}
}

func TestDataDirLock(t *testing.T) {
	dir := t.TempDir()
	d := mustOpen(t, testOpts(dir))
	defer d.Close()
	if _, err := Open(testOpts(dir)); err == nil {
		t.Fatal("second open of the same data directory succeeded")
	}
}

// TestSnapshotReplayEquivalence checks the three ways a collection can be
// restored: snapshot only, snapshot plus log replay, and log replay only.
// Each must contain exactly the same live vectors as the original, and the
// snapshot restore must reproduce the original graph exactly, so its search
// results are identical.
func TestSnapshotReplayEquivalence(t *testing.T) {
	dir := t.TempDir()
	d := mustOpen(t, testOpts(dir))
	c, _ := d.CreateCollection("eq", CollectionConfig{Dim: 16, M: 12, EfConstruction: 100})
	base := dataset.Clustered(3000, 16, 8, 0.15, 1)
	for i := 0; i < 2000; i += 100 {
		c.Upsert(items(base[i:i+100], "v", i))
	}
	c.Delete([]string{"v1", "v2", "v3"})
	snapLSN, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Writes after the snapshot live only in the log.
	for i := 2000; i < 3000; i += 100 {
		c.Upsert(items(base[i:i+100], "v", i))
	}
	c.Upsert(items(base[:5], "v", 10)) // overwrite v10..v14
	c.Delete([]string{"v2500"})
	want := contents(c)
	queries := dataset.Clustered(50, 16, 8, 0.15, 1)
	// A final snapshot captures the exact graph; compare search results
	// against it below.
	if _, err := c.Snapshot(); err != nil {
		t.Fatal(err)
	}
	var wantHits [][]Hit
	for _, q := range queries {
		h, _ := c.Search(q, 10, 64)
		wantHits = append(wantHits, h)
	}
	d.Close()

	check := func(name string, wantReplay bool, exactGraph bool) {
		t.Helper()
		d := mustOpen(t, testOpts(dir))
		defer d.Close()
		c, _ := d.Collection("eq")
		if got := contents(c); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: restored %d vectors, want %d", name, len(got), len(want))
		}
		if wantReplay != (c.recovery.ReplayedRecords > 0) {
			t.Fatalf("%s: replayed %d records", name, c.recovery.ReplayedRecords)
		}
		recallHits := 0
		for i, q := range queries {
			h, _ := c.Search(q, 10, 64)
			if exactGraph && !reflect.DeepEqual(h, wantHits[i]) {
				t.Fatalf("%s: query %d results differ", name, i)
			}
			ids := map[string]bool{}
			for _, x := range wantHits[i] {
				ids[x.ID] = true
			}
			for _, x := range h {
				if ids[x.ID] {
					recallHits++
				}
			}
		}
		agreement := float64(recallHits) / float64(10*len(queries))
		t.Logf("%s: %d vectors, replayed %d records, agreement with original %.3f", name, len(want), c.recovery.ReplayedRecords, agreement)
		if agreement < 0.95 {
			t.Fatalf("%s: agreement %.3f", name, agreement)
		}
	}

	check("snapshot-only", false, true)

	// Remove the newest snapshot: recovery falls back to the earlier one
	// and replays the log written after it.
	snaps, _ := listSnapshots(filepath.Join(dir, "collections", "eq"))
	if len(snaps) != 2 || snaps[1] != snapLSN {
		t.Fatalf("snapshots %v, want two with older %d", snaps, snapLSN)
	}
	newest := filepath.Join(dir, "collections", "eq", snapshotName(snaps[0]))
	os.Remove(newest)
	check("snapshot+replay", true, false)

	// Remove every snapshot. Rebuilding from the log alone is not possible
	// because segments covered by the older snapshot were removed, so this
	// part uses a fresh directory with no snapshots at all.
	dir2 := t.TempDir()
	d2 := mustOpen(t, testOpts(dir2))
	c2, _ := d2.CreateCollection("eq", CollectionConfig{Dim: 16, M: 12, EfConstruction: 100})
	for i := 0; i < 3000; i += 100 {
		c2.Upsert(items(base[i:i+100], "v", i))
	}
	c2.Upsert(items(base[:5], "v", 10))
	c2.Delete([]string{"v1", "v2", "v3", "v2500"})
	want2 := contents(c2)
	d2.Close()
	d2 = mustOpen(t, Options{DataDir: dir2, Sync: wal.SyncNever})
	c2, _ = d2.Collection("eq")
	if got := contents(c2); !reflect.DeepEqual(got, want2) || !reflect.DeepEqual(want2, want) {
		t.Fatalf("replay-only restore differs: %d vs %d vs %d", len(got), len(want2), len(want))
	}
	if c2.recovery.ReplayedRecords != 32 {
		t.Fatalf("replay-only: replayed %d records", c2.recovery.ReplayedRecords)
	}
	d2.Close()
}

func TestCorruptSnapshotFallsBack(t *testing.T) {
	dir := t.TempDir()
	d := mustOpen(t, testOpts(dir))
	c, _ := d.CreateCollection("s", CollectionConfig{Dim: 8})
	base := dataset.Uniform(400, 8, 3)
	c.Upsert(items(base[:200], "v", 0))
	c.Snapshot()
	c.Upsert(items(base[200:], "v", 200))
	c.Snapshot()
	want := contents(c)
	d.Close()

	snaps, _ := listSnapshots(filepath.Join(dir, "collections", "s"))
	path := filepath.Join(dir, "collections", "s", snapshotName(snaps[0]))
	data, _ := os.ReadFile(path)
	data[len(data)/2] ^= 0xff
	os.WriteFile(path, data, 0o644)

	d = mustOpen(t, testOpts(dir))
	defer d.Close()
	c, _ = d.Collection("s")
	if got := contents(c); !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback restore has %d vectors, want %d", len(got), len(want))
	}
	if c.recovery.SnapshotsTried != 2 || c.recovery.SnapshotLSN != snaps[1] {
		t.Fatalf("recovery %+v", c.recovery)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatal("corrupt snapshot was not moved aside")
	}
}

// TestTornWALTailInCollection appends a partial record (a write that was
// never acknowledged) to a collection log and checks recovery keeps every
// acknowledged batch.
func TestTornWALTailInCollection(t *testing.T) {
	dir := t.TempDir()
	d := mustOpen(t, testOpts(dir))
	c, _ := d.CreateCollection("t", CollectionConfig{Dim: 8})
	base := dataset.Uniform(100, 8, 4)
	for i := 0; i < 100; i += 10 {
		c.Upsert(items(base[i:i+10], "v", i))
	}
	want := contents(c)
	d.Close()

	segs, _ := filepath.Glob(filepath.Join(dir, "collections", "t", "wal", "wal-*.log"))
	sort.Strings(segs)
	rec := wal.EncodeRecord(11, encodeUpsert(items(dataset.Uniform(10, 8, 5), "x", 0), 8))
	f, _ := os.OpenFile(segs[len(segs)-1], os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write(rec[:len(rec)-7])
	f.Close()

	d = mustOpen(t, testOpts(dir))
	defer d.Close()
	c, _ = d.Collection("t")
	if got := contents(c); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %d vectors, want %d", len(got), len(want))
	}
	if c.recovery.TruncatedBytes != int64(len(rec)-7) {
		t.Fatalf("recovery %+v", c.recovery)
	}
	if lsn, err := c.Upsert(items(base[:1], "after", 0)); err != nil || lsn != 11 {
		t.Fatalf("append after truncation: lsn %d err %v", lsn, err)
	}
}

// TestConcurrentWritesSearchesAndSnapshots runs under -race with writers,
// readers, deleters and background snapshots all active, then verifies the
// restored state matches.
func TestConcurrentWritesSearchesAndSnapshots(t *testing.T) {
	dir := t.TempDir()
	opts := testOpts(dir)
	opts.SnapshotInterval = 20 * time.Millisecond
	opts.SnapshotOnClose = true
	d := mustOpen(t, opts)
	c, _ := d.CreateCollection("cc", CollectionConfig{Dim: 8, M: 8, EfConstruction: 50})
	base := dataset.Uniform(4000, 8, 6)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w * 1000; i < (w+1)*1000; i += 50 {
				if _, err := c.Upsert(items(base[i:i+50], "v", i)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	var rg sync.WaitGroup
	for r := range 4 {
		rg.Add(1)
		go func() {
			defer rg.Done()
			rng := rand.New(rand.NewSource(int64(r)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				q := base[rng.Intn(len(base))]
				if _, err := c.Search(q, 5, 32); err != nil {
					t.Error(err)
				}
				c.Stats()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 4000; i += 97 {
			c.Delete([]string{fmt.Sprintf("v%d", i)})
		}
	}()
	wg.Wait()
	close(stop)
	rg.Wait()
	want := contents(c)
	if c.Stats().Snapshots == 0 {
		t.Log("no background snapshot completed during the run")
	}
	d.Close()

	d = mustOpen(t, testOpts(dir))
	defer d.Close()
	c, _ = d.Collection("cc")
	if got := contents(c); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored %d vectors, want %d", len(got), len(want))
	}
	if c.recovery.ReplayedRecords != 0 {
		t.Fatalf("snapshot on close should leave nothing to replay, replayed %d", c.recovery.ReplayedRecords)
	}
}
