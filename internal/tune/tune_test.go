package tune

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"stask/internal/conf"
)

func TestDetect(t *testing.T) {
	if c := CPUs(); c <= 0 || c > float64(runtime.NumCPU()) {
		t.Fatalf("CPUs = %v (NumCPU %d)", c, runtime.NumCPU())
	}
	if m := Memory(); m < 64<<20 {
		t.Fatalf("Memory = %d", m)
	}
	d := DiskOf(t.TempDir())
	if d.FS == "" || d.SizeBytes <= 0 {
		t.Fatalf("DiskOf: %+v", d)
	}
}

// The generated file sets only settings staskd has, with valid values.
func TestRenderLoads(t *testing.T) {
	dir := t.TempDir()
	f, err := Gather(dir, 600*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	s := f.Speed
	if s.SeqWriteMBps <= 0 || s.SyncFreshOps == 0 || s.SyncZeroedOps == 0 || s.SyncFreshP99 < s.SyncFreshP50 {
		t.Fatalf("speed %+v", s)
	}
	text := Render(f)
	p := filepath.Join(dir, "staskd.conf")
	os.WriteFile(p, []byte(text), 0o644)
	// the staskd settings stasktune writes
	fs := flag.NewFlagSet("staskd", flag.ContinueOnError)
	for _, n := range []string{"dir", "commit-wait", "history-retention"} {
		fs.String(n, "", "")
	}
	for _, n := range []string{"heavy-workers", "scan-workers", "procs", "fg-weight", "heavy-weight", "heavy-nice", "scan-min-helpers"} {
		fs.Int(n, 0, "")
	}
	for _, n := range []string{"mem-limit-mb", "go-gc", "segment-mb", "zero-rate-mb", "gc-rate-mb"} {
		fs.Int64(n, 0, "")
	}
	for _, n := range []string{"runnable-threshold", "heavy-cap"} {
		fs.Float64(n, 0, "")
	}
	for _, n := range []string{"cpu-groups", "prezero", "gc"} {
		fs.Bool(n, false, "")
	}
	fs.Parse(nil)
	if err := conf.Load(fs, p); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if !strings.Contains(text, "measured: sequential write") {
		t.Fatalf("no measurements in\n%s", text)
	}
}

func TestGCTunerPercent(t *testing.T) {
	g := &GCTuner{Headroom: 2 << 30, MinPercent: 25, MaxPercent: 400}
	for _, c := range []struct {
		live int64
		want int
	}{{0, 400}, {100 << 20, 400}, {512 << 20, 400}, {1 << 30, 200}, {5 << 30, 40}, {20 << 30, 25}} {
		if got := g.PercentFor(c.live); got != c.want {
			t.Errorf("live %d MB: GOGC %d, want %d", c.live>>20, got, c.want)
		}
	}
}
