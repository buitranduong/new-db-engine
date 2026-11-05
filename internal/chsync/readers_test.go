package chsync

import "testing"

func TestReadersFloor(t *testing.T) {
	var rs Readers
	if x := rs.floor(100); x != 100 {
		t.Fatalf("no readers: floor %d, want 100", x)
	}
	if !rs.Acquire(150) || !rs.Acquire(120) || !rs.Acquire(120) {
		t.Fatal("reads at or above the consolidated LSN refused")
	}
	if x := rs.floor(200); x != 120 {
		t.Fatalf("floor %d, want the oldest read 120", x)
	}
	rs.Release(120)
	if x := rs.floor(200); x != 120 {
		t.Fatalf("one of two reads at 120 left: floor %d, want 120", x)
	}
	rs.Release(120)
	if x := rs.floor(200); x != 150 {
		t.Fatalf("floor %d, want 150", x)
	}
	if rs.Acquire(149) {
		t.Fatal("a read below a consolidation that started at 150 was allowed")
	}
	rs.Release(150)
	if x := rs.floor(300); x != 300 {
		t.Fatalf("floor %d, want 300", x)
	}
}
