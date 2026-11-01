package main

import (
	"fmt"
)

// The "load happened" guard (docs/32): a run is valid only if every class of load it was configured to apply
// produced a minimum number of successes. Without it a run whose import never completed a batch printed "all
// oracles held" because import_history_match_journal_final compared an empty set with an empty set.

// classCounts is the number of successful operations per class, summed over the run.
func classCounts(run *Run) map[string]int {
	out := map[string]int{}
	for _, rc := range run.recs {
		for _, x := range rc.recs {
			if x.err == "" {
				out[x.class]++
			}
		}
	}
	return out
}

// loadGuard returns one line per unmet minimum (empty = the load happened).
func loadGuard(cfg *Config, counters map[string]int64, ok map[string]int, crash *CrashInfo) []string {
	var bad []string
	need := func(name string, got, min int, why string) {
		if min > 0 && got < min {
			bad = append(bad, fmt.Sprintf("%s: %d successes, minimum %d (%s)", name, got, min, why))
		}
	}
	if cfg.T > 0 {
		need("T transactions (t_order+t_pay+t_cancel)", ok["t_order"]+ok["t_pay"]+ok["t_cancel"], cfg.MinT, fmt.Sprintf("-t %d, -min-t", cfg.T))
	}
	if cfg.P > 0 {
		need("P reads (r1_point+r1_keyset)", ok["r1_point"]+ok["r1_keyset"], cfg.MinP, fmt.Sprintf("-p %d, -min-p", cfg.P))
	}
	if cfg.R > 0 {
		need("r2_page", ok["r2_page"], cfg.MinR2, fmt.Sprintf("-r %d, -min-r2", cfg.R))
	}
	if cfg.Tear > 0 {
		need("tear_tx", ok["tear_tx"], 1, fmt.Sprintf("-tear %d", cfg.Tear))
	}
	if cfg.ImportMode != "never" && cfg.MinImport > 0 {
		need("import batches", int(counters["import_batches"]), cfg.MinImport, "import-mode "+cfg.ImportMode+", -min-import")
		// at least one FULL slice: all batches of one slice committed (1% of the sims)
		if cfg.N > 0 && int(counters["import_rows"]) < cfg.N/100 {
			bad = append(bad, fmt.Sprintf("import slice: %d rows imported, a full slice is %d rows (1%% of the sims)", counters["import_rows"], cfg.N/100))
		}
	}
	if cfg.Probes > 0 && cfg.MinProbesPct > 0 {
		need("read-your-writes probes", int(counters["probes_done"]), cfg.Probes*cfg.MinProbesPct/100, fmt.Sprintf("-probes %d, -min-probes-pct %d", cfg.Probes, cfg.MinProbesPct))
	}
	if cfg.CrashAt > 0 {
		switch {
		case crash == nil:
			bad = append(bad, "crash drill: -crash-at was set but the drill did not run (-kill-cmd/-restart-cmd missing or the run ended first)")
		default:
			if !crash.KillOK {
				bad = append(bad, "crash drill: the kill command failed ("+crash.KillOut+"): the server was not killed")
			}
			if crash.Restarts < 1 || !crash.Recovered {
				bad = append(bad, fmt.Sprintf("crash drill: restart count %d, recovered %v", crash.Restarts, crash.Recovered))
			}
			if cfg.PidCmd != "" && (crash.PidBefore == "" || crash.PidBefore == crash.PidAfter) {
				bad = append(bad, fmt.Sprintf("crash drill: server pid before %q, after %q: no evidence the server was replaced", crash.PidBefore, crash.PidAfter))
			}
		}
	}
	return bad
}
