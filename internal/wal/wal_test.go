package wal_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/adeoluo/vectordb-go/internal/wal"
)

type rec struct {
	lsn  uint64
	data string
}

func open(t *testing.T, dir string, after uint64, opts ...func(*wal.Options)) (*wal.Log, wal.ReplayStats, []rec) {
	t.Helper()
	o := wal.Options{Dir: dir, Sync: wal.SyncNever}
	for _, f := range opts {
		f(&o)
	}
	var got []rec
	l, st, err := wal.Open(o, after, func(lsn uint64, data []byte) error {
		got = append(got, rec{lsn, string(data)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return l, st, got
}

func appendN(t *testing.T, l *wal.Log, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if _, err := l.Append([]byte(fmt.Sprintf("record-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
}

func segments(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "wal-*.log"))
	sort.Strings(m)
	return m
}

func TestAppendAndReplay(t *testing.T) {
	dir := t.TempDir()
	l, _, got := open(t, dir, 0)
	if len(got) != 0 {
		t.Fatal("fresh log replayed records")
	}
	appendN(t, l, 0, 100)
	if l.LastLSN() != 100 {
		t.Fatalf("last lsn %d", l.LastLSN())
	}
	l.Close()

	l, st, got := open(t, dir, 0)
	defer l.Close()
	if len(got) != 100 || st.Records != 100 || st.TruncatedBytes != 0 {
		t.Fatalf("replayed %d, stats %+v", len(got), st)
	}
	for i, r := range got {
		if r.lsn != uint64(i+1) || r.data != fmt.Sprintf("record-%d", i) {
			t.Fatalf("record %d = %+v", i, r)
		}
	}
	if lsn, _ := l.Append([]byte("next")); lsn != 101 {
		t.Fatalf("next lsn %d", lsn)
	}
}

func TestReplaySkipsRecordsCoveredBySnapshot(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := open(t, dir, 0)
	appendN(t, l, 0, 10)
	l.Close()
	l, st, got := open(t, dir, 6)
	defer l.Close()
	if len(got) != 4 || got[0].lsn != 7 || st.Records != 10 || st.Replayed != 4 {
		t.Fatalf("got %+v stats %+v", got, st)
	}
}

// TestTornTailIsTruncated simulates a crash in the middle of write(2): the
// last record is only partly on disk. Recovery must keep every complete
// record, drop the partial one, and leave the file ready for appends.
func TestTornTailIsTruncated(t *testing.T) {
	for _, cut := range []int{1, 4, 8, 12, 20} {
		t.Run(fmt.Sprintf("cut-%d", cut), func(t *testing.T) {
			dir := t.TempDir()
			l, _, _ := open(t, dir, 0)
			appendN(t, l, 0, 5)
			l.Close()
			seg := segments(t, dir)[0]
			good, _ := os.ReadFile(seg)
			partial := wal.EncodeRecord(6, []byte("this record was never acknowledged"))
			os.WriteFile(seg, append(append([]byte{}, good...), partial[:cut]...), 0o644)

			l, st, got := open(t, dir, 0)
			if len(got) != 5 || st.TruncatedBytes != int64(cut) {
				t.Fatalf("replayed %d, stats %+v", len(got), st)
			}
			after, _ := os.ReadFile(seg)
			if !bytes.Equal(after, good) {
				t.Fatal("segment was not truncated back to the last valid record")
			}
			appendN(t, l, 5, 7)
			l.Close()
			l, _, got = open(t, dir, 0)
			l.Close()
			if len(got) != 7 || got[6].lsn != 7 {
				t.Fatalf("after re-append: %+v", got)
			}
		})
	}
}

// TestChecksumCorruption flips one bit in the payload of the last record.
// The CRC must catch it; the earlier records must survive.
func TestChecksumCorruption(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := open(t, dir, 0)
	appendN(t, l, 0, 10)
	l.Close()
	seg := segments(t, dir)[0]
	data, _ := os.ReadFile(seg)
	data[len(data)-3] ^= 0x10
	os.WriteFile(seg, data, 0o644)

	l, st, got := open(t, dir, 0)
	defer l.Close()
	if len(got) != 9 || st.TruncatedBytes == 0 {
		t.Fatalf("replayed %d records, stats %+v", len(got), st)
	}
	for i, r := range got {
		if r.data != fmt.Sprintf("record-%d", i) {
			t.Fatalf("record %d corrupted: %q", i, r.data)
		}
	}
}

func TestCorruptLengthFieldDoesNotAllocate(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := open(t, dir, 0)
	appendN(t, l, 0, 3)
	l.Close()
	seg := segments(t, dir)[0]
	f, _ := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write([]byte{0xff, 0xff, 0xff, 0x7f, 0, 0, 0, 0, 1, 2, 3})
	f.Close()
	l, st, got := open(t, dir, 0)
	l.Close()
	if len(got) != 3 || st.TruncatedBytes != 11 {
		t.Fatalf("got %d, stats %+v", len(got), st)
	}
}

func TestCorruptionInEarlierSegmentIsAnError(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := open(t, dir, 0)
	appendN(t, l, 0, 5)
	l.Rotate()
	appendN(t, l, 5, 10)
	l.Close()
	segs := segments(t, dir)
	if len(segs) != 2 {
		t.Fatalf("segments: %v", segs)
	}
	data, _ := os.ReadFile(segs[0])
	data[20] ^= 0xff
	os.WriteFile(segs[0], data, 0o644)
	_, _, err := wal.Open(wal.Options{Dir: dir}, 0, func(uint64, []byte) error { return nil })
	if !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt, got %v", err)
	}
}

func TestRotationAndRemoval(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := open(t, dir, 0, func(o *wal.Options) { o.SegmentSize = 200 })
	appendN(t, l, 0, 50)
	n := len(segments(t, dir))
	if n < 3 {
		t.Fatalf("expected rotation, have %d segments", n)
	}
	last, _ := l.Rotate()
	if last != 50 {
		t.Fatalf("rotate returned %d", last)
	}
	removed, err := l.RemoveThrough(30)
	if err != nil || removed == 0 {
		t.Fatalf("removed %d err %v", removed, err)
	}
	appendN(t, l, 50, 55)
	l.Close()

	l, _, got := open(t, dir, 30)
	defer l.Close()
	if len(got) != 25 || got[0].lsn != 31 || got[len(got)-1].lsn != 55 {
		t.Fatalf("after removal replayed %d records starting %d", len(got), got[0].lsn)
	}
}

func TestSnapshotAheadOfLog(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := open(t, dir, 0)
	appendN(t, l, 0, 5)
	l.Rotate()
	l.RemoveThrough(5)
	l.Close()
	l, st, got := open(t, dir, 5)
	defer l.Close()
	if len(got) != 0 || st.LastLSN != 5 {
		t.Fatalf("got %d, stats %+v", len(got), st)
	}
	if lsn, _ := l.Append([]byte("x")); lsn != 6 {
		t.Fatalf("lsn %d", lsn)
	}
}

func TestMissingSegmentIsAnError(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := open(t, dir, 0)
	appendN(t, l, 0, 5)
	l.Rotate()
	appendN(t, l, 5, 10)
	l.Close()
	os.Remove(segments(t, dir)[0])
	_, _, err := wal.Open(wal.Options{Dir: dir}, 0, func(uint64, []byte) error { return nil })
	if !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt for a gap, got %v", err)
	}
}

func TestSyncPolicies(t *testing.T) {
	for _, p := range []wal.SyncPolicy{wal.SyncAlways, wal.SyncInterval, wal.SyncNever} {
		t.Run(p.String(), func(t *testing.T) {
			parsed, err := wal.ParseSyncPolicy(p.String())
			if err != nil || parsed != p {
				t.Fatal(err)
			}
			dir := t.TempDir()
			l, _, _ := open(t, dir, 0, func(o *wal.Options) { o.Sync = p })
			appendN(t, l, 0, 10)
			if p == wal.SyncAlways && l.Fsyncs() != 10 {
				t.Fatalf("fsyncs = %d", l.Fsyncs())
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Append(nil); !errors.Is(err, wal.ErrClosed) {
				t.Fatalf("append after close: %v", err)
			}
		})
	}
}

func BenchmarkAppend(b *testing.B) {
	payload := bytes.Repeat([]byte{7}, 256)
	for _, p := range []wal.SyncPolicy{wal.SyncNever, wal.SyncInterval, wal.SyncAlways} {
		b.Run(p.String(), func(b *testing.B) {
			l, _, err := wal.Open(wal.Options{Dir: b.TempDir(), Sync: p}, 0, func(uint64, []byte) error { return nil })
			if err != nil {
				b.Fatal(err)
			}
			defer l.Close()
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for range b.N {
				if _, err := l.Append(payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAppendParallel shows group commit: with the "always" policy,
// concurrent appenders share fsyncs, so throughput scales with writers
// even though each Append still returns only after its record is durable.
func BenchmarkAppendParallel(b *testing.B) {
	payload := bytes.Repeat([]byte{7}, 256)
	l, _, err := wal.Open(wal.Options{Dir: b.TempDir(), Sync: wal.SyncAlways}, 0, func(uint64, []byte) error { return nil })
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	b.SetParallelism(8)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := l.Append(payload); err != nil {
				b.Error(err)
				return
			}
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(b.N)/float64(l.Fsyncs()), "appends/fsync")
}

func TestGroupCommitDurability(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := open(t, dir, 0, func(o *wal.Options) { o.Sync = wal.SyncAlways; o.SegmentSize = 4096 })
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				if _, err := l.Append([]byte(fmt.Sprintf("w%d-%d", w, i))); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("400 appends, %d fsyncs, %d segments", l.Fsyncs(), l.Segments())
	if l.Fsyncs() >= 400 {
		t.Errorf("expected concurrent appends to share fsyncs, got %d", l.Fsyncs())
	}
	l.Close()
	l, _, got := open(t, dir, 0)
	defer l.Close()
	if len(got) != 400 {
		t.Fatalf("replayed %d", len(got))
	}
}
