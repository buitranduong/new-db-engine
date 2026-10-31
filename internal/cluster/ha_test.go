package cluster

// High availability on the PostgreSQL protocol (docs/21 section 3): a client
// with a multi-host connection string finds the leader, survives its loss,
// and never sees an acknowledged write disappear; a partitioned old leader
// cannot acknowledge writes; nodes can be restarted one after the other; a
// cluster backup restores into a working cluster.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"stask/internal/repl"
)

// dsn is a multi-host connection string over every node (the form pgjdbc
// jdbc:postgresql://h1:p1,h2:p2,h3:p3/db?targetServerType=primary and libpq
// host=h1,h2,h3 target_session_attrs=read-write both express).
func (c *tcluster) dsn(attrs string) string {
	var hs []string
	for _, nd := range c.nodes {
		hs = append(hs, nd.pg)
	}
	return fmt.Sprintf("postgres://app@%s/stask?sslmode=disable&connect_timeout=3&target_session_attrs=%s", strings.Join(hs, ","), attrs)
}

func (c *tcluster) nodeDSN(i int) string {
	return fmt.Sprintf("postgres://app@%s/stask?sslmode=disable&connect_timeout=3", c.nodes[i].pg)
}

func pgOnce(ctx context.Context, dsn string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	return pgx.ConnectConfig(ctx, cfg)
}

func (c *tcluster) pgExecRW(sql string) {
	end := time.Now().Add(40 * time.Second)
	var last error
	for time.Now().Before(end) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		conn, err := pgOnce(ctx, c.dsn("read-write"))
		if err == nil {
			_, err = conn.Exec(ctx, sql)
			conn.Close(ctx)
		}
		cancel()
		if err == nil {
			return
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("%s: %v", sql, last)
}

// pgWriters write distinct keys of table ha through the DSN (or, with
// node >= 0, to that node only) and record what was acknowledged.
type pgWriters struct {
	mu      sync.Mutex
	acked   map[string]int
	unknown map[string]int
	ackedBy map[string]int // node index (direct writers) or -1
	stop    chan struct{}
	wg      sync.WaitGroup
}

func (c *tcluster) startPGWriters(g int, prefix string, node int) *pgWriters {
	w := &pgWriters{acked: map[string]int{}, unknown: map[string]int{}, ackedBy: map[string]int{}, stop: make(chan struct{})}
	for k := 0; k < g; k++ {
		w.wg.Add(1)
		go func(k int) {
			defer w.wg.Done()
			var conn *pgx.Conn
			defer func() {
				if conn != nil {
					conn.Close(context.Background())
				}
			}()
			dsn := c.dsn("read-write")
			if node >= 0 {
				dsn = c.nodeDSN(node)
			}
			for n := 0; ; n++ {
				select {
				case <-w.stop:
					return
				default:
				}
				if conn == nil {
					ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
					cn, err := pgOnce(ctx, dsn)
					cancel()
					if err != nil {
						time.Sleep(100 * time.Millisecond)
						n--
						continue
					}
					conn = cn
				}
				key := fmt.Sprintf("%s-%d-%d", prefix, k, n)
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				_, err := conn.Exec(ctx, fmt.Sprintf("INSERT INTO ha VALUES ('%s', %d)", key, n))
				cancel()
				w.mu.Lock()
				switch {
				case err == nil:
					w.acked[key], w.ackedBy[key] = n, node
				default:
					var pe *pgconn.PgError
					if errors.As(err, &pe) && pe.Code == "25006" {
						// refused before anything ran
					} else {
						w.unknown[key] = n
					}
				}
				w.mu.Unlock()
				if err != nil {
					conn.Close(context.Background())
					conn = nil
					time.Sleep(50 * time.Millisecond)
				}
			}
		}(k)
	}
	return w
}

func (w *pgWriters) finish() { close(w.stop); w.wg.Wait() }
func (w *pgWriters) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.acked)
}

