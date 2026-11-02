//go:build !unix

package db

// lockDir is a no-op on platforms without flock.
func lockDir(string) (func(), error) { return func() {}, nil }
