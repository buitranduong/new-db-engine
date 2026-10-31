package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- helpers shared by D3, D4 and D5 ----

// tableDigest reads a whole table and returns its row count and an
// order-independent checksum of the wire bytes of every row.
func tableDigest(url, table string) (int64, uint64, error) {
	c, err := connect(url)
	if err != nil {
		return 0, 0, err
	}
	defer c.Close(context.Background())
	rows, err := c.Query(context.Background(), "SELECT * FROM "+table)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var n int64
	var sum uint64
	sep := []byte{0xff}
	for rows.Next() {
		h := fnv.New64a()
		for _, v := range rows.RawValues() {
			h.Write(v)
			h.Write(sep)
		}
		sum += h.Sum64()
		n++
	}
	return n, sum, rows.Err()
}

type digest struct {
	n   int64
	sum uint64
}

func digests(url string, tables ...string) (map[string]digest, error) {
	m := map[string]digest{}
	for _, t := range tables {
		n, s, err := tableDigest(url, t)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		m[t] = digest{n, s}
	}
	return m, nil
}

func sameDigests(a, b map[string]digest) string {
	var diff []string
	for t, d := range a {
		if b[t] != d {
			diff = append(diff, fmt.Sprintf("%s: %d rows/%x vs %d rows/%x", t, d.n, d.sum, b[t].n, b[t].sum))
		}
	}
	sort.Strings(diff)
	return strings.Join(diff, "; ")
}

func sudoOK() bool { return exec.Command("sudo", "-n", "true").Run() == nil }

// smallMount mounts a tmpfs of the given size at a new directory (sudo -n).
// The returned function stops nothing: callers stop their nodes first.
func smallMount(e *Env, name string, mb int) (string, func(), error) {
	dir := filepath.Join(e.Work, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	opt := fmt.Sprintf("size=%dm,uid=%d,gid=%d,mode=0755", mb, os.Getuid(), os.Getgid())
	if out, err := exec.Command("sudo", "-n", "mount", "-t", "tmpfs", "-o", opt, "g2drills-"+name, dir).CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("mount: %v: %s", err, out)
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			exec.Command("sudo", "-n", "umount", "-l", dir).Run()
			os.Remove(dir)
		})
	}
	addCleanup(cleanup)
	return dir, cleanup, nil
}

func dirBytes(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// logSince returns the node log text of its latest start.
func (n *Node) logSince() string {
	b, _ := os.ReadFile(n.Log)
	s := string(b)
	if i := strings.LastIndex(s, "---- start "); i >= 0 {
		s = s[i:]
	}
	return s
}

func lastLines(s string, k int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > k {
		ls = ls[len(ls)-k:]
	}
	var out []string
	for _, l := range ls {
		if len(l) > 220 {
			l = l[:220]
		}
		out = append(out, l)
	}
	return strings.Join(out, " | ")
}

// ---- filler: rows big enough to fill a small disk, with a ledger ----

const fillDDL = "CREATE TABLE fill (id bigint PRIMARY KEY, payload text NOT NULL)"

func fillPayload(id int64, size int) string {
	b := make([]byte, size)
	x := uint64(id)*0x9E3779B97F4A7C15 + 1
	for i := range b {
		x = x*6364136223846793005 + 1442695040888963407
		b[i] = 'a' + byte((x>>33)%26)
	}
	return string(b)
}

type Filler struct {
	URL     string
	Size    int
	Workers int
	mu      sync.Mutex
	Acked   map[int64]bool
	Unknown map[int64]bool
	next    atomic.Int64
	stop    atomic.Bool
	wg      sync.WaitGroup
	errMu   sync.Mutex
	FirstEr string
	LastEr  string
	ErrCode string
	Errs    atomic.Int64
	OKs     atomic.Int64
	lastOK  atomic.Int64 // unix nanos of the last acknowledged insert
	Lo, Hi  int64        // the ids this filler may use: [Lo, Hi)
}

func NewFiller(url string, size, workers int) *Filler {
	return &Filler{URL: url, Size: size, Workers: workers, Acked: map[int64]bool{}, Unknown: map[int64]bool{}, Lo: 0, Hi: 1_000_000}
}

func (f *Filler) Start() {
	for w := 0; w < f.Workers; w++ {
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			var conn interface {
				Close(context.Context) error
			}
			_ = conn
			for !f.stop.Load() {
				c, err := connect(f.URL)
				if err != nil {
					f.noteErr(err, "")
					time.Sleep(100 * time.Millisecond)
					continue
				}
				for !f.stop.Load() && !c.IsClosed() {
					id := f.next.Add(1)
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					_, err := c.Exec(ctx, "INSERT INTO fill (id, payload) VALUES ($1, $2)", id, fillPayload(id, f.Size))
					cancel()
					if err == nil {
						f.mu.Lock()
						f.Acked[id] = true
						f.mu.Unlock()
						f.OKs.Add(1)
						f.lastOK.Store(time.Now().UnixNano())
						continue
					}
					code := pgCode(err)
					if code == "" { // the connection broke: the insert may or may not be there
						f.mu.Lock()
						f.Unknown[id] = true
						f.mu.Unlock()
					}
					f.noteErr(err, code)
					if code != "" {
						time.Sleep(20 * time.Millisecond)
					}
				}
				c.Close(context.Background())
			}
		}()
	}
}

