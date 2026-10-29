package conf

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	cap := fs.Float64("heavy-cap", 0, "")
	gc := fs.Bool("gc", true, "")
	iv := fs.Duration("gc-interval", time.Second, "")
	dir := fs.String("dir", "/d", "")
	if err := fs.Parse([]string{"-dir", "/cli"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "s.conf")
	os.WriteFile(p, []byte("# comment\n\nheavy-cap = 0.75\ngc false\ngc-interval = \"5s\"\ndir = /file\n"), 0o644)
	if err := Load(fs, p); err != nil {
		t.Fatal(err)
	}
	if *cap != 0.75 || *gc || *iv != 5*time.Second {
		t.Fatalf("got cap=%v gc=%v interval=%v", *cap, *gc, *iv)
	}
	if *dir != "/cli" {
		t.Fatalf("command line must win over the file: dir=%q", *dir)
	}
	for _, bad := range []string{"no-such = 1\n", "heavy-cap = lots\n"} {
		fs := flag.NewFlagSet("y", flag.ContinueOnError)
		fs.Float64("heavy-cap", 0, "")
		fs.Parse(nil)
		os.WriteFile(p, []byte(bad), 0o644)
		if err := Load(fs, p); err == nil || !strings.Contains(err.Error(), ":1:") {
			t.Fatalf("%q: want an error naming line 1, got %v", bad, err)
		}
	}
}
