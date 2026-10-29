//go:build unix

package pindex

import "syscall"

// allocArena returns n bytes of anonymous memory outside the Go heap. The
// buffer pool is the largest object of the process; kept on the heap it would
// raise the GC's target (GOGC x live heap) and, with staskd's GOGC=400, let
// RSS reach several times the configured pool size.
func allocArena(n int) ([]byte, func()) {
	b, err := syscall.Mmap(-1, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return make([]byte, n), func() {}
	}
	return b, func() { syscall.Munmap(b) }
}