func (f *Filler) noteErr(err error, code string) {
	f.Errs.Add(1)
	f.errMu.Lock()
	if f.FirstEr == "" {
		f.FirstEr, f.ErrCode = err.Error(), code
	}
	f.LastEr = err.Error()
	f.errMu.Unlock()
}

func (f *Filler) Stop() { f.stop.Store(true); f.wg.Wait() }

// Verify checks that every acknowledged row is present with its exact payload
// and that no row nobody inserted exists.
func (f *Filler) Verify(url string) []string {
	var bad []string
	c, err := connect(url)
	if err != nil {
		return []string{err.Error()}
	}
	defer c.Close(context.Background())
	rows, err := c.Query(context.Background(), "SELECT id, payload FROM fill")
	if err != nil {
		return []string{err.Error()}
	}
	seen := map[int64]bool{}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			bad = append(bad, err.Error())
			break
		}
		if id < f.Lo || id >= f.Hi {
			continue
		}
		seen[id] = true
		if p != fillPayload(id, f.Size) {
			bad = append(bad, fmt.Sprintf("row %d has a wrong payload", id))
		}
	}
	rows.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	lost := 0
	for id := range f.Acked {
		if !seen[id] {
			lost++
		}
	}
	if lost > 0 {
		bad = append(bad, fmt.Sprintf("%d acknowledged fill rows are missing", lost))
	}
	for id := range seen {
		if !f.Acked[id] && !f.Unknown[id] {
			// an insert that failed with an error answer must not be there either
			bad = append(bad, fmt.Sprintf("row %d exists although its insert was refused", id))
			break
		}
	}
	return bad
}

// ---- D3: disk full ----
//
// A node (single) or one node of three (cluster) keeps its data on a 96 MB
// tmpfs of which a ballast file takes 58 MB. Fillers insert 3 KB rows and the
// bank workload runs until the disk is full. Expected (docs/01, docs/04):
// the log refuses the write with a clear error and stays alive, reads are
// served, a restart on the full disk is safe, and once space is freed the
// node takes writes again; no acknowledged write is lost or corrupted. In a
// cluster two cases are different: a follower with a full disk stops while
// the majority goes on; a LEADER with a full disk keeps its leadership and
// the cluster refuses writes until the operator stops that node (documented
// in docs/33 as a known limit).

type d3env struct {
	e       *Env
	o       *Opts
	r       *Result
	mode    string
	t       *Topo
	victim  *Node
	mnt     string
	ballast string
	l       *Ledger
	dsn     string
	fillers []*Filler
	nextLo  int64
}

func (d *d3env) newFiller(size, workers int) *Filler {
	f := NewFiller(d.dsn, size, workers)
	f.Lo, f.Hi = d.nextLo, d.nextLo+1_000_000
	f.next.Store(d.nextLo)
	d.nextLo += 1_000_000
	d.fillers = append(d.fillers, f)
	return f
}

func (d *d3env) makeBallast() bool {
	os.Remove(d.ballast)
	free := freeMB(d.mnt)
	keep := int64(36)
	if d.mode == "cluster" {
		keep = 14
	}
	if free <= keep+2 {
		return true
	}
	out, err := exec.Command("fallocate", "-l", fmt.Sprintf("%dm", free-keep), d.ballast).CombinedOutput()
	return d.r.Check(err == nil, "fallocate: %v %s", err, out)
}

