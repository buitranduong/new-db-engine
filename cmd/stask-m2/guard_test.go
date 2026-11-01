package main

import (
	"strings"
	"testing"
	"time"
)

func fullCfg() *Config {
	return &Config{N: 100000, T: 4, P: 4, R: 2, Tear: 1, ImportMode: "always", Probes: 1000,
		MinT: 100, MinP: 100, MinR2: 30, MinImport: 1, MinProbesPct: 90}
}

func healthy() (map[string]int64, map[string]int) {
	return map[string]int64{"import_batches": 20, "import_rows": 1000, "probes_done": 1000},
		map[string]int{"t_order": 80, "t_pay": 40, "t_cancel": 20, "r1_point": 90, "r1_keyset": 90, "r2_page": 31, "tear_tx": 2}
}

func TestLoadGuardHealthyRunIsValid(t *testing.T) {
	c, ok := healthy()
	if bad := loadGuard(fullCfg(), c, ok, nil); len(bad) != 0 {
		t.Fatalf("a run that applied its load is INVALID: %v", bad)
	}
}

// the 10M run: every oracle "held" with zero completed import batches
func TestLoadGuardZeroImportBatchesIsInvalid(t *testing.T) {
	c, ok := healthy()
	c["import_batches"], c["import_rows"] = 0, 0
	bad := loadGuard(fullCfg(), c, ok, nil)
	if len(bad) == 0 || !strings.Contains(strings.Join(bad, "\n"), "import batches: 0 successes") {
		t.Fatalf("zero import batches must be INVALID, got %v", bad)
	}
	cfg := fullCfg()
	cfg.ImportMode = "never"
	if bad := loadGuard(cfg, c, ok, nil); len(bad) != 0 {
		t.Fatalf("no import was configured: %v", bad)
	}
	cfg = fullCfg()
	cfg.MinImport = 0
	c["import_batches"] = 0
	if bad := loadGuard(cfg, c, ok, nil); len(bad) != 0 && strings.Contains(strings.Join(bad, ""), "import batches") {
		t.Fatalf("a 0 minimum disables the check: %v", bad)
	}
}

func TestLoadGuardPartialSliceAndOtherClasses(t *testing.T) {
	c, ok := healthy()
	c["import_rows"] = 999 // less than 1% of 100000
	if bad := loadGuard(fullCfg(), c, ok, nil); len(bad) != 1 || !strings.Contains(bad[0], "full slice") {
		t.Fatalf("got %v", bad)
	}
	c, ok = healthy()
	ok["r2_page"], ok["t_order"], ok["t_pay"], ok["t_cancel"], c["probes_done"] = 0, 10, 0, 0, 100
	bad := strings.Join(loadGuard(fullCfg(), c, ok, nil), "\n")
	for _, want := range []string{"r2_page", "T transactions", "read-your-writes probes"} {
		if !strings.Contains(bad, want) {
			t.Errorf("missing %q in %s", want, bad)
		}
	}
}

func TestLoadGuardCrashDrillNeedsEvidence(t *testing.T) {
	c, ok := healthy()
	cfg := fullCfg()
	cfg.CrashAt, cfg.PidCmd = time.Second, "cat pid"
	if bad := loadGuard(cfg, c, ok, nil); len(bad) == 0 {
		t.Fatal("a drill that did not run is INVALID")
	}
	same := &CrashInfo{KillOK: true, Restarts: 1, Recovered: true, PidBefore: "42", PidAfter: "42"}
	if bad := loadGuard(cfg, c, ok, same); len(bad) == 0 || !strings.Contains(bad[0], "pid") {
		t.Fatalf("same pid after the kill: %v", bad)
	}
	failed := &CrashInfo{KillOK: false, KillOut: "no such process", Restarts: 1, Recovered: true, PidBefore: "42", PidAfter: "43"}
	if bad := loadGuard(cfg, c, ok, failed); len(bad) == 0 || !strings.Contains(bad[0], "kill command failed") {
		t.Fatalf("failed kill: %v", bad)
	}
	good := &CrashInfo{KillOK: true, Restarts: 1, Recovered: true, PidBefore: "42", PidAfter: "43"}
	if bad := loadGuard(cfg, c, ok, good); len(bad) != 0 {
		t.Fatalf("real kill and restart: %v", bad)
	}
}
