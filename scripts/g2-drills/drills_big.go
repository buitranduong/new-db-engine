package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- D5: backup, encrypted backup, restore and PITR on a large table ----
//
// A node with an archive directory and a replication port loads o.Rows rows.
// Then: staskctl backup (plain) + verify + restore into a new directory +
// start + compare; the same encrypted (and a restore with no key / the wrong
// key must be refused); then point-in-time recovery: after the base backup
// 270k rows are changed in the source, the time and LSN are recorded and a
// digest of the source taken, the source goes on changing until the archive
// holds the segment of the target, and two restores (by time and by LSN) of
// the base backup + archive must equal the recorded digest. The times printed
// are indicative: taken on a shared VM, with the load average printed.

func bigLock(r *Result) (func(), error) {
	f, err := os.OpenFile("/tmp/stask-bigdata.lock", os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil, err
	}
	f.Chmod(0o666)
	t0 := time.Now()
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if time.Since(t0) > 6*time.Hour {
			f.Close()
			return nil, fmt.Errorf("waited 6 h for /tmp/stask-bigdata.lock")
		}
		if int(time.Since(t0).Seconds())%300 == 0 {
			r.Logf("waiting for /tmp/stask-bigdata.lock (another job holds it) for %v", time.Since(t0).Round(time.Second))
		}
		time.Sleep(time.Second)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (n *Node) runCtl(args ...string) (string, time.Duration, error) {
	t0 := time.Now()
	cmd := exec.Command(n.env.Ctl, args...)
	cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return string(out), time.Since(t0), err
}

const bigDDL = "CREATE TABLE big (id bigint PRIMARY KEY, grp int NOT NULL, name text NOT NULL, amount bigint NOT NULL, upd bigint NOT NULL) WITH (columnar = true)"

func loadBig(url string, rows int) error {
	c, err := connect(url)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	const chunk = 250_000
	for lo := 0; lo < rows; lo += chunk {
		hi := min(lo+chunk, rows)
		i := lo
		_, err := c.CopyFrom(context.Background(), pgx.Identifier{"big"}, []string{"id", "grp", "name", "amount", "upd"},
			pgx.CopyFromFunc(func() ([]any, error) {
				if i >= hi {
					return nil, nil
				}
				id := int64(i)
				i++
				return []any{id, int32(id % 1000), fmt.Sprintf("sim-%09d", id), id * 7 % 100003, int64(0)}, nil
			}))
		if err != nil {
			return err
		}
	}
	return nil
}

// mutate changes rows with transactions of 1000 statements: updates of a
// stretch of ids, inserts above the current maximum and deletes of a stretch.
func mutate(url string, base int, updates, inserts, deletes int, tag int64) (int, error) {
	c, err := connect(url)
	if err != nil {
		return 0, err
	}
	defer c.Close(context.Background())
	ctx := context.Background()
	ntx := 0
	for lo := 0; lo < updates; lo += 1000 {
		if _, err := c.Exec(ctx, "UPDATE big SET amount = amount + 1, upd = $1 WHERE id >= $2 AND id < $3", tag, lo*3, lo*3+1000); err != nil {
			return ntx, err
		}
		ntx++
	}
	for lo := 0; lo < inserts; lo += 10000 {
		i := lo
		_, err := c.CopyFrom(ctx, pgx.Identifier{"big"}, []string{"id", "grp", "name", "amount", "upd"},
			pgx.CopyFromFunc(func() ([]any, error) {
				if i >= lo+10000 || i >= inserts {
					return nil, nil
				}
				id := int64(base + i)
				i++
				return []any{id, int32(1), fmt.Sprintf("ins-%d", id), id, tag}, nil
			}))
		if err != nil {
			return ntx, err
		}
		ntx++
	}
	for lo := 0; lo < deletes; lo += 1000 {
		if _, err := c.Exec(ctx, "DELETE FROM big WHERE id >= $1 AND id < $2", 2_000_000+lo, 2_000_000+lo+1000); err != nil {
			return ntx, err
		}
		ntx++
	}
	return ntx, nil
}

func drillD5(e *Env, o *Opts, r *Result, mode string) {
	unlock, err := bigLock(r)
	if !r.Check(err == nil, "%v", err) {
		return
	}
	defer unlock()
	rows := o.Rows
	// source + archive + base backup + one restored copy, about 500 bytes per row in all, and 3 GB must stay free
	if need, free := int64(rows)*500>>20+3072, freeMB(e.Work); free < need {
		r.Skipped = fmt.Sprintf("only %d MB free in %s; %d rows need about %d MB (including the 3 GB that must stay free): not started", free, e.Work, rows, need)
		return
	}
	r.Logf("rows=%d; load average at the start of the drill: %s", rows, loadAvg())
	const segMB = 32
	arch := filepath.Join(e.Work, "d5-archive")
	n := e.NewNode("d5", "-gc-interval", "30s", "-segment-mb", fmt.Sprint(segMB), "-repl-addr", "", "-archive-dir", arch, "-view-save-every", "10m")
	n.args = replaceFlag(n.args, "-repl-addr", n.Repl)
	n.args = append(n.args, "-checkpoint-min-mb", "256")
	if !r.Check(n.Up() == nil, "start") {
		return
	}
	// ---- load
	t0 := time.Now()
	if err := exec1(n.URL(), bigDDL); !r.Check(err == nil, "ddl: %v", err) {
		return
	}
	if err := loadBig(n.URL(), rows); !r.Check(err == nil, "load: %v", err) {
		return
	}
	r.Logf("loaded %d rows with COPY in %v (indicative; shared VM, load average %s)", rows, time.Since(t0).Round(time.Second), loadAvg())
	r.Check(n.WaitReady(15*time.Minute) == nil, "views did not catch up")
	d0, err := digests(n.URL(), "big")
	if !r.Check(err == nil, "digest: %v", err) {
		return
	}
	r.Logf("source digest: %d rows, %x; data dir %d MB, free disk %d MB", d0["big"].n, d0["big"].sum, dirBytes(n.Dir)>>20, freeMB(e.Work))
	cnt, _ := scalarInt(n.URL(), "SELECT count(*) FROM big")
	r.Check(cnt == int64(rows), "count(*) = %d, want %d", cnt, rows)

	backupRoot := filepath.Join(e.Work, "d5-backups")
	// ---- plain backup, verify, restore
	out, dur, err := n.runCtl("backup", "-from", n.Repl, "-out", backupRoot)
	if !r.Check(err == nil, "backup: %v %s", err, trunc(out, 300)) {
		return
	}
	b1 := newestDir(backupRoot)
	r.Logf("staskctl backup: %v for %d MB (indicative; load average %s)", dur.Round(time.Second), dirBytes(b1)>>20, loadAvg())
	out, dur, err = n.runCtl("verify", "-backup", b1, "-staskd", e.Bin)
	r.Check(err == nil, "verify: %v %s", err, trunc(out, 400))
	r.Logf("staskctl verify (a real restore into a scratch directory, startup, CRC scrub, row counts): %v: %s", dur.Round(time.Second), trunc(lastLines(out, 3), 300))
	restoreAndCompare(e, r, n, "plain restore", b1, nil, d0, rows)

	// ---- encrypted backup
	keyFile := filepath.Join(e.Work, "d5.key")
	out, _, err = n.runCtl("keygen", "-out", keyFile)
	if !r.Check(err == nil, "keygen: %v %s", err, out) {
		return
	}
	encRoot := filepath.Join(e.Work, "d5-backups-enc")
	out, dur, err = n.runCtl("backup", "-from", n.Repl, "-out", encRoot, "-key-file", keyFile)
	if !r.Check(err == nil, "encrypted backup: %v %s", err, trunc(out, 300)) {
		return
	}
	b2 := newestDir(encRoot)
	r.Logf("staskctl backup (AES-256-GCM): %v for %d MB", dur.Round(time.Second), dirBytes(b2)>>20)
	// no plaintext of the data in the backup: a row text from the middle must not appear
	needle := fmt.Sprintf("sim-%09d", rows/2)
	grep := exec.Command("grep", "-rl", "--binary-files=text", needle, b2)
	gout, _ := grep.Output()
	r.Check(len(strings.TrimSpace(string(gout))) == 0, "the encrypted backup contains the plaintext %q in %s", needle, strings.TrimSpace(string(gout)))
	out, _, err = n.runCtl("verify", "-backup", b2, "-staskd", e.Bin)
	r.Check(err != nil, "verify of an encrypted backup without a key succeeded: %s", trunc(out, 200))
	wrong := filepath.Join(e.Work, "d5.wrongkey")
	n.runCtl("keygen", "-out", wrong)
	out, _, err = n.runCtl("restore", "-backup", b2, "-dir", filepath.Join(e.Work, "d5-wrong"), "-key-file", wrong)
	r.Check(err != nil, "restore with the wrong key succeeded")
	r.Logf("encrypted backup without a key / with a wrong key is refused: %s", trunc(strings.TrimSpace(lastLines(out, 1)), 160))
	os.RemoveAll(filepath.Join(e.Work, "d5-wrong"))
	restoreAndCompare(e, r, n, "encrypted restore", b2, []string{"-key-file", keyFile}, d0, rows)
	os.RemoveAll(encRoot)

	// ---- PITR
	// the source changes after the base backup b1
	ntx, err := mutate(n.URL(), rows, 150_000, 100_000, 20_000, 1)
	if !r.Check(err == nil, "mutation 1: %v", err) {
		return
	}
	tTarget := time.Now().UTC()
	lsnTarget := n.DurableLSN()
	cnt1, _ := scalarInt(n.URL(), "SELECT count(*) FROM big")
	d1, err := digests(n.URL(), "big")
	if !r.Check(err == nil, "digest at the target: %v", err) {
		return
	}
	r.Logf("PITR target recorded after %d transactions: time %s, lsn %d, %d rows, digest %x", ntx, tTarget.Format(time.RFC3339Nano), lsnTarget, cnt1, d1["big"].sum)
	time.Sleep(1500 * time.Millisecond)
	// more changes until the archive holds well beyond the target; after each step the LSN and
	// an aggregate of the table are recorded: more points a recovery must reproduce
	type point struct {
		lsn uint64
		agg [5]int64
	}
	var points []point
	segBefore := archiveCount(arch)
	for i := 0; i < 40 && archiveCount(arch) < segBefore+3; i++ {
		if _, err := mutate(n.URL(), rows+200_000+i*100_000, 60_000, 50_000, 0, int64(2+i)); err != nil {
			r.Check(false, "mutation 2: %v", err)
			return
		}
		a, err := bigAgg(n.URL())
		if !r.Check(err == nil, "aggregate: %v", err) {
			return
		}
		points = append(points, point{n.DurableLSN(), a})
	}
	d2, err := digests(n.URL(), "big")
	if !r.Check(err == nil, "final digest: %v", err) {
		return
	}
	r.Check(d2["big"] != d1["big"], "the source did not change after the target")
	finalLSN := n.DurableLSN()
	r.Logf("source went on to lsn %d in %d steps: archive holds %d segments (%d MB), final digest %d rows %x", finalLSN, len(points), archiveCount(arch), dirBytes(arch)>>20, d2["big"].n, d2["big"].sum)
	n.Term()
	os.RemoveAll(n.Dir) // the base backup + the archive are all that is left
	type target struct {
		name string
		args []string
		want map[string]digest
		agg  *point
	}
	mid := points[len(points)/2]
	for _, tg := range []target{
		{"by time " + tTarget.Format(time.RFC3339Nano), []string{"-recovery-target-time", tTarget.Format(time.RFC3339Nano)}, d1, nil},
		{fmt.Sprintf("by LSN %d", lsnTarget), []string{"-recovery-target-lsn", fmt.Sprint(lsnTarget)}, d1, nil},
		{fmt.Sprintf("by LSN %d (in the middle of the later load)", mid.lsn), []string{"-recovery-target-lsn", fmt.Sprint(mid.lsn)}, nil, &mid},
		{"to the end of the archive (no target)", nil, nil, nil},
	} {
		if freeMB(e.Work) < 3072 {
			r.Check(false, "only %d MB free: stopping before %s", freeMB(e.Work), tg.name)
			return
		}
		dir := filepath.Join(e.Work, "d5-pitr-data")
		os.RemoveAll(dir)
		out, rdur, err := n.runCtl("restore", "-backup", b1, "-dir", dir)
		if !r.Check(err == nil, "restore for PITR: %v %s", err, trunc(out, 300)) {
			return
		}
		pn := e.NewNode("d5-pitr", append([]string{"-segment-mb", fmt.Sprint(segMB), "-restore-archive", arch}, tg.args...)...)
		pn.SetDir(dir)
		t1 := time.Now()
		upErr := pn.Start()
		if upErr == nil {
			upErr = pn.WaitSQL(40 * time.Minute)
		}
		tready := time.Since(t1)
		if !r.Check(upErr == nil, "PITR %s: start: %v: %s", tg.name, upErr, lastLines(pn.logSince(), 4)) {
			return
		}
		lsnGot := pn.DurableLSN()
		eq := true
		var what string
		switch {
		case tg.want != nil:
			got, err := digests(pn.URL(), "big")
			eq = err == nil && sameDigests(tg.want, got) == ""
			r.Check(eq, "PITR %s: the restored table differs from the recorded state: %v %s", tg.name, err, sameDigests(tg.want, got))
			what = fmt.Sprintf("%d rows, row count and checksum of every row equal to the source at the target: %v", got["big"].n, eq)
		case tg.agg != nil:
			a, err := bigAgg(pn.URL())
			eq = err == nil && a == tg.agg.agg
			r.Check(eq, "PITR %s: aggregate %v differs from the recorded %v (%v)", tg.name, a, tg.agg.agg, err)
			what = fmt.Sprintf("count/sum aggregate equal to the source at that point: %v", eq)
		default:
			a, err := bigAgg(pn.URL())
			r.Check(err == nil && lsnGot >= lsnTarget && lsnGot <= finalLSN, "PITR %s: recovered to lsn %d (target %d, final %d): %v", tg.name, lsnGot, lsnTarget, finalLSN, err)
			what = fmt.Sprintf("replayed the sealed segments of the archive up to lsn %d of the source's %d (the last, unsealed segment is not archived); %d rows", lsnGot, finalLSN, a[0])
		}
		if tg.args != nil {
			want := lsnTarget
			if tg.agg != nil {
				want = tg.agg.lsn
			}
			r.Check(lsnGot+1 >= want && lsnGot <= want+1, "PITR %s: recovered to lsn %d, target %d", tg.name, lsnGot, want)
		}
		r.Logf("PITR %s: restore copy %v, then staskd with -restore-archive was serving after %v (indicative; load average %s); recovered to lsn %d; %s", tg.name, rdur.Round(time.Second), tready.Round(time.Second), loadAvg(), lsnGot, what)
		rep, rerr := pn.WaitInSync(10 * time.Minute)
		r.Check(rerr == nil && rep.Verdict == "IN_SYNC", "PITR %s: views: %v", tg.name, rerr)
		wres := tryWriteBig(pn)
		r.Check(wres == "accepted", "PITR %s: the restored node does not accept writes: %s", tg.name, wres)
		pn.Term()
		os.RemoveAll(dir)
	}
	r.OracleOK("%d-row table: plain and AES-256-GCM encrypted backups verified, restored into new directories and equal to the source (row count and checksum of every row); encrypted backup holds no plaintext and is refused without/with a wrong key; PITR by time and by LSN of base backup + archive equals the source state recorded at that point (%d rows, same checksum of every row), a third LSN in the middle of the later load reproduces its recorded aggregate, a replay with no target stops at the end of the sealed archive; every restored node is in sync (reconcile -full) and takes writes", rows, cnt1)
	os.RemoveAll(backupRoot)
	os.RemoveAll(arch)
}

func tryWriteBig(n *Node) string {
	err := exec1(n.URL(), "INSERT INTO big VALUES (-1, 0, 'after recovery', 0, 0)")
	if err != nil {
		return "refused: " + trunc(err.Error(), 100)
	}
	return "accepted"
}

func loadAvg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "?"
	}
	f := strings.Fields(string(b))
	if len(f) >= 3 {
		return strings.Join(f[:3], " ")
	}
	return strings.TrimSpace(string(b))
}

