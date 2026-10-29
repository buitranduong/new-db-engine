package core

import (
	"testing"
	"unsafe"

	"stask/internal/wal"
)

// One Version and one wal.Loc pair are kept per key: growing them past a
// Go size class costs every key (96 instead of 64 bytes at 20M keys is
// 640MB).
func TestVersionSize(t *testing.T) {
	if s := unsafe.Sizeof(wal.Loc{}); s != 12 {
		t.Fatalf("wal.Loc is %d bytes, want 12", s)
	}
	if s := unsafe.Sizeof(Version{}); s > 64 {
		t.Fatalf("Version is %d bytes, want at most 64", s)
	}
	// the index keeps one entry per key, in leaves that fill a size class
	if s := unsafe.Sizeof(entry{}); s != 40 {
		t.Fatalf("index entry is %d bytes, want 40", s)
	}
	if s := unsafe.Sizeof(leafNode{}); s > 2688 {
		t.Fatalf("leaf is %d bytes, want at most 2688", s)
	}
}