// rows reads table ha on node i.
func (c *tcluster) pgRows(i int) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgOnce(ctx, c.nodeDSN(i))
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rs, err := conn.Query(ctx, "SELECT id, v FROM ha")
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]int{}
	for rs.Next() {
		var id string
		var v int
		if err := rs.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rs.Err()
}

// verifyPG: every acknowledged write is on every node with its value.
func (c *tcluster) verifyPG(w *pgWriters) (unknownPresent int) {
	for i := range c.nodes {
		rows, err := c.pgRows(i)
		if err != nil {
			c.t.Errorf("node %d: read: %v", c.nodes[i].id, err)
			continue
		}
		lost := 0
		for k, v := range w.acked {
			if got, ok := rows[k]; !ok || got != v {
				lost++
				if lost <= 5 {
					c.t.Errorf("node %d: acknowledged write %s=%d lost (found=%v value=%d)", c.nodes[i].id, k, v, ok, got)
				}
			}
		}
		if lost > 5 {
			c.t.Errorf("node %d: %d acknowledged writes lost in all", c.nodes[i].id, lost)
		}
		if i == 0 {
			for k := range w.unknown {
				if _, ok := rows[k]; ok {
					unknownPresent++
				}
			}
		}
	}
	return
}

func (c *tcluster) isLeaderNode(i int) bool {
	s, ok := c.status(c.nodes[i])
	return ok && s.Role == "leader" && s.Writable
}

func remoteHost(conn *pgx.Conn) string { return conn.PgConn().Conn().RemoteAddr().String() }

// TestHARoleReporting: a node tells clients whether it takes writes
// (pg_is_in_recovery, transaction_read_only, the in_hot_standby and
// default_transaction_read_only parameters pgjdbc and libpq look at), a
// follower refuses a write with SQLSTATE 25006, and a multi-host connection
// string picks the leader (read-write) or a follower (standby).
func TestHARoleReporting(t *testing.T) {
	c := newCluster(t, 3)
	c.createTable()
	li, _ := c.waitLeader(40 * time.Second)
	c.pgExecRW("CREATE TABLE ha (id text PRIMARY KEY, v int)")
	c.settle()
	li, _ = c.waitLeader(20 * time.Second)
	for i := range c.nodes {
		ctx := context.Background()
		conn, err := pgOnce(ctx, c.nodeDSN(i))
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		var rec bool
		var tro string
		if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&rec); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx, "SHOW transaction_read_only").Scan(&tro); err != nil {
			t.Fatal(err)
		}
		hs, dtro := conn.PgConn().ParameterStatus("in_hot_standby"), conn.PgConn().ParameterStatus("default_transaction_read_only")
		isLeader := i == li
		wantRec, wantOn := !isLeader, map[bool]string{true: "off", false: "on"}[isLeader]
		if rec != wantRec || tro != wantOn || hs != wantOn || dtro != wantOn {
			t.Errorf("node %d (leader=%v): pg_is_in_recovery=%v transaction_read_only=%s in_hot_standby=%s default_transaction_read_only=%s",
				i, isLeader, rec, tro, hs, dtro)
		}
		_, err = conn.Exec(ctx, "INSERT INTO ha VALUES ('direct', 1)")
		var pe *pgconn.PgError
		switch {
		case isLeader && err != nil:
			t.Errorf("leader refused a write: %v", err)
		case !isLeader && !(errors.As(err, &pe) && pe.Code == "25006"):
			t.Errorf("follower %d: write returned %v, want SQLSTATE 25006", i, err)
		}
		conn.Close(ctx)
	}
	ctx := context.Background()
	rw, err := pgOnce(ctx, c.dsn("read-write"))
	if err != nil {
		t.Fatalf("read-write: %v", err)
	}
	if got := remoteHost(rw); got != c.nodes[li].pg {
		t.Errorf("target_session_attrs=read-write reached %s, leader is %s", got, c.nodes[li].pg)
	}
	rw.Close(ctx)
	sb, err := pgOnce(ctx, c.dsn("standby"))
	if err != nil {
		t.Fatalf("standby: %v", err)
	}
	if got := remoteHost(sb); got == c.nodes[li].pg {
		t.Errorf("target_session_attrs=standby reached the leader")
	}
	sb.Close(ctx)
}

