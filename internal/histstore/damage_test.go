package histstore

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openFileOf(t *testing.T, dir string) string {
	ents, _ := os.ReadDir(dir)
	for _, de := range ents {
		if strings.HasPrefix(de.Name(), "open-") && !strings.Contains(de.Name(), "damaged") {
			return filepath.Join(dir, de.Name())
		}
	}
	t.Fatal("no open file")
	return ""
}

// A flipped byte in the middle of an open file is not a torn tail: the
// records after it must not vanish silently (found by the G2 corruption
// drill, docs/33 F2). The store says so, discards its saved position and
// records a gap; a genuine torn tail (the last write cut short) keeps the
// old quiet behaviour.
func TestDamageInTheMiddleIsReported(t *testing.T) {
	c := newClock()
	var mu sync.Mutex
	var logs []string
	mkOpts := func(dir string) Options {
		o := opts(dir, c)
		o.Logf = func(f string, a ...any) {
			mu.Lock()
			logs = append(logs, strings.TrimSpace(f))
			mu.Unlock()
		}
		return o
	}
	build := func() string {
		dir := t.TempDir()
		st, err := Open(mkOpts(dir))
		if err != nil {
			t.Fatal(err)
		}
		m := newModel()
		keys := []string{"k1", "k2", "k3", "k4", "k5", "k6"}
		for i := 0; i < 80; i++ {
			m.write(t, st, t0.Add(time.Duration(i)*time.Second), 1, keys, i)
		}
		st.SetCursor(Cursor{LSN: m.lsn, Seg: 1, Off: 100})
		if err := st.Sync(); err != nil {
			t.Fatal(err)
		}
		st.Close()
		return dir
	}
	// damage in the middle
	dir := build()
	p := openFileOf(t, dir)
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xff
	os.WriteFile(p, b, 0o644)
	logs = nil
	st, err := Open(mkOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Saved(); ok {
		t.Fatal("the saved position was kept although records were lost")
	}
	cv := st.Cover()
	if len(cv.Gaps) == 0 {
		t.Fatalf("no gap recorded: %+v", cv)
	}
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, "\n"), "bad record") {
		t.Fatalf("the damage was not reported: %v", logs)
	}
	if ents, _ := filepath.Glob(p + ".damaged-*"); len(ents) != 1 {
		t.Fatalf("the cut bytes were not kept: %v", ents)
	}
	st.Close()

	// a torn tail: the last write cut short
	dir = build()
	p = openFileOf(t, dir)
	b, _ = os.ReadFile(p)
	os.WriteFile(p, b[:len(b)-5], 0o644)
	logs = nil
	st, err = Open(mkOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Saved(); !ok {
		t.Fatal("a torn tail discarded the saved position")
	}
	if len(st.Cover().Gaps) != 0 || len(logs) != 0 {
		t.Fatalf("a torn tail was reported as damage: %v %+v", logs, st.Cover())
	}
	st.Close()
}
