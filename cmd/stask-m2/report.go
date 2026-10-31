package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func writeReport(cfg *Config, run *Run, mach Machine, spans any, elapsed time.Duration, crash *CrashInfo, barrier []QCompare, replay map[string]any, journalN int) {
	if len(mach.DirtyWhy) > 0 {
		mach.Dirty = true
	}
	// merge records
	byClass := map[string][]Rec{}
	byClassPhase := map[string][]Rec{}
	for _, rc := range run.recs {
		for _, x := range rc.recs {
			byClass[x.class] = append(byClass[x.class], x)
			byClassPhase[x.class+"|"+x.phase] = append(byClassPhase[x.class+"|"+x.phase], x)
		}
	}
	classes := map[string]Stat{}
	var names []string
	for k, v := range byClass {
		classes[k] = statOf(v, elapsed.Seconds())
		names = append(names, k)
	}
	sort.Strings(names)
	phases := map[string]map[string]Stat{}
	for k, v := range byClassPhase {
		p := strings.SplitN(k, "|", 2)
		if phases[p[0]] == nil {
			phases[p[0]] = map[string]Stat{}
		}
		// window of a phase
		w := elapsed.Seconds()
		if cfg.ImportMode == "alternate" {
			w = cfg.Duration.Seconds() / 2
		}
		phases[p[0]][p[1]] = statOf(v, w)
	}
	prio := map[string]any{}
	for _, c := range []string{"r1_point", "r1_keyset", "r2_page"} {
		a, b := phases[c]["noimport"], phases[c]["import"]
		e := map[string]any{"p99_noimport_ms": a.P99, "p99_import_ms": b.P99, "n_noimport": a.OK, "n_import": b.OK}
		if a.P99 > 0 && b.P99 > 0 {
			e["ratio_p99_import_over_noimport"] = b.P99 / a.P99
		}
		prio[c] = e
	}
	counters := run.counters
	oracles := []OracleRes{}
	for _, o := range run.oracle {
		oracles = append(oracles, *o)
	}
	sort.Slice(oracles, func(i, j int) bool { return oracles[i].Name < oracles[j].Name })

	commit := ""
	if b, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		commit = strings.TrimSpace(string(b))
	}
	rep := map[string]any{
		"label": cfg.Label, "harness_commit": commit, "date": time.Now().UTC().Format(time.RFC3339),
		"scale": map[string]any{"sims": cfg.N, "rows_total": NewGen(cfg.N, cfg.Seed, cfg.HistPerSim).TotalRows(), "seed": cfg.Seed},
		"load": map[string]any{"T": cfg.T, "P": cfg.P, "R": cfg.R, "import_every_s": cfg.ImportEvery, "import_mode": cfg.ImportMode,
			"duration_s": cfg.Duration.Seconds(), "elapsed_s": elapsed.Seconds(), "hot_frac": cfg.HotFrac, "hot": cfg.Hot, "exec_mode": cfg.Exec, "isolation": cfg.Iso},
		"machine": mach, "phases": spans, "classes": classes, "classes_by_phase": phases, "priority_probe": prio,
		"counters": counters, "oracles": oracles, "journal_txns": journalN, "replay": replay, "barrier": barrier, "crash": crash,
		"note": "all numbers are measurements of this run at the stated scale and load; no performance claim",
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	os.WriteFile(filepath.Join(cfg.Out, "report.json"), b, 0o644)

	var m strings.Builder
	tag := "clean"
	if mach.Dirty {
		tag = "DIRTY (" + strings.Join(mach.DirtyWhy, "; ") + ")"
	}
	fmt.Fprintf(&m, "# stask-m2 report: %s\n\n", cfg.Label)
	fmt.Fprintf(&m, "- run: %s, harness commit %s, **%s**\n", rep["date"], commit, tag)
	fmt.Fprintf(&m, "- scale: %d sims (%d rows), seed %s; load: T=%d P=%d R=%d, import %s every %ds, hot set %d at %.0f%%; %.0f s elapsed\n",
		cfg.N, NewGen(cfg.N, cfg.Seed, cfg.HistPerSim).TotalRows(), cfg.Seed, cfg.T, cfg.P, cfg.R, cfg.ImportMode, cfg.ImportEvery, cfg.Hot, cfg.HotFrac*100, elapsed.Seconds())
	fmt.Fprintf(&m, "- machine: %d CPUs, load1 start %.2f / max %.2f / end %.2f, disk free %.1f GB\n\n", mach.CPUs, mach.Load1At0, mach.Load1Max, mach.Load1End, mach.FreeGBEnd)
	fmt.Fprintf(&m, "## Oracles\n\n| oracle | checks | violations | example |\n|---|---|---|---|\n")
	totalFail := 0
	for _, o := range oracles {
		ex := ""
		if len(o.Detail) > 0 {
			ex = strings.ReplaceAll(o.Detail[0], "|", "/")
		}
		st := fmt.Sprint(o.Fails)
		if o.Fails > 0 {
			st = "**" + st + "**"
			totalFail += o.Fails
		}
		fmt.Fprintf(&m, "| %s | %d | %s | %s |\n", o.Name, o.Checks, st, ex)
	}
	if totalFail == 0 {
		m.WriteString("\nAll oracles held.\n")
	} else {
		fmt.Fprintf(&m, "\n**%d oracle violations.**\n", totalFail)
	}
	fmt.Fprintf(&m, "\n## Latency and throughput per class (scale %d sims, load as above, measured)\n\n| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |\n|---|---|---|---|---|---|---|---|\n", cfg.N)
	for _, n := range names {
		s := classes[n]
		fmt.Fprintf(&m, "| %s | %d | %v | %.1f | %.2f | %.2f | %.2f | %.2f |\n", n, s.OK, s.Errors, s.PerSec, s.P50, s.P95, s.P99, s.Max)
	}
	if cfg.ImportMode == "alternate" {
		fmt.Fprintf(&m, "\n## Priority probe (record only, no threshold)\n\nphase order: %s first. p99 while the import worker runs vs not (same run, same load of T/P/R):\n\n| class | p99 no import ms | p99 import ms | ratio | n no import | n import |\n|---|---|---|---|---|---|\n", cfg.PhaseFirst)
		for _, c := range []string{"r1_point", "r1_keyset", "r2_page"} {
			e := prio[c].(map[string]any)
			ratio := "n/a"
			if v, ok := e["ratio_p99_import_over_noimport"]; ok {
				ratio = fmt.Sprintf("%.2f", v)
			}
			fmt.Fprintf(&m, "| %s | %.2f | %.2f | %s | %v | %v |\n", c, e["p99_noimport_ms"], e["p99_import_ms"], ratio, e["n_noimport"], e["n_import"])
		}
		if s, ok := phases["import"]["import"]; ok {
			fmt.Fprintf(&m, "\nimport transactions: %d ok, p50 %.0f ms, max %.0f ms, errors %v\n", s.OK, s.P50, s.Max, s.Errors)
		}
	}
	if len(barrier) > 0 {
		fmt.Fprintf(&m, "\n## Barrier comparison with postgres:16 (journal of %d committed transactions replayed in commit order)\n\n", journalN)
		fmt.Fprintf(&m, "replay: %v\n\n| query | rows sut | rows ref | equal | ms sut | ms ref | first difference |\n|---|---|---|---|---|---|---|\n", replay)
		for _, q := range barrier {
			eq := "yes"
			if !q.Equal {
				eq = "**NO**"
				if q.SameSet {
					eq += " (same multiset, order differs)"
				}
			}
			fmt.Fprintf(&m, "| %s | %d | %d | %s | %d | %d | %s%s%s |\n", q.Query, q.RowsSUT, q.RowsRef, eq, q.MsSUT, q.MsRef, strings.ReplaceAll(q.Diff, "|", "/"), trunc(q.ErrSUT, 120), trunc(q.ErrRef, 120))
		}
	}
	if crash != nil {
		fmt.Fprintf(&m, "\n## Crash drill\n\nkill -9 at %.1f s; acknowledged before the kill: %d; recovered: %v in %d ms; in flight with unknown outcome: %d (of which present after recovery: %d)\n", crash.AtS, crash.AckedBefore, crash.Recovered, crash.RestartMs, crash.Unknown, crash.Present)
	}
	fmt.Fprintf(&m, "\n## Counters\n\n")
	var ks []string
	for k := range counters {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		fmt.Fprintf(&m, "- %s = %d\n", k, counters[k])
	}
	os.WriteFile(filepath.Join(cfg.Out, "report.md"), []byte(m.String()), 0o644)
}