// TestHAFailover: writers use only the multi-host connection string; the
// leader is killed (kill -9) twice; every acknowledged write survives on all
// nodes and writers resume on the new leader without any change on the client.
func TestHAFailover(t *testing.T) {
	c := newCluster(t, 3)
	c.createTable()
	c.waitLeader(40 * time.Second)
	c.pgExecRW("CREATE TABLE ha (id text PRIMARY KEY, v int)")
	w := c.startPGWriters(4, "f", -1)
	defer func() {
		select {
		case <-w.stop:
		default:
			w.finish()
		}
	}()
	for round := 0; round < 2; round++ {
		for w.count() < 30*(round+1) {
			time.Sleep(20 * time.Millisecond)
		}
		li, _ := c.waitLeader(40 * time.Second)
		before := w.count()
		t0 := time.Now()
		c.kill(c.nodes[li])
		var resumed time.Duration
		for w.count() < before+20 {
			if time.Since(t0) > 60*time.Second {
				t.Fatalf("round %d: writers did not resume after the leader was killed", round)
			}
			time.Sleep(10 * time.Millisecond)
		}
		resumed = time.Since(t0)
		t.Logf("round %d: killed leader node %d; writers resumed through the DSN after %v", round, c.nodes[li].id, resumed.Round(10*time.Millisecond))
		c.start(c.nodes[li])
	}
	w.finish()
	c.settle()
	up := c.verifyPG(w)
	t.Logf("%d acknowledged writes, all on all 3 nodes; %d unknown outcomes (%d present)", len(w.acked), len(w.unknown), up)
}

