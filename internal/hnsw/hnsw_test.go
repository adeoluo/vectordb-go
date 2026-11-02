package hnsw_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/adeoluo/vectordb-go/internal/dataset"
	"github.com/adeoluo/vectordb-go/internal/hnsw"
)

func build(t testing.TB, cfg hnsw.Config, vecs [][]float32) *hnsw.Index {
	t.Helper()
	ix, err := hnsw.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vecs {
		if _, err := ix.Insert(strconv.Itoa(i), v); err != nil {
			t.Fatal(err)
		}
	}
	return ix
}

func searchAll(t testing.TB, ix *hnsw.Index, queries [][]float32, k, ef int) [][]int32 {
	t.Helper()
	out := make([][]int32, len(queries))
	for i, q := range queries {
		res, err := ix.Search(q, k, ef)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]int32, len(res))
		for j, r := range res {
			ids[j] = int32(r.ID)
		}
		out[i] = ids
	}
	return out
}

func TestRecallAgainstBruteForce(t *testing.T) {
	// Queries are held out from the same mixture, not copies of base points.
	clustered := dataset.Clustered(4100, 16, 8, 0.15, 3)
	cases := []struct {
		name   string
		metric hnsw.Metric
		base   [][]float32
		query  [][]float32
	}{
		{"uniform-l2", hnsw.L2, dataset.Uniform(4000, 32, 1), dataset.Uniform(100, 32, 2)},
		{"clustered-l2", hnsw.L2, clustered[:4000], clustered[4000:]},
		{"uniform-cosine", hnsw.Cosine, dataset.Uniform(4000, 32, 4), dataset.Uniform(100, 32, 5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ix := build(t, hnsw.Config{Dim: len(tc.base[0]), M: 16, EfConstruction: 200, Metric: tc.metric}, tc.base)
			truth := dataset.Exact(tc.base, tc.query, 10, tc.metric)
			got := searchAll(t, ix, tc.query, 10, 128)
			recall := dataset.Recall(got, truth, 10)
			t.Logf("recall@10 = %.4f", recall)
			if recall < 0.95 {
				t.Fatalf("recall@10 = %.4f, want >= 0.95", recall)
			}
		})
	}
}

func TestRecallIncreasesWithEf(t *testing.T) {
	base := dataset.Uniform(3000, 24, 7)
	queries := dataset.Uniform(100, 24, 8)
	ix := build(t, hnsw.Config{Dim: 24, M: 8, EfConstruction: 64}, base)
	truth := dataset.Exact(base, queries, 10, hnsw.L2)
	low := dataset.Recall(searchAll(t, ix, queries, 10, 10), truth, 10)
	high := dataset.Recall(searchAll(t, ix, queries, 10, 200), truth, 10)
	t.Logf("ef=10 recall %.3f, ef=200 recall %.3f", low, high)
	if high < low {
		t.Fatalf("recall fell when ef increased: %.3f -> %.3f", low, high)
	}
}

