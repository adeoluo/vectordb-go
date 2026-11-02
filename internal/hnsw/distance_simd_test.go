package hnsw

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func randVec(r *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

func refL2(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}

func refDot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// The dispatched kernels must agree with a float64 reference for every length,
// including lengths below, at, and around multiples of 16.
func TestKernelsMatchReference(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for n := 0; n <= 300; n++ {
		for trial := 0; trial < 5; trial++ {
			a, b := randVec(r, n), randVec(r, n)
			wantL2, wantDot := refL2(a, b), refDot(a, b)
			tol := 1e-5 * math.Max(1, math.Abs(wantL2))
			if got := float64(l2sq(a, b)); math.Abs(got-wantL2) > tol {
				t.Fatalf("l2sq n=%d: got %v want %v", n, got, wantL2)
			}
			tol = 1e-5 * math.Max(1, refDot(a, a)+refDot(b, b))
			if got := float64(dot(a, b)); math.Abs(got-wantDot) > tol {
				t.Fatalf("dot n=%d: got %v want %v", n, got, wantDot)
			}
		}
	}
}

// Slices that start at an unaligned offset inside a larger array must work.
func TestKernelsUnalignedSlices(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	buf := randVec(r, 1024)
	for off := 0; off < 8; off++ {
		a, b := buf[off:off+128], buf[512+off:512+off+128]
		if got, want := float64(l2sq(a, b)), refL2(a, b); math.Abs(got-want) > 1e-4*want {
			t.Fatalf("offset %d: got %v want %v", off, got, want)
		}
	}
}

func BenchmarkL2(b *testing.B) {
	for _, dim := range []int{16, 32, 128, 768} {
		r := rand.New(rand.NewSource(3))
		x, y := randVec(r, dim), randVec(r, dim)
		b.Run(fmt.Sprintf("neon/dim=%d", dim), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sink += l2sq(x, y)
			}
		})
		b.Run(fmt.Sprintf("generic/dim=%d", dim), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sink += l2sqGeneric(x, y)
			}
		})
	}
}

var sink float32
