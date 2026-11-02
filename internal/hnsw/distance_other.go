//go:build !arm64

package hnsw

func l2sq(a, b []float32) float32 { return l2sqGeneric(a, b) }

func dot(a, b []float32) float32 { return dotGeneric(a, b) }