func replaceFlag(args []string, flag, val string) []string {
	for i := range args {
		if args[i] == flag && i+1 < len(args) {
			args[i+1] = val
		}
	}
	return args
}

func newestDir(root string) string {
	ents, _ := os.ReadDir(root)
	var best string
	for _, e := range ents {
		if e.IsDir() && e.Name() > best {
			best = e.Name()
		}
	}
	return filepath.Join(root, best)
}

func archiveCount(dir string) int {
	ents, _ := os.ReadDir(dir)
	return len(ents)
}

// restoreAndCompare restores a backup into a new directory, starts a node on it
// and compares the table with the source digest.
func restoreAndCompare(e *Env, r *Result, src *Node, label, backup string, keyArgs []string, want map[string]digest, rows int) {
	dir := filepath.Join(e.Work, "d5-restore")
	os.RemoveAll(dir)
	args := append([]string{"restore", "-backup", backup, "-dir", dir}, keyArgs...)
	out, rdur, err := src.runCtl(args...)
	if !r.Check(err == nil, "%s: restore: %v %s", label, err, trunc(out, 300)) {
		return
	}
	pn := e.NewNode("d5-restored", "-segment-mb", "32", "-checkpoint-min-mb", "256")
	pn.SetDir(dir)
	t1 := time.Now()
	upErr := pn.Start()
	if upErr == nil {
		upErr = pn.WaitSQL(40 * time.Minute)
	}
	if !r.Check(upErr == nil, "%s: start: %v %s", label, upErr, lastLines(pn.logSince(), 4)) {
		return
	}
	tup := time.Since(t1)
	pn.WaitReady(10 * time.Minute)
	got, err := digests(pn.URL(), "big")
	eq := err == nil && sameDigests(want, got) == ""
	r.Check(eq, "%s: the restored table differs from the source: %v %s", label, err, sameDigests(want, got))
	rep, rerr := pn.WaitInSync(10 * time.Minute)
	r.Check(rerr == nil && rep.Verdict == "IN_SYNC", "%s: views of the restored node: %v", label, rerr)
	r.Logf("%s: restore into a new directory %v, staskd serving after %v (indicative; load average %s); %d rows, row count and checksum equal to the source: %v; views in sync", label, rdur.Round(time.Second), tup.Round(time.Millisecond), loadAvg(), got["big"].n, eq)
	pn.Term()
	os.RemoveAll(dir)
}

// bigAgg is a cheap aggregate of the whole table.
func bigAgg(url string) ([5]int64, error) {
	c, err := connect(url)
	if err != nil {
		return [5]int64{}, err
	}
	defer c.Close(context.Background())
	var a [5]int64
	err = c.QueryRow(context.Background(), "SELECT count(*), sum(id), sum(grp), sum(amount), sum(upd) FROM big").Scan(&a[0], &a[1], &a[2], &a[3], &a[4])
	return a, err
}