// fillOnce fills the disk and reports how the system reacted. leaderCase:
// the victim is the leader (or the only node).
func (d *d3env) fillOnce(label string, leaderCase bool) (first, last string, refused bool) {
	r := d.r
	wl := d.t.Workload(d.l, 3)
	wl.Start(d.o.Seed)
	fl := d.newFiller(3000, 3)
	fl.Start()
	t0 := time.Now()
	for time.Since(t0) < 4*time.Minute && !refused {
		time.Sleep(300 * time.Millisecond)
		if leaderCase || d.mode == "single" {
			refused = fl.Errs.Load() > 20
		} else {
			refused = !d.victim.Running() || healthCode(d.victim) != 200 || nodeExited(d.victim) || strings.Contains(d.victim.logSince(), "cannot allocate log segment")
		}
	}
	if !r.Check(refused, "%s: the disk never filled (%d fill rows acknowledged, %d MB used)", label, fl.OKs.Load(), dirBytes(d.victim.Dir)>>20) {
		wl.Stop()
		fl.Stop()
		return
	}
	fl.errMu.Lock()
	first, last = fl.FirstEr, fl.LastEr
	code := fl.ErrCode
	fl.errMu.Unlock()
	r.Logf("%s: disk full after %v and %d acknowledged fill rows (data dir %d MB)", label, time.Since(t0).Round(time.Second), fl.OKs.Load(), dirBytes(d.victim.Dir)>>20)
	if first != "" && (leaderCase || d.mode == "single") {
		r.Logf("%s: first error seen by a client: SQLSTATE %q: %s", label, code, first)
	}
	a0, _, _ := d.l.Counts()
	ok0 := fl.OKs.Load()
	time.Sleep(4 * time.Second)
	a1, _, _ := d.l.Counts()
	ok1 := fl.OKs.Load()
	died := d.mode == "cluster" && (nodeExited(d.victim) || healthCode(d.victim) == 0)
	why := lastLines(grepLines(d.victim.logSince(), []string{"panic:", "fatal applying", "cannot allocate", "no space"}), 2)
	switch {
	case d.mode == "cluster" && died && leaderCase:
		r.Check(a1 > a0, "%s: the leader stopped (fail-stop) but no bank transaction was acknowledged afterwards (%d in 4 s)", label, a1-a0)
		r.Logf("%s: the leader (node %d) stopped itself (fail-stop) with: %s; the majority elected another leader and went on: %d bank transactions and %d fill rows acknowledged in 4 s", label, d.victim.ID, why, a1-a0, ok1-ok0)
	case leaderCase || d.mode == "single":
		r.Check(ok1 == ok0 && a1 == a0, "%s: %d fill and %d bank transactions were acknowledged in the 4 s after the refusal began", label, ok1-ok0, a1-a0)
		low := strings.ToLower(first + " " + last)
		r.Check(strings.Contains(low, "space") || strings.Contains(low, "disk") || strings.Contains(low, "read-only") || strings.Contains(low, "write failed"),
			"%s: the refusal does not say why: %q", label, trunc(last, 160))
		b, c, _ := d.victim.Get("/v1/health")
		r.Logf("%s: GET /v1/health -> %d %s (the refusal is a clean error at segment allocation, not a failed engine)", label, c, strings.TrimSpace(b))
		if cnt, err := scalarInt(d.victim.URL(), "SELECT count(*) FROM fill"); r.Check(err == nil, "%s: reads are not served: %v", label, err) {
			r.Logf("%s: reads still served: %d fill rows visible", label, cnt)
		}
		if d.mode == "cluster" {
			r.Logf("%s: the leader (node %d) is alive with a full disk and refuses writes: the whole cluster refused writes for these 4 s (%d fill, %d bank acknowledged); only stopping the node restores availability", label, d.victim.ID, ok1-ok0, a1-a0)
		}
	default:
		r.Check(a1 > a0, "%s: the cluster stopped acknowledging writes when the follower's disk filled (%d bank tx in 4 s)", label, a1-a0)
		r.Logf("%s: follower %d: process alive=%v; the majority went on: %d bank transactions and %d fill rows acknowledged in 4 s; follower log: %s",
			label, d.victim.ID, !died, a1-a0, ok1-ok0, why)
	}
	wl.Stop()
	fl.Stop()
	return
}