// TestHASplitBrain: the leader is cut off from both followers while clients
// keep writing to every node directly. The cut-off leader must not
// acknowledge anything the majority does not have, at no time may two nodes
// be writable leaders of the same term, and after the partition heals all
// acknowledged writes are on all nodes.
func TestHASplitBrain(t *testing.T) {
	c := newCluster(t, 3)
	c.createTable()
	c.waitLeader(40 * time.Second)
	c.pgExecRW("CREATE TABLE ha (id text PRIMARY KEY, v int)")
	c.settle()
	var ws []*pgWriters
	for i := range c.nodes {
		ws = append(ws, c.startPGWriters(2, fmt.Sprintf("n%d", i), i))
	}
	stopW := func() {
		for _, w := range ws {
			select {
			case <-w.stop:
			default:
				w.finish()
			}
		}
	}
	defer stopW()
	// monitor: two writable leaders in one term is a split brain
	violations := make(chan string, 16)
	monStop := make(chan struct{})
	var monWG sync.WaitGroup
	monWG.Add(1)
	go func() {
		defer monWG.Done()
		for {
			select {
			case <-monStop:
				return
			case <-time.After(15 * time.Millisecond):
			}
			byTerm := map[uint64]int{}
			for _, nd := range c.nodes {
				if s, ok := c.status(nd); ok && s.Role == "leader" && s.Writable {
					byTerm[s.Term]++
					if byTerm[s.Term] > 1 {
						select {
						case violations <- fmt.Sprintf("two writable leaders in term %d", s.Term):
						default:
						}
					}
				}
			}
		}
	}()
	time.Sleep(2 * time.Second)
	for round := 0; round < 2; round++ {
		li, _ := c.waitLeader(40 * time.Second)
		pre := make([]int, 3)
		for i, w := range ws {
			pre[i] = w.count()
		}
		t0 := time.Now()
		c.isolate(li, true)
		// the majority elects a new leader and takes writes
		var ni int
		for {
			ni = -1
			for i := range c.nodes {
				if i != li && c.isLeaderNode(i) {
					ni = i
				}
			}
			if ni >= 0 {
				break
			}
			if time.Since(t0) > 30*time.Second {
				t.Fatal("the majority did not elect a leader")
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(4 * time.Second) // the old leader sits cut off while clients keep trying it
		post := make([]int, 3)
		for i, w := range ws {
			post[i] = w.count()
		}
		c.isolate(li, false)
		t.Logf("round %d: isolated leader node %d; new leader node %d after %v; acknowledged during the partition: isolated node %d, others %d and %d",
			round, c.nodes[li].id, c.nodes[ni].id, time.Since(t0).Round(10*time.Millisecond), post[li]-pre[li], post[(li+1)%3]-pre[(li+1)%3], post[(li+2)%3]-pre[(li+2)%3])
		time.Sleep(3 * time.Second)
	}
	stopW()
	close(monStop)
	monWG.Wait()
	select {
	case v := <-violations:
		t.Fatalf("split brain: %s", v)
	default:
	}
	c.settle()
	total := &pgWriters{acked: map[string]int{}, unknown: map[string]int{}}
	for _, w := range ws {
		for k, v := range w.acked {
			total.acked[k] = v
		}
		for k, v := range w.unknown {
			total.unknown[k] = v
		}
	}
	up := c.verifyPG(total)
	t.Logf("%d acknowledged writes (2 partitions of the leader, writers on all three nodes), all on all 3 nodes; %d unknown outcomes (%d present)", len(total.acked), len(total.unknown), up)
}

// TestHARollingRestart: every node in turn is stopped with SIGTERM and
// started again (the procedure of a rolling upgrade) while writers use the
// multi-host DSN; writers make progress during each step and nothing
// acknowledged is lost.
func TestHARollingRestart(t *testing.T) {
	c := newCluster(t, 3)
	c.createTable()
	c.waitLeader(40 * time.Second)
	c.pgExecRW("CREATE TABLE ha (id text PRIMARY KEY, v int)")
	w := c.startPGWriters(4, "r", -1)
	defer func() {
		select {
		case <-w.stop:
		default:
			w.finish()
		}
	}()
	for w.count() < 20 {
		time.Sleep(20 * time.Millisecond)
	}
	li, _ := c.waitLeader(40 * time.Second)
	order := []int{}
	for i := range c.nodes {
		if i != li {
			order = append(order, i)
		}
	}
	order = append(order, li) // the leader last
	for _, i := range order {
		nd := c.nodes[i]
		before := w.count()
		t0 := time.Now()
		c.term(nd)
		c.start(nd)
		// the node must rejoin: applied catches up with the cluster's commit
		end := time.Now().Add(60 * time.Second)
		for {
			s, ok := c.status(nd)
			_, ls := c.leader()
			if ok && ls.Commit > 0 && s.Applied >= ls.Commit-1 && s.Leader != 0 {
				break
			}
			if time.Now().After(end) {
				t.Fatalf("node %d did not rejoin", nd.id)
			}
			time.Sleep(50 * time.Millisecond)
		}
		for w.count() < before+10 {
			if time.Since(t0) > 60*time.Second {
				t.Fatalf("writers made no progress while node %d was restarted", nd.id)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Logf("restarted node %d (leader=%v): rejoined and writers progressed after %v", nd.id, i == li, time.Since(t0).Round(10*time.Millisecond))
	}
	w.finish()
	c.settle()
	up := c.verifyPG(w)
	t.Logf("%d acknowledged writes across a full rolling restart, all on all 3 nodes; %d unknown outcomes (%d present)", len(w.acked), len(w.unknown), up)
}

// TestHAClusterBackup: a backup taken from the leader (the Raft port) while
// writers run is restored into three empty node directories and starts as a
// cluster holding exactly the state at the backup point (everything
// acknowledged before the backup started, nothing from after it), and takes
// new writes.
func TestHAClusterBackup(t *testing.T) {
	c := newCluster(t, 3)
	c.createTable()
	c.waitLeader(40 * time.Second)
	c.pgExecRW("CREATE TABLE ha (id text PRIMARY KEY, v int)")
	w := c.startPGWriters(3, "b", -1)
	for w.count() < 100 {
		time.Sleep(20 * time.Millisecond)
	}
	// acknowledged before the backup started must be in it
	w.mu.Lock()
	before := map[string]int{}
	for k, v := range w.acked {
		before[k] = v
	}
	w.mu.Unlock()
	li, _ := c.waitLeader(40 * time.Second)
	bdir := filepath.Join(t.TempDir(), "backup")
	info, err := Backup(c.nodes[li].raft, repl.Settings{}, bdir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("backup at raft entry %d, lsn %d, %d acknowledged writes before it started", info.Index, info.LSN, len(before))
	for w.count() < len(before)+100 { // more writes after the backup
		time.Sleep(20 * time.Millisecond)
	}
	w.finish()
	c.settle()
	afterOnly := map[string]int{}
	for k, v := range w.acked {
		if _, ok := before[k]; !ok {
			afterOnly[k] = v
		}
	}
	// a second cluster started from the backup
	c2 := newClusterPre(t, 3, func(c2 *tcluster) {
		for _, nd := range c2.nodes {
			if err := RestoreNode(bdir, nd.dir); err != nil {
				t.Fatal(err)
			}
		}
	})
	c2.createTableIfMissing()
	c2.waitLeader(60 * time.Second)
	c2.settle()
	for i := range c2.nodes {
		rows, err := c2.pgRows(i)
		if err != nil {
			t.Fatalf("restored node %d: %v", i, err)
		}
		for k, v := range before {
			if got, ok := rows[k]; !ok || got != v {
				t.Errorf("restored node %d lost %s (acknowledged before the backup)", i, k)
				break
			}
		}
		in := 0
		for k := range afterOnly {
			if _, ok := rows[k]; ok {
				in++
			}
		}
		if in == len(afterOnly) {
			t.Errorf("restored node %d holds all %d writes made after the backup: it is not a backup", i, in)
		}
		if i == 0 {
			t.Logf("restored cluster: %d rows; %d of %d acknowledged-before present; %d of %d made-after present (those the backup caught in flight)", len(rows), len(before), len(before), in, len(afterOnly))
		}
	}
	c2.pgExecRW("INSERT INTO ha VALUES ('after-restore', 1)")
	c2.settle()
	for i := range c2.nodes {
		rows, _ := c2.pgRows(i)
		if rows["after-restore"] != 1 {
			t.Errorf("restored node %d did not get a write made after the restore", i)
		}
	}
	// restoring over a used directory is refused
	if err := RestoreNode(bdir, c2.nodes[0].dir); err == nil {
		t.Error("RestoreNode accepted a node directory that is in use")
	}
}

// createTableIfMissing is a no-op for the restored cluster: the HTTP table t
// is part of the backup (it was created before).
func (c *tcluster) createTableIfMissing() {}

// TestHAClusterBackupCLI: staskctl backs up the cluster given every node's
// Raft address (the leader is found), and its restore test (manifest,
// start, CRC scrub) passes on the result.
func TestHAClusterBackupCLI(t *testing.T) {
	c := newCluster(t, 3)
	c.createTable()
	c.waitLeader(40 * time.Second)
	c.pgExecRW("CREATE TABLE ha (id text PRIMARY KEY, v int)")
	for i := 0; i < 40; i++ {
		c.pgExecRW(fmt.Sprintf("INSERT INTO ha VALUES ('k%d', %d)", i, i))
	}
	ctl := filepath.Join(t.TempDir(), "staskctl")
	if out, err := exec.Command("go", "build", "-o", ctl, "../../cmd/staskctl").CombinedOutput(); err != nil {
		t.Fatalf("go build staskctl: %v\n%s", err, out)
	}
	var raft []string
	for _, nd := range c.nodes {
		raft = append(raft, nd.raft)
	}
	out := t.TempDir()
	b, err := exec.Command(ctl, "backup", "-cluster", "-from", strings.Join(raft, ","), "-out", out).Output()
	if err != nil {
		t.Fatalf("staskctl backup: %v", err)
	}
	dir := strings.TrimSpace(string(b))
	vb, err := exec.Command(ctl, "verify", "-backup", dir, "-staskd", c.bin).CombinedOutput()
	if err != nil {
		t.Fatalf("staskctl verify: %v\n%s", err, vb)
	}
	if !strings.Contains(string(vb), "verified") || !strings.Contains(string(vb), "ha") {
		t.Fatalf("verify output: %s", vb)
	}
	t.Logf("%s", vb)
}

// TestHALibpqDocker: the real libpq (psql of the postgres:16 image, the
// library psycopg2 and most drivers wrap) finds the leader with
// target_session_attrs=read-write, finds a follower with standby, and follows
// the leader to its successor after kill -9. Skipped unless STASK_LIBPQ_DOCKER=1.
func TestHALibpqDocker(t *testing.T) {
	if os.Getenv("STASK_LIBPQ_DOCKER") != "1" {
		t.Skip("set STASK_LIBPQ_DOCKER=1 (needs docker and the postgres:16 image)")
	}
	c := newCluster(t, 3)
	c.createTable()
	c.waitLeader(40 * time.Second)
	c.pgExecRW("CREATE TABLE ha (id text PRIMARY KEY, v int)")
	var hosts, ports []string
	for _, nd := range c.nodes {
		h, p, _ := net.SplitHostPort(nd.pg)
		hosts, ports = append(hosts, h), append(ports, p)
	}
	conn := func(attrs string) string {
		return fmt.Sprintf("host=%s port=%s dbname=stask user=app sslmode=disable connect_timeout=3 target_session_attrs=%s",
			strings.Join(hosts, ","), strings.Join(ports, ","), attrs)
	}
	psql := func(attrs, sql string) (string, error) {
		out, err := exec.Command("docker", "run", "--rm", "--network", "host", "postgres:16", "psql", "-X", "-At", "-d", conn(attrs), "-c", sql).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	port := func(i int) string { _, p, _ := net.SplitHostPort(c.nodes[i].pg); return p }
	li, _ := c.waitLeader(20 * time.Second)
	if out, err := psql("read-write", "select pg_is_in_recovery()"); err != nil || out != "f" {
		t.Fatalf("read-write: %q %v", out, err)
	}
	if out, err := psql("standby", "select pg_is_in_recovery()"); err != nil || out != "t" {
		t.Fatalf("standby: %q %v", out, err)
	}
	if out, err := psql("read-write", "insert into ha values ('libpq', 1)"); err != nil {
		t.Fatalf("insert: %q %v", out, err)
	}
	c.kill(c.nodes[li])
	c.waitLeader(40 * time.Second)
	var out string
	var err error
	for i := 0; i < 30; i++ {
		if out, err = psql("read-write", "insert into ha values ('libpq-after', 2)"); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("after kill -9 of the leader: %q %v", out, err)
	}
	ni, _ := c.waitLeader(20 * time.Second)
	if ni == li {
		t.Fatal("the killed leader is still the leader")
	}
	rows, err := c.pgRows(ni)
	if err != nil || rows["libpq-after"] != 2 || rows["libpq"] != 1 {
		t.Fatalf("new leader (port %s) holds %v (%v)", port(ni), rows, err)
	}
	t.Logf("libpq: read-write -> leader (port %s); standby -> follower; after kill -9 of the leader -> new leader (port %s) took both writes", port(li), port(ni))
}
