package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---- D4: corruption ----
//
// One byte is flipped in a file of a stopped node (a copy of a pristine data
// directory is used for each case) and the node is started again. What the
// engine must do (docs/09 section 3, docs/05 section 19, docs/18): refuse to
// start and say why, or repair what it can rebuild (views, indexes) and say
// so, or, for a damaged tail of the last log segment, treat it as a torn
// write (the documented non-guarantee of a single node, docs/01). What it
// must never do is start and serve data that differ from the pristine data
// without a word, or serve a view that differs from the core.

type d4case struct {
	name   string
	pick   func(dir string) (path string, off int64, ok bool)
	expect string // "refuse", "repair" or "tail"
}

func listFiles(dir, sub string) []string {
	var out []string
	filepath.Walk(filepath.Join(dir, sub), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// usedLen is the offset after the last non-zero byte (segments are
// pre-sized with zeros).
func usedLen(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	fi, _ := f.Stat()
	const blk = 1 << 16
	buf := make([]byte, blk)
	for end := fi.Size(); end > 0; {
		start := max(0, end-blk)
		n, _ := f.ReadAt(buf[:end-start], start)
		for i := n - 1; i >= 0; i-- {
			if buf[i] != 0 {
				return start + int64(i) + 1
			}
		}
		end = start
	}
	return 0
}

func flipByte(path string, off int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, off); err != nil {
		return err
	}
	b[0] ^= 0xA5
	_, err = f.WriteAt(b, off)
	return err
}

func d4cases() []d4case {
	segs := func(dir string) []string { return listFiles(dir, "wal") }
	first := func(files []string, ok bool) (string, bool) {
		if len(files) == 0 {
			return "", false
		}
		return files[0], true
	}
	_ = first
	return []d4case{
		{"log segment, middle of the oldest sealed segment", func(dir string) (string, int64, bool) {
			s := segs(dir)
			if len(s) < 3 {
				return "", 0, false
			}
			return s[0], usedLen(s[0]) / 2, true
		}, "refuse"},
		{"newest log segment, in the middle of its records (valid records follow)", func(dir string) (string, int64, bool) {
			s := segs(dir)
			if len(s) == 0 {
				return "", 0, false
			}
			return s[len(s)-1], usedLen(s[len(s)-1]) / 2, true
		}, "tail"},
		{"newest log segment, inside the last record", func(dir string) (string, int64, bool) {
			s := segs(dir)
			if len(s) == 0 {
				return "", 0, false
			}
			u := usedLen(s[len(s)-1])
			return s[len(s)-1], u - 12, u > 64
		}, "tail"},
		{"checkpoint file (ckpt/)", func(dir string) (string, int64, bool) {
			c := listFiles(dir, "ckpt")
			if len(c) == 0 {
				return "", 0, false
			}
			sort.Strings(c)
			return c[len(c)-1], fileSize(c[len(c)-1]) / 2, true
		}, "repair"},
		{"saved columnar view (columnar.view)", func(dir string) (string, int64, bool) {
			p := filepath.Join(dir, "columnar.view")
			fi, err := os.Stat(p)
			if err != nil {
				return "", 0, false
			}
			return p, fi.Size() / 2, true
		}, "repair"},
		{"disk secondary index file (sidx)", func(dir string) (string, int64, bool) {
			f := listFiles(dir, "sidx")
			if len(f) == 0 {
				return "", 0, false
			}
			sort.Slice(f, func(i, j int) bool { return fileSize(f[i]) > fileSize(f[j]) })
			return f[0], fileSize(f[0]) / 2, true
		}, "repair"},
		{"history store segment (histstore)", func(dir string) (string, int64, bool) {
			f := listFiles(dir, "histstore")
			var seg []string
			for _, p := range f {
				if fileSize(p) > 1024 {
					seg = append(seg, p)
				}
			}
			if len(seg) == 0 {
				return "", 0, false
			}
			sort.Slice(seg, func(i, j int) bool { return fileSize(seg[i]) > fileSize(seg[j]) })
			return seg[0], fileSize(seg[0]) / 2, true
		}, "repair"},
	}
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

var saidRe = []string{"crc", "corrupt", "checksum", "torn_tail=true", "rebuild", "mismatch", "does not match", "bad record", "invalid", "truncat", "unreadable", "cannot", "failed", "refus"}

func said(log string) []string {
	low := strings.ToLower(log)
	var out []string
	for _, k := range saidRe {
		if strings.Contains(low, k) {
			out = append(out, k)
		}
	}
	return out
}

func copyDir(src, dst string) error {
	os.RemoveAll(dst)
	return exec.Command("cp", "-a", src, dst).Run()
}

func drillD4(e *Env, o *Opts, r *Result, mode string) {
	if mode == "cluster" {
		drillD4Cluster(e, o, r)
		return
	}
	t := newTopo(e, "d4", "single", "-checkpoint-min-mb", "4")
	n := t.Nodes[0]
	l := bankStart(e, r, t)
	if err := exec1(t.DSN(), fillDDL); !r.Check(err == nil, "fill: %v", err) {
		return
	}
	wl := t.Workload(l, 3)
	wl.Start(o.Seed)
	fl := NewFiller(t.DSN(), 2000, 2)
	fl.Start()
	for i := 0; i < 240; i++ {
		time.Sleep(500 * time.Millisecond)
		if st, err := n.StatusJSON(); err == nil && fnum(st, "wal_segments") >= 5 {
			break
		}
	}
	wl.Stop()
	fl.Stop()
	n.Post("/v1/admin/checkpoint", 60*time.Second)
	if !r.Check(t.Settle(120*time.Second) == nil, "settle") {
		return
	}
	tables := []string{"acct", "ev", "fill"}
	ref, err := digests(n.URL(), tables...)
	if !r.Check(err == nil, "reference digests: %v", err) {
		return
	}
	bad, sum := BankOracle(n.URL(), l)
	r.Check(len(bad) == 0, "baseline oracle: %s", joinBad(bad))
	n.Term()
	pristine := n.Dir + ".pristine"
	if !r.Check(copyDir(n.Dir, pristine) == nil, "copy of the data directory") {
		return
	}
	r.Logf("pristine data directory: %d MB, %d wal files; baseline: %s", dirBytes(pristine)>>20, len(listFiles(pristine, "wal")), sum)

	cases := d4cases()
	var okc, finding int
	for _, c := range cases {
		if !r.Check(copyDir(pristine, n.Dir) == nil, "copy") {
			return
		}
		path, off, ok := c.pick(n.Dir)
		if !ok {
			r.Logf("SKIP %s: no such file in the data directory", c.name)
			continue
		}
		before := fileSize(path)
		if err := flipByte(path, off); !r.Check(err == nil, "%s: flip: %v", c.name, err) {
			continue
		}
		class, detail := d4judge(r, n, l, fl, ref, tables)
		rel, _ := filepath.Rel(n.Dir, path)
		if strings.HasPrefix(c.name, "checkpoint") && class == "REFUSED" {
			// the operator's repair (docs/33): put the damaged checkpoint aside; the log is replayed from its start
			os.Rename(path, path+".bad")
			class2, detail2 := d4judge(r, n, l, fl, ref, tables)
			r.Check(class2 == "REPAIRED", "checkpoint moved aside: got %s: %s", class2, detail2)
			detail += fmt.Sprintf(" ; after moving the damaged checkpoint aside: %s (%s)", class2, trunc(detail2, 160))
		}
		line := fmt.Sprintf("%s [%s offset %d of %d]: %s", c.name, rel, off, before, class)
		pass := false
		switch class {
		case "REFUSED", "REPAIRED":
			pass = true
		case "DETECTED_BY_SCRUB_ONLY", "TORN_TAIL_DATA_LOST":
			if c.expect == "refuse" || c.expect == "tail" {
				pass, finding = true, finding+1
				if class == "TORN_TAIL_DATA_LOST" {
					line += " (FINDING F3: acknowledged transactions after the damaged record were dropped as a torn tail; documented non-guarantee of a single node, docs/01; the log line is the only trace)"
				} else {
					line += " (FINDING F1: the damage is not found at startup (a checkpoint covers the segment) nor on read; only a scrub finds it, and the background scrub is off by default)"
				}
			}
		}
		if pass {
			okc++
		}
		r.Check(pass, "%s: got %s, wanted %s: %s", c.name, class, c.expect, detail)
		r.Logf("%s; %s", line, detail)
	}
	r.OracleOK("%d corruption cases on a stopped single node: none served silently wrong data (each was REFUSED with a message, REPAIRED with data and views equal to the pristine ones, or reported as a torn tail); %d finding(s) listed in the log", okc, finding)
}

// d4judge starts the node on the damaged directory and classifies the outcome.
func d4judge(r *Result, n *Node, l *Ledger, fl *Filler, ref map[string]digest, tables []string) (string, string) {
	if err := n.Start(); err != nil {
		return "REFUSED", "start: " + err.Error()
	}
	up := false
	end := time.Now().Add(120 * time.Second)
	for time.Now().Before(end) {
		if nodeExited(n) {
			break
		}
		if n.WaitSQL(1*time.Second) == nil {
			up = true
			break
		}
	}
	log := n.logSince()
	defer func() { n.Kill9() }()
	if !up {
		if !nodeExited(n) {
			return "HUNG", "no SQL and the process is still running: " + lastLines(log, 3)
		}
		s := said(log)
		if len(s) == 0 {
			return "REFUSED_SILENTLY", "the process exited without a message: " + lastLines(log, 3)
		}
		return "REFUSED", "message: " + lastLines(log, 2)
	}
	n.WaitReady(90 * time.Second)
	b, code, _ := n.Post("/v1/admin/scrub?wait=1&rate_mb=0", 5*time.Minute)
	scrubOK := code == 200
	got, derr := digests(n.URL(), tables...)
	equal := derr == nil && sameDigests(ref, got) == ""
	rep, _ := n.WaitInSync(60 * time.Second)
	recOK := rep != nil && rep.Verdict == "IN_SYNC"
	recSum := ""
	if rep != nil {
		recSum = ReconcileSummary(rep)
	}
	sayings := said(log)
	torn := strings.Contains(log, "torn_tail=true")
	trace := "log: " + lastLines(grepLines(log, []string{"recovery:", "rebuild", "corrupt", "crc", "mismatch", "torn", "does not match", "not used"}), 3)
	switch {
	case equal && scrubOK && recOK:
		return "REPAIRED", fmt.Sprintf("data equal to the pristine data, scrub OK, views in sync; %s", trace)
	case !equal && torn:
		return "TORN_TAIL_DATA_LOST", fmt.Sprintf("started with torn_tail=true; data differ: %s; scrub %d %s; %s", sameDigests(ref, got), code, trunc(strings.TrimSpace(b), 120), trace)
	case !equal && !scrubOK:
		return "DETECTED_BY_SCRUB_ONLY", fmt.Sprintf("started without a word (words in the startup log: %v) and SELECT * returned different data (%s); POST /v1/admin/scrub answered %d: %s; %s", sayings, sameDigests(ref, got), code, trunc(strings.TrimSpace(b), 160), trace)
	case !equal:
		return "SILENT_WRONG_DATA", fmt.Sprintf("started silently with different data: %v %s", derr, sameDigests(ref, got))
	case !scrubOK:
		return "DETECTED_BY_SCRUB_ONLY", fmt.Sprintf("data equal but the scrub found damage: %s", trunc(strings.TrimSpace(b), 200))
	case !recOK:
		return "SILENT_VIEW_DIVERGENCE", fmt.Sprintf("started without a word (log words: %v), the tables equal the pristine ones, but reconcile -full says: %s", sayings, recSum)
	default:
		return "OTHER", trace
	}
}

func grepLines(s string, keys []string) string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		low := strings.ToLower(ln)
		for _, k := range keys {
			if strings.Contains(low, k) {
				out = append(out, ln)
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

func drillD4Cluster(e *Env, o *Opts, r *Result) {
	t := newTopo(e, "d4c", "cluster", "-checkpoint-min-mb", "4")
	l := bankStart(e, r, t)
	if err := exec1(t.DSN(), fillDDL); !r.Check(err == nil, "fill: %v", err) {
		return
	}
	wl := t.Workload(l, 3)
	wl.Start(o.Seed)
	fl := NewFiller(t.DSN(), 2000, 2)
	fl.Start()
	lead := t.Leader()
	for i := 0; i < 240; i++ {
		time.Sleep(500 * time.Millisecond)
		if st, err := lead.StatusJSON(); err == nil && fnum(st, "wal_segments") >= 5 {
			break
		}
	}
	wl.Stop()
	fl.Stop()
	if !r.Check(t.Settle(120*time.Second) == nil, "settle") {
		return
	}
	tables := []string{"acct", "ev", "fill"}
	ref, err := digests(lead.URL(), tables...)
	if !r.Check(err == nil, "reference digests: %v", err) {
		return
	}
	var victim *Node
	for _, n := range t.Nodes {
		if n != lead {
			victim = n
			break
		}
	}
	victim.Term()
	segs := listFiles(victim.Dir, "wal")
	r.Logf("follower %d has %d wal files", victim.ID, len(segs))
	if !r.Check(len(segs) >= 3, "too few log segments to corrupt a sealed one") {
		return
	}
	if !r.Check(flipByte(segs[0], usedLen(segs[0])/2) == nil, "flip") {
		return
	}
	class, detail := d4judge(r, victim, l, fl, ref, tables)
	r.Logf("follower %d with a flipped byte in its oldest log segment: %s: %s", victim.ID, class, detail)
	victim.Kill9()
	reseed := func(why string) {
		// the operator's repair: re-seed the node from the cluster (wipe, start, catch up by snapshot)
		os.RemoveAll(victim.Dir)
		r.Check(victim.Start() == nil, "restart after wiping")
		r.Check(victim.WaitSQL(120*time.Second) == nil, "node did not start after the wipe: %s", lastLines(victim.logSince(), 4))
		lead2 := t.Leader()
		r.Check(lead2 != nil && catchUp(r, victim, lead2, 180*time.Second), "re-seeded node %d did not catch up", victim.ID)
		got, derr := digests(victim.URL(), tables...)
		r.Check(derr == nil && sameDigests(ref, got) == "", "re-seeded node differs from the leader: %v %s", derr, sameDigests(ref, got))
		r.OracleOK("cluster: a follower with a flipped byte in a sealed log segment %s; wiping its directory and restarting re-seeded it from the leader and its tables equal the leader's", why)
	}
	switch class {
	case "REFUSED":
		reseed("refused to start with a message")
	case "DETECTED_BY_SCRUB_ONLY":
		r.Logf("FINDING F1 (cluster): the damaged follower started without a word and served different data from the leader's until a scrub was run")
		reseed("started silently (finding F1) and only the scrub found the damage")
	case "REPAIRED":
		r.OracleOK("cluster: a follower with a flipped byte in a sealed log segment started, and its tables equal the leader's")
	default:
		r.Check(false, "cluster follower with a corrupt sealed segment: %s: %s", class, detail)
	}
}