func (d *d3env) recover(label string) {
	r := d.r
	v := d.victim
	// crash while the disk is still full; restart on the full disk
	v.Kill9()
	if err := v.Start(); !r.Check(err == nil, "%s: start: %v", label, err) {
		return
	}
	if v.WaitSQL(40*time.Second) == nil {
		_, werr := scalarInt(v.URL(), "SELECT count(*) FROM acct")
		r.Logf("%s: restart on the full disk: the node starts and serves reads (%v); a write sent to it is %s", label, werr, tryWrite(v, "full"))
	} else {
		r.Logf("%s: restart on the full disk: the node does not start; log: %s", label, lastLines(v.logSince(), 3))
		r.Check(strings.TrimSpace(v.logSince()) != "", "%s: the node died on a full disk without a word", label)
	}
	os.Remove(d.ballast)
	r.Logf("%s: ballast removed: %d MB free", label, freeMB(d.mnt))
	recovered := false
	for i := 0; i < 40 && !recovered; i++ {
		if d.mode == "single" {
			recovered = tryWrite(v, "after") == "accepted"
		} else {
			recovered = v.WaitSQL(time.Second) == nil && catchUpQuick(d.t, v) && healthCode(v) == 200
		}
		if !recovered {
			time.Sleep(500 * time.Millisecond)
		}
	}
	how := "without a restart"
	if !recovered {
		how = "after a restart"
		v.Kill9()
		if !r.Check(v.Start() == nil, "%s: restart after freeing space", label) {
			return
		}
		if !r.Check(v.WaitSQL(120*time.Second) == nil, "%s: the node did not start after the space was freed: %s", label, lastLines(v.logSince(), 4)) {
			return
		}
	}
	r.Logf("%s: space freed: the node is healthy and takes part again %s", label, how)
	if d.mode == "cluster" {
		lead := d.t.Leader()
		r.Check(lead != nil && catchUp(r, v, lead, 180*time.Second), "%s: node %d did not catch up after the space was freed", label, v.ID)
	}
}

