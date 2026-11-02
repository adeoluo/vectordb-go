// Package dataset generates deterministic synthetic vectors, reads and
// writes the standard fvecs and ivecs formats, and computes exact nearest
// neighbor ground truth by brute force.
package dataset

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"sync"

	"github.com/adeoluo/vectordb-go/internal/hnsw"
)

// Clustered returns n vectors drawn from a mixture of Gaussian clusters, the
// same construction the Python ann-vector-search-library uses: cluster
// centers uniform in [-1, 1]^dim, points offset by N(0, spread^2) noise.
func Clustered(n, dim, clusters int, spread float64, seed int64) [][]float32 {
	rng := rand.New(rand.NewSource(seed))
	centers := make([][]float64, clusters)
	for c := range centers {
		centers[c] = make([]float64, dim)
		for j := range centers[c] {
			centers[c][j] = rng.Float64()*2 - 1
		}
	}
	out := make([][]float32, n)
	for i := range out {
		c := centers[rng.Intn(clusters)]
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(c[j] + rng.NormFloat64()*spread)
		}
		out[i] = v
	}
	return out
}

// Uniform returns n vectors with components uniform in [-1, 1].
func Uniform(n, dim int, seed int64) [][]float32 {
	rng := rand.New(rand.NewSource(seed))
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()*2 - 1
		}
		out[i] = v
	}
	return out
}

// Exact returns, for each query, the indices of the k nearest base vectors
// under metric m, computed by a parallel brute-force scan.
func Exact(base, queries [][]float32, k int, m hnsw.Metric) [][]int32 {
	out := make([][]int32, len(queries))
	next := make(chan int, len(queries))
	for i := range queries {
		next <- i
	}
	close(next)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			type pair struct {
				d float32
				i int32
			}
			all := make([]pair, len(base))
			for qi := range next {
				q := queries[qi]
				for i, b := range base {
					all[i] = pair{hnsw.Distance(m, q, b), int32(i)}
				}
				sort.Slice(all, func(a, b int) bool {
					if all[a].d != all[b].d {
						return all[a].d < all[b].d
					}
					return all[a].i < all[b].i
				})
				res := make([]int32, min(k, len(all)))
				for j := range res {
					res[j] = all[j].i
				}
				out[qi] = res
			}
		}()
	}
	wg.Wait()
	return out
}

// Recall returns the mean fraction of the true top-k found in predicted.
func Recall(predicted, truth [][]int32, k int) float64 {
	if len(truth) == 0 {
		return 0
	}
	hits := 0
	for i := range truth {
		want := make(map[int32]struct{}, k)
		for _, t := range truth[i][:min(k, len(truth[i]))] {
			want[t] = struct{}{}
		}
		for _, p := range predicted[i][:min(k, len(predicted[i]))] {
			if _, ok := want[p]; ok {
				hits++
			}
		}
	}
	return float64(hits) / float64(k*len(truth))
}

// WriteFvecs writes vectors in fvecs format: for each vector, a little
// endian int32 dimension followed by that many float32 values.
func WriteFvecs(path string, vecs [][]float32) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	var b [4]byte
	for _, v := range vecs {
		binary.LittleEndian.PutUint32(b[:], uint32(len(v)))
		w.Write(b[:])
		for _, x := range v {
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(x))
			w.Write(b[:])
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadFvecs reads an fvecs file.
func ReadFvecs(path string) ([][]float32, error) {
	rows, err := readVecs(path)
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(rows))
	for i, r := range rows {
		v := make([]float32, len(r))
		for j, u := range r {
			v[j] = math.Float32frombits(u)
		}
		out[i] = v
	}
	return out, nil
}

// ReadIvecs reads an ivecs file (same layout as fvecs with int32 values).
func ReadIvecs(path string) ([][]int32, error) {
	rows, err := readVecs(path)
	if err != nil {
		return nil, err
	}
	out := make([][]int32, len(rows))
	for i, r := range rows {
		v := make([]int32, len(r))
		for j, u := range r {
			v[j] = int32(u)
		}
		out[i] = v
	}
	return out, nil
}

func readVecs(path string) ([][]uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var out [][]uint32
	var b [4]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		dim := binary.LittleEndian.Uint32(b[:])
		if dim == 0 || dim > 1<<16 {
			return nil, fmt.Errorf("%s: bad dimension %d", path, dim)
		}
		raw := make([]byte, 4*dim)
		if _, err := io.ReadFull(r, raw); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		row := make([]uint32, dim)
		for j := range row {
			row[j] = binary.LittleEndian.Uint32(raw[4*j:])
		}
		out = append(out, row)
	}
}