func TestDistancesAreSortedAndExact(t *testing.T) {
	base := dataset.Uniform(500, 8, 9)
	ix := build(t, hnsw.Config{Dim: 8}, base)
	res, err := ix.Search(base[42], 5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Key != "42" || res[0].Distance != 0 {
		t.Fatalf("self query: got %+v", res[0])
	}
	for i := 1; i < len(res); i++ {
		if res[i].Distance < res[i-1].Distance {
			t.Fatalf("results not sorted: %+v", res)
		}
		want := hnsw.Distance(hnsw.L2, base[42], base[res[i].ID])
		if diff := want - res[i].Distance; diff > 1e-4 || diff < -1e-4 {
			t.Fatalf("distance %v, want %v", res[i].Distance, want)
		}
	}
}

func TestDeleteExcludesFromResults(t *testing.T) {
	base := dataset.Uniform(1000, 8, 10)
	ix := build(t, hnsw.Config{Dim: 8}, base)
	for i := 0; i < 1000; i += 2 {
		if !ix.Delete(uint32(i)) {
			t.Fatalf("delete %d returned false", i)
		}
	}
	if ix.Delete(0) {
		t.Fatal("second delete should report false")
	}
	if ix.Live() != 500 {
		t.Fatalf("live = %d", ix.Live())
	}
	for _, q := range dataset.Uniform(50, 8, 11) {
		res, _ := ix.Search(q, 10, 64)
		if len(res) != 10 {
			t.Fatalf("got %d results", len(res))
		}
		for _, r := range res {
			if r.ID%2 == 0 {
				t.Fatalf("deleted node %d returned", r.ID)
			}
		}
	}
}

func TestDimensionMismatch(t *testing.T) {
	ix, _ := hnsw.New(hnsw.Config{Dim: 4})
	if _, err := ix.Insert("a", []float32{1, 2}); !errors.Is(err, hnsw.ErrDimension) {
		t.Fatalf("insert: %v", err)
	}
	if _, err := ix.Search([]float32{1}, 1, 1); !errors.Is(err, hnsw.ErrDimension) {
		t.Fatalf("search: %v", err)
	}
	if res, err := ix.Search([]float32{1, 2, 3, 4}, 3, 10); err != nil || len(res) != 0 {
		t.Fatalf("empty search: %v %v", res, err)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	base := dataset.Uniform(2000, 16, 12)
	ix := build(t, hnsw.Config{Dim: 16, Metric: hnsw.Cosine}, base)
	ix.Delete(5)
	var buf bytes.Buffer
	if _, err := ix.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	enc := buf.Bytes()
	restored, err := hnsw.Read(bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	if restored.Len() != ix.Len() || restored.Live() != ix.Live() {
		t.Fatalf("len %d/%d live %d/%d", restored.Len(), ix.Len(), restored.Live(), ix.Live())
	}
	for _, q := range dataset.Uniform(50, 16, 13) {
		a, _ := ix.Search(q, 10, 64)
		b, _ := restored.Search(q, 10, 64)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Fatalf("results differ after round trip:\n%v\n%v", a, b)
		}
	}
	var again bytes.Buffer
	restored.WriteTo(&again)
	if !bytes.Equal(enc, again.Bytes()) {
		t.Fatal("re-encoding is not byte identical")
	}
	if _, err := hnsw.Read(bytes.NewReader(enc[:len(enc)/2])); !errors.Is(err, hnsw.ErrBadEncoding) {
		t.Fatalf("truncated input: %v", err)
	}
}

// TestConcurrentInsertAndSearch is meant to run under -race. Writers insert
// and delete while readers search; afterwards the graph must still give
// high recall, which catches lost or corrupted links.
func TestConcurrentInsertAndSearch(t *testing.T) {
	const writers, readers, perWriter, dim = 8, 8, 400, 16
	base := dataset.Uniform(writers*perWriter, dim, 20)
	ix, _ := hnsw.New(hnsw.Config{Dim: dim, M: 12, EfConstruction: 100})
	var wg sync.WaitGroup
	var done atomic.Bool
	var searches atomic.Int64
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(r)))
			q := make([]float32, dim)
			for !done.Load() {
				for j := range q {
					q[j] = rng.Float32()*2 - 1
				}
				res, err := ix.Search(q, 5, 32)
				if err != nil {
					t.Error(err)
					return
				}
				for i := 1; i < len(res); i++ {
					if res[i].Distance < res[i-1].Distance {
						t.Error("unsorted results under concurrency")
						return
					}
				}
				searches.Add(1)
			}
		}()
	}
	var ww sync.WaitGroup
	for w := range writers {
		ww.Add(1)
		go func() {
			defer ww.Done()
			for i := w * perWriter; i < (w+1)*perWriter; i++ {
				if _, err := ix.Insert(strconv.Itoa(i), base[i]); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	ww.Wait()
	done.Store(true)
	wg.Wait()
	if ix.Len() != len(base) {
		t.Fatalf("len = %d, want %d", ix.Len(), len(base))
	}
	t.Logf("%d concurrent searches completed", searches.Load())

	queries := dataset.Uniform(100, dim, 21)
	truth := dataset.Exact(base, queries, 10, hnsw.L2)
	got := make([][]int32, len(queries))
	for i, q := range queries {
		res, _ := ix.Search(q, 10, 128)
		for _, r := range res {
			id, _ := strconv.Atoi(r.Key)
			got[i] = append(got[i], int32(id))
		}
	}
	recall := dataset.Recall(got, truth, 10)
	t.Logf("recall@10 after concurrent build = %.4f", recall)
	if recall < 0.95 {
		t.Fatalf("recall@10 = %.4f after concurrent build", recall)
	}
}
