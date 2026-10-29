//go:build !unix

package pindex

func allocArena(n int) ([]byte, func()) { return make([]byte, n), func() {} }
