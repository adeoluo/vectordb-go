package hnsw

//go:noescape
func l2sqNEON(a, b *float32, n int) float32

//go:noescape
func dotNEON(a, b *float32, n int) float32

// neonMin is the shortest vector for which the assembly call pays for itself
// (measured with BenchmarkL2: slower at 16 dimensions, faster from 32 up).
const neonMin = 32

// l2sq uses the NEON kernel for the largest multiple of 16 elements and the
// portable loop for the remainder.
func l2sq(a, b []float32) float32 {
	n := len(a)
	b = b[:n]
	if n < neonMin {
		return l2sqGeneric(a, b)
	}
	m := n &^ 15
	s := l2sqNEON(&a[0], &b[0], m)
	if m < n {
		s += l2sqGeneric(a[m:], b[m:])
	}
	return s
}

// dot uses the NEON kernel for the largest multiple of 16 elements and the
// portable loop for the remainder.
func dot(a, b []float32) float32 {
	n := len(a)
	b = b[:n]
	if n < neonMin {
		return dotGeneric(a, b)
	}
	m := n &^ 15
	s := dotNEON(&a[0], &b[0], m)
	if m < n {
		s += dotGeneric(a[m:], b[m:])
	}
	return s
}