func drillD3(e *Env, o *Opts, r *Result, mode string) {
	if !sudoOK() {
		r.Skipped = "sudo -n is not available: cannot mount a small tmpfs for the full-disk drill"
		return
	}
	mnt, cleanup, err := smallMount(e, "d3-small", 96)
	if !r.Check(err == nil, "%v", err) {
		return
	}
	d := &d3env{e: e, o: o, r: r, mode: mode, mnt: mnt, ballast: filepath.Join(mnt, "ballast"), nextLo: 1}
	defer func() {
		if d.t != nil {
			d.t.KillAll()
		}
		cleanup()
	}()
	dataDir := filepath.Join(mnt, "data")
	d.t = newTopoOpt(e, "d3", mode, TopoOpts{Plain: true, Extra: []string{"-checkpoint-min-mb", "8", "-gc=false"}, Dir: func(i int) string {
		if mode == "single" || i == 2 {
			return dataDir
		}
		return ""
	}})
	d.victim = d.t.Nodes[len(d.t.Nodes)-1]
	d.l = bankStart(e, r, d.t)
	d.dsn = d.t.DSN()
	if err := exec1(d.dsn, fillDDL, "CREATE TABLE probe (id bigint PRIMARY KEY, tag text)"); !r.Check(err == nil, "tables: %v", err) {
		return
	}
	var lastMsg string
	if mode == "single" {
		if !d.makeBallast() {
			return
		}
		_, lastMsg, _ = d.fillOnce("single node", true)
		d.recover("single node")
	} else {
		// case A: the node with the small disk is a follower
		for try := 0; try < 8 && d.victim.IsLeader(); try++ {
			d.victim.Kill9()
			WaitLeader(d.t.Nodes, 30*time.Second, d.victim)
			d.victim.Start()
			d.victim.WaitSQL(60 * time.Second)
			time.Sleep(time.Second)
		}
		if !r.Check(!d.victim.IsLeader(), "could not make node %d a follower", d.victim.ID) || !d.makeBallast() {
			return
		}
		_, lastMsg, _ = d.fillOnce("cluster, follower with a full disk", false)
		d.recover("cluster, follower with a full disk")
		// case B: the node with the small disk is the leader
		for try := 0; try < 12 && !d.victim.IsLeader(); try++ {
			lead, err := WaitLeader(d.t.Nodes, 30*time.Second)
			if err != nil || lead == d.victim {
				break
			}
			lead.Kill9()
			WaitLeader(d.t.Nodes, 30*time.Second, lead)
			lead.Start()
			lead.WaitSQL(60 * time.Second)
			time.Sleep(time.Second)
		}
		if !r.Check(d.victim.IsLeader(), "could not make node %d the leader", d.victim.ID) || !d.makeBallast() {
			return
		}
		_, lastMsg, _ = d.fillOnce("cluster, leader with a full disk", true)
		// the operator's step: stop the node, the majority elects another leader
		t0 := time.Now()
		d.victim.Kill9()
		nl, err := WaitLeader(d.t.Nodes, 60*time.Second, d.victim)
		if r.Check(err == nil, "no new leader after stopping the leader whose disk is full") {
			fl := d.newFiller(300, 2)
			fl.Start()
			time.Sleep(3 * time.Second)
			fl.Stop()
			r.Check(fl.OKs.Load() > 0, "no write acknowledged after node %d was stopped (new leader %d): %s", d.victim.ID, nl.ID, fl.LastEr)
			r.Logf("cluster, leader with a full disk: stopping the full node restored write availability after %v (new leader %d, %d fill rows acknowledged in 3 s)", time.Since(t0).Round(100*time.Millisecond), nl.ID, fl.OKs.Load())
		}
		d.recover("cluster, leader with a full disk")
	}
	// new writes work, old data intact everywhere
	fl2 := d.newFiller(200, 1)
	fl2.Start()
	time.Sleep(2 * time.Second)
	fl2.Stop()
	r.Check(fl2.OKs.Load() > 0, "no write accepted after the space was freed (error: %s)", fl2.LastEr)
	if err := d.t.Settle(180 * time.Second); !r.Check(err == nil, "settle: %v", err) {
		return
	}
	for _, n := range d.t.Nodes {
		bad, sum := BankOracle(n.URL(), d.l)
		r.Check(len(bad) == 0, "node %d bank oracle: %s", n.ID, joinBad(bad))
		if n == d.victim || mode == "single" {
			r.Logf("node %d: %s", n.ID, sum)
		}
	}
	nfill := 0
	for _, f := range d.fillers {
		nfill += len(f.Acked)
		for _, n := range d.t.Nodes {
			bad := f.Verify(n.URL())
			r.Check(len(bad) == 0, "node %d fill oracle: %s", n.ID, joinBad(bad))
		}
	}
	for _, n := range d.t.Nodes {
		scrubCheck(r, n)
	}
	d.t.CheckViews(r)
	a, m, _ := d.l.Counts()
	if mode == "single" {
		r.OracleOK("disk full: writes refused with %q, reads served, restart on the full disk handled, writes accepted again once space was freed; every acknowledged write present (%d bank tx, %d unknown, %d fill rows), no phantom, scrub clean, views equal the core", trunc(lastMsg, 90), a, m, nfill)
	} else {
		r.OracleOK("disk full (3 nodes, one on a 96 MB disk): a follower and a leader with a full disk both fail-stop with 'storage failure, the node cannot continue ... no space left on device' and the majority goes on (a leader that stays alive blocks writes until stopped: finding F5); restart on the full disk handled, node rejoins after space is freed; every acknowledged write present on all nodes (%d bank tx, %d unknown, %d fill rows), no phantom, scrub clean, views equal the core", a, m, nfill)
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func healthCode(n *Node) int {
	if !n.Running() {
		return 0
	}
	_, c, err := n.Get("/v1/health")
	if err != nil {
		return 0
	}
	return c
}

func nodeExited(n *Node) bool {
	n.mu.Lock()
	done := n.done
	n.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// tryWrite sends one insert straight to the node: "accepted" or the error.
func tryWrite(n *Node, tag string) string {
	c, err := connect(n.URL())
	if err != nil {
		return "no connection: " + err.Error()
	}
	defer c.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := time.Now().UnixNano() % 1_000_000_000
	_, err = c.Exec(ctx, "INSERT INTO probe (id, tag) VALUES ($1, $2)", id, tag)
	if err != nil {
		return "refused: " + trunc(err.Error(), 120)
	}
	return "accepted"
}

func catchUpQuick(t *Topo, n *Node) bool {
	lead := t.Leader()
	if lead == nil || !n.Running() {
		return false
	}
	ls, e1 := lead.ClusterStatus()
	ns, e2 := n.ClusterStatus()
	return e1 == nil && e2 == nil && fnum(ns, "applied")+2 >= fnum(ls, "commit")
}
