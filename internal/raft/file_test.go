package raft

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileStorage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "raft.log")
	s, err := OpenFileStorage(p)
	if err != nil {
		t.Fatal(err)
	}
	s.nofs = true
	s.SetHardState(HardState{Term: 3, Vote: 2})
	s.Append([]Entry{{Index: 1, Term: 1, Data: []byte("a")}, {Index: 2, Term: 1, Data: []byte("b")}, {Index: 3, Term: 2, Noop: true}})
	s.Append([]Entry{{Index: 3, Term: 3, Data: []byte("c")}}) // overwrites index 3
	if err := s.Append([]Entry{{Index: 9, Term: 3}}); err == nil {
		t.Fatal("a gap must be refused")
	}
	s.Close()
	size := func() int64 { fi, _ := os.Stat(p); return fi.Size() }
	good := size()
	// a torn tail: half a record
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte{40, 0, 0, 0, 1, 2, 3, 4, 2, 9})
	f.Close()
	s, err = OpenFileStorage(p)
	if err != nil {
		t.Fatal(err)
	}
	hs, _, es, _ := s.Load()
	if hs != (HardState{Term: 3, Vote: 2}) || len(es) != 3 || string(es[2].Data) != "c" || es[2].Term != 3 || string(es[1].Data) != "b" {
		t.Fatalf("reloaded %v %v", hs, es)
	}
	if size() != good {
		t.Fatalf("torn tail not cut: %d != %d", size(), good)
	}
	s.Append([]Entry{{Index: 4, Term: 3, Data: []byte("d")}})
	s.Close()
	s, _ = OpenFileStorage(p)
	if _, _, es, _ = s.Load(); len(es) != 4 {
		t.Fatalf("append after recovery lost: %d entries", len(es))
	}
	s.Close()
}
