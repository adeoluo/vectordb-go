package hnsw

import (
	"fmt"
	"math"
	"strings"
)

// Metric selects the distance function used by an index.
type Metric uint8

const (
	// L2 is Euclidean distance. Internally the index compares squared
	// distances, and search results report the square root.
	L2 Metric = iota
	// Cosine is cosine distance (1 - cosine similarity). Vectors are
	// normalized on insert so the comparison reduces to a dot product.
	Cosine
)

// String returns the canonical lowercase name of the metric.
func (m Metric) String() string {
	switch m {
	case L2:
		return "l2"
	case Cosine:
		return "cosine"
	default:
		return fmt.Sprintf("metric(%d)", m)
	}
}

// ParseMetric parses "l2" (or "euclidean") and "cosine".
func ParseMetric(s string) (Metric, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "l2", "euclidean":
		return L2, nil
	case "cosine":
		return Cosine, nil
	default:
		return 0, fmt.Errorf("unknown metric %q (want l2 or cosine)", s)
	}
}

// l2sq returns the squared Euclidean distance between a and b. The loop is
// unrolled by four, which lets the compiler keep four independent
// accumulators in registers and roughly doubles throughput on short vectors.
func l2sq(a, b []float32) float32 {
	n := len(a)
	b = b[:n]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= n; i += 4 {
		d0 := a[i] - b[i]
		d1 := a[i+1] - b[i+1]
		d2 := a[i+2] - b[i+2]
		d3 := a[i+3] - b[i+3]
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
	}
	for ; i < n; i++ {
		d := a[i] - b[i]
		s0 += d * d
	}
	return (s0 + s1) + (s2 + s3)
}

func dot(a, b []float32) float32 {
	n := len(a)
	b = b[:n]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= n; i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < n; i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}

// cosineDist assumes both inputs are unit length (or zero).
func cosineDist(a, b []float32) float32 {
	return 1 - dot(a, b)
}

// normalize scales v to unit length in place. Zero vectors are left as is.
func normalize(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
}

// Distance computes the user-facing distance between two raw vectors under
// metric m. It is used for exact (brute force) search and in tests.
func Distance(m Metric, a, b []float32) float32 {
	switch m {
	case Cosine:
		na := append([]float32(nil), a...)
		nb := append([]float32(nil), b...)
		normalize(na)
		normalize(nb)
		return cosineDist(na, nb)
	default:
		return float32(math.Sqrt(float64(l2sq(a, b))))
	}
}
