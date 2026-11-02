package hnsw_test

import (
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/adeoluo/vectordb-go/internal/dataset"
	"github.com/adeoluo/vectordb-go/internal/hnsw"
)

// BenchmarkInsert measures single-goroutine insertion into a growing index
// of clustered dim-32 vectors with M=16, efConstruction=200.
func BenchmarkInsert(b *testing.B) {
	vecs := dataset.Clustered(b.N, 32, 8, 0.15, 1)
	ix, _ := hnsw.New(hnsw.Config{Dim: 32, M: 16, EfConstruction: 200})
	b.ResetTimer()
	for i := range b.N {
		ix.Insert(strconv.Itoa(i), vecs[i])
	}
}

// BenchmarkInsertParallel measures insertion throughput with GOMAXPROCS
// goroutines inserting concurrently.
func BenchmarkInsertParallel(b *testing.B) {
	vecs := dataset.Clustered(b.N, 32, 8, 0.15, 1)
	ix, _ := hnsw.New(hnsw.Config{Dim: 32, M: 16, EfConstruction: 200})
	var next atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := next.Add(1) - 1
			ix.Insert(strconv.Itoa(int(i)), vecs[i])
		}
	})
}

var benchIndex *hnsw.Index

func searchIndex(b *testing.B) *hnsw.Index {
	if benchIndex == nil {
		vecs := dataset.Clustered(10000, 32, 8, 0.15, 1)
		ix, _ := hnsw.New(hnsw.Config{Dim: 32, M: 16, EfConstruction: 200})
		for i, v := range vecs {
			ix.Insert(strconv.Itoa(i), v)
		}
		benchIndex = ix
	}
	return benchIndex
}

// BenchmarkSearch measures k=10, ef=64 queries against a 10,000 vector,
// dim-32 index from one goroutine.
func BenchmarkSearch(b *testing.B) {
	ix := searchIndex(b)
	queries := dataset.Clustered(1000, 32, 8, 0.15, 2)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		ix.Search(queries[i%len(queries)], 10, 64)
	}
}

// BenchmarkSearchParallel measures the same query load from GOMAXPROCS
// goroutines, exercising the lock-free read path.
func BenchmarkSearchParallel(b *testing.B) {
	ix := searchIndex(b)
	queries := dataset.Clustered(1000, 32, 8, 0.15, 2)
	var next atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := next.Add(1)
			ix.Search(queries[int(i)%len(queries)], 10, 64)
		}
	})
}
