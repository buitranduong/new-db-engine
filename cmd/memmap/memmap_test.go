package main

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	runtime.MemProfileRate = 2048 // before any allocation worth attributing
	os.Exit(m.Run())
}

func TestNumbersAreUnique(t *testing.T) {
	for _, d := range []string{"simbench", "prefix"} {
		f, err := numFn(d)
		if err != nil {
			t.Fatal(err)
		}
		seen := make(map[string]int64, 300000)
		for i := int64(0); i < 300000; i++ {
			n := f(i)
			if len(n) != 10 || n[0] != '0' {
				t.Fatalf("%s: %q is not a 10-digit number starting with 0", d, n)
			}
			if j, dup := seen[n]; dup {
				t.Fatalf("%s: i=%d and i=%d both give %s", d, j, i, n)
			}
			seen[n] = i
		}
	}
	if _, err := numFn("nope"); err == nil {
		t.Fatal("unknown distribution accepted")
	}
}

func TestPrefixDistributionIsClustered(t *testing.T) {
	cnt := map[string]int{}
	for i := int64(0); i < 100000; i++ {
		cnt[prefixNum(i)[:3]]++
	}
	if len(cnt) != len(prefixes) {
		t.Fatalf("%d distinct prefixes, want %d", len(cnt), len(prefixes))
	}
	if cnt["090"] < 3*cnt["059"] {
		t.Fatalf("weights not applied: 090=%d 059=%d", cnt["090"], cnt["059"])
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		frames []string
		want   string
	}{
		{[]string{"runtime.mallocgc", "stask/internal/core.(*keyArena).add", "stask/internal/core.(*tree).insert", "stask/internal/core.(*Engine).commit"}, sPrimary},
		{[]string{"runtime.growslice", "stask/internal/proj.(*Substr).Apply", "stask/internal/core.(*Follower).run"}, sTrigram},
		{[]string{"runtime.makeslice", "stask/internal/proj.(*colTable).append", "stask/internal/proj.(*Columnar).Apply"}, sColumn},
		{[]string{"runtime.newobject", "stask/internal/core.(*secIndex).add"}, sSecond},
		{[]string{"runtime.newobject", "stask/internal/core.(*Engine).noteMoved"}, sHistory},
		{[]string{"runtime.makeslice", "stask/internal/wal.(*Log).Scan"}, sWAL},
		{[]string{"runtime.makemap", "stask/internal/core.newCommitBatch", "stask/internal/core.(*Engine).committer"}, sRing},
		{[]string{"runtime.makeslice", "stask/cmd/memmap.(*runner).load"}, sBench},
		{[]string{"runtime.main"}, sUnknown},
	}
	for _, c := range cases {
		if got := classify(c.frames); got != c.want {
			t.Errorf("%v: got %q want %q", c.frames, got, c.want)
		}
	}
}

// TestRunSmall runs the whole measurement at N=20k, the size the unit test
// is allowed (docs/19: scale numbers come from scripts/perf-run.sh only).
func TestRunSmall(t *testing.T) {
	for _, dist := range []string{"simbench", "prefix"} {
		rep, err := run(config{N: 20000, Dir: t.TempDir() + "/mm", Dist: dist, Updates: 2, Batch: 500, Rate: 4096}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, s := range rep.Stages {
			names = append(names, s.Name)
		}
		if strings.Join(names, ",") != "baseline,primary,history,secondary,trigram,columnar" {
			t.Fatalf("stages %v", names)
		}
		for _, n := range []string{"primary", "secondary", "trigram", "columnar"} {
			if s := stage(rep, n); s.Delta <= 0 {
				t.Errorf("%s: stage added %d bytes of heap, want > 0", n, s.Delta)
			}
		}
		if rep.Direct["primary_index_bytes"] <= 0 || rep.Direct["secondary_entries"] != 20000 || rep.Direct["columnar_mem_bytes"] <= 0 {
			t.Errorf("direct numbers: %v", rep.Direct)
		}
		// 20k keys: the primary index cannot cost less than the key bytes
		if p := stage(rep, "primary").Delta; p < 20000*10 {
			t.Errorf("primary index %d B for 20000 10-byte keys", p)
		}
		if rep.PprofTot <= 0 || len(rep.Pprof) == 0 {
			t.Errorf("no pprof attribution")
		}
		var buf bytes.Buffer
		printReport(&buf, rep)
		out := buf.String()
		for _, want := range []string{"primary index", "trigram view", "columnar view", "secondary btree", "history:", "pprof inuse_space"} {
			if !strings.Contains(out, want) {
				t.Errorf("report lacks %q:\n%s", want, out)
			}
		}
		if dist == "prefix" {
			t.Log("\n" + out)
		}
	}
}
