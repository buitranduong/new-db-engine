// staskd runs the STASK prototype server.
package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"stask/internal/conf"
	"stask/internal/tune"
	"strconv"
	"strings"
	"syscall"
	"time"

	"stask/internal/cluster"
	"stask/internal/core"
	"stask/internal/histstore"
	"stask/internal/pgwire"
	"stask/internal/planner"
	"stask/internal/proj"
	"stask/internal/repl"
	"stask/internal/server"
	"stask/internal/spill"
	"stask/internal/sql"
	"stask/internal/wal"
)

func main() {
	dir := flag.String("dir", "/mnt/sda3/stask-data/db", "data directory")
	addr := flag.String("addr", "127.0.0.1:17070", "listen address")
	seg := flag.Int64("segment-mb", 256, "log segment size (MB)")
	batch := flag.Int("max-batch", 512, "max transactions per group commit")
	ckptMin := flag.Int64("checkpoint-min-mb", 512, "minimum log growth between checkpoints (MB)")
	noCkpt := flag.Bool("no-checkpoint", false, "disable background checkpoints")
	compress := flag.String("compress", "off", "compress sealed log segments: off, s2 or zstd")
	compressKeep := flag.Int("compress-keep-raw", 1, "newest sealed log segments left uncompressed")
	compressCache := flag.Int64("compress-cache", 64, "MB of decompressed blocks cached for reads of compressed segments")
	slotMaxLag := flag.Int64("slot-max-lag-mb", 0, "invalidate a CDC slot that holds back more log than this, as PostgreSQL's max_slot_wal_keep_size (0 = no limit)")
	viewEvery := flag.Duration("view-save-every", 10*time.Minute, "save the columnar view to <dir>/columnar.view this often while data changes, and at shutdown, so a start loads it and replays only the log after it (0 = default, negative = only at shutdown)")
	noViewFile := flag.Bool("no-view-file", false, "do not save or load the columnar view file: rebuild it from the data at every start")
	colPack := flag.Bool("columnar-pack", true, "store full chunks of integer columns of columnar views in 1, 2 or 4 bytes per value (frame of reference); false keeps 8 (4 for dictionary codes)")
	preZero := flag.Bool("prezero", true, "write zeros into log segments ahead of use: fdatasync then needs no extent conversion (about half the commit latency) at the cost of writing the log twice")
	zeroRate := flag.Int64("zero-rate-mb", 150, "cap for background zeroing (MB/s)")
	gc := flag.Bool("gc", true, "reclaim log space (relocate current versions out of old segments, then delete them)")
	gcRetentionS := flag.String("history-retention", "1h", "history kept for as-of reads and /v1/history when -gc is on: 36h, 60d, \"60 days\" (a table's ALTER TABLE ... SET (history_retention=...) overrides it)")
	histStore := flag.Bool("hist-store", false, "keep the update history of -hist-store-tables in the history store <dir>/histstore (compact, time-bucketed, expired by whole segment; docs/19). Default off. With it, set the tables' history_retention to hours: the log then needs no deep chains and GC stays cheap")
	histStoreTables := flag.String("hist-store-tables", "", "comma list of tables whose history goes to the history store (needs -hist-store)")
	histStoreRetentionS := flag.String("hist-store-retention", "60d", "how long the history store keeps versions (36h, 60d, ...)")
	histStoreBucket := flag.Duration("hist-store-bucket", time.Hour, "time span of one base segment of the history store; 24 of them are merged into one daily segment")
	gcInterval := flag.Duration("gc-interval", 10*time.Second, "time between log reclamation rounds")
	gcRate := flag.Int64("gc-rate-mb", 64, "cap for relocation writes (MB/s)")
	gcLive := flag.Float64("gc-max-live", 0.5, "clean segments whose still-current data is at most this share")
	replAddr := flag.String("repl-addr", "", "address replicas connect to (empty = off)")
	replicaOf := flag.String("replica-of", "", "run as a read-only replica of the leader whose -repl-addr this is")
	replSecret := flag.String("repl-secret", "", "secret shared by the leader and its replicas, proved by challenge-response both ways (never sent); prefer -repl-secret-file or the -config file, the command line is visible to other users")
	replSecretFile := flag.String("repl-secret-file", "", "file holding -repl-secret")
	replTLSCert := flag.String("repl-tls-cert", "", "PEM certificate: the leader's for -repl-addr, the replica's client certificate with -replica-of (with -repl-tls-key)")
	replTLSKey := flag.String("repl-tls-key", "", "PEM private key of -repl-tls-cert")
	replTLSCA := flag.String("repl-tls-ca", "", "PEM CA: a replica dials TLS and verifies the leader's certificate and name with it (verify-full); the leader verifies client certificates with it (-repl-tls-client-auth)")
	replTLSName := flag.String("repl-tls-server-name", "", "name the leader's certificate must carry (default: the host of -replica-of)")
	replTLSClientAuth := flag.Bool("repl-tls-client-auth", false, "leader: require replicas to present a certificate signed by -repl-tls-ca (mTLS); plaintext replicas are then refused")
	replRequire := flag.Bool("repl-require-secure", false, "leader: refuse replicas that are not both on TLS and authenticated (secret or client certificate); replica: refuse a leader without TLS")
	replInsecure := flag.Bool("repl-insecure", false, "allow -repl-addr on a non-loopback address without replica authentication")
	raftID := flag.Uint64("raft-id", 0, "run as a node of a Raft cluster with this id (0 = off: a single node, unchanged). A write is acknowledged when a majority has fsynced it; the leader is elected, and failover needs no operator. Uses the -repl-secret*/-repl-tls-* settings for the node-to-node port (docs/06 section 5)")
	raftPeers := flag.String("raft-peers", "", "every node of the cluster: id=raftHost:port[@httpHost:port], comma separated, this node included")
	raftListen := flag.String("raft-listen", "", "address for Raft connections (default: this node's address in -raft-peers)")
	raftTick := flag.Duration("raft-tick", 50*time.Millisecond, "Raft tick; the election timeout is -raft-election-ticks to twice that many ticks")
	raftElection := flag.Int("raft-election-ticks", 20, "ticks without a leader before a node asks for votes (at least 4)")
	raftCompact := flag.Int("raft-compact-entries", 20000, "compact the Raft log when it holds more entries than this")
	raftKeep := flag.Int("raft-compact-keep", 2000, "entries kept behind the applied one when compacting (followers further behind fetch a snapshot)")
	raftInsecure := flag.Bool("raft-insecure", false, "allow -raft-listen on a non-loopback address without authentication")
	pgAddr := flag.String("pg-addr", "127.0.0.1:17432", "PostgreSQL protocol listen address (empty = off)")
	pgInsecure := flag.Bool("pg-insecure", false, "allow -pg-addr on a non-loopback address without a password (-pg-password)")
	// Off by default: the lane needs GOMAXPROCS above the CPU count, and the
	// GC then runs idle mark workers on the extra Ps, taking most of the
	// real CPUs for each mark phase (20M rows: write p99 313ms with the
	// lane, 43ms without; docs/08_memory.md).
	heavyWorkers := flag.Int("heavy-workers", -2, "OS threads that run statements classified heavy (multi-row writes, scans that the columnar view does not answer cheaply); point statements run on their session. -2 (default) = automatic: none below 4 CPUs, else a quarter of the CPUs (1 to 4); -1 = the usable CPUs; 0 = no lanes")
	heavyShare := flag.Float64("heavy-share", 0.25, "fraction of the CPUs the heavy lane is guaranteed while the node is saturated and the most it takes then (user-space token bucket, no nice needed); 0 = off, the lane then relies on -heavy-nice / -cpu-groups")
	admission := flag.Bool("admission", false, "queue statements beyond -admission-slots, reads before writes (measured: no gain on the mixed benchmark, where reads wait on CPU, not on slots)")
	admissionSlots := flag.Int("admission-slots", 0, "statements executing at once before others queue by priority (0 = adapt to scheduler latency)")
	runnableThreshold := flag.Float64("runnable-threshold", 4, "with -heavy-cap: runnable goroutines per CPU (averaged over 10ms) at or above which the heavy lane is capped (Go 1.26+). Aggregate helpers yield to waiting goroutines by themselves; with older Go they are cut on -sched-target instead")
	heavyCap := flag.Float64("heavy-cap", 0, "with -cpu-groups: CPUs the heavy lane may use while the CPU is overloaded (see -runnable-threshold); 0 = no cap")
	heavyCapPeriod := flag.Duration("heavy-cap-period", 10*time.Millisecond, "period over which -heavy-cap is enforced (shorter: shorter stalls of capped threads)")
	helperBackoff := flag.Duration("helper-backoff", 100*time.Microsecond, "a full-scan aggregate helper that sees goroutines waiting for a CPU sleeps this long and looks again (0 = never yields)")
	scanMin := flag.Int("scan-min-helpers", 1, "helpers a full-scan aggregate keeps even when scheduler latency is high (with -cpu-groups their CPU share is guaranteed by -heavy-weight)")
	schedTarget := flag.Duration("sched-target", time.Millisecond, "scheduler latency p99 above which aggregate helpers are cut")
	scanWorkers := flag.Int("scan-workers", 0, "most cores one full-scan aggregate uses (1 = sequential, 0 = the usable CPUs)")
	heavyNice := flag.Int("heavy-nice", 10, "nice value of the heavy-lane threads when CPU groups are off (Linux)")
	cpuGroups := flag.Bool("cpu-groups", true, "split a saturated CPU between foreground work and the heavy lane by cgroup v2 weights (needs a delegated cgroup; falls back to -heavy-nice)")
	fgWeight := flag.Int("fg-weight", 1000, "cpu.weight of the foreground group")
	heavyWeight := flag.Int("heavy-weight", 50, "cpu.weight of the heavy-lane group")
	pgMaxConns := flag.Int64("pg-max-conns", 100000, "maximum PostgreSQL sessions; more are refused with SQLSTATE 53300 (0 = unlimited)")
	pgPassword := flag.String("pg-password", "", "password of PostgreSQL connections (empty = trust); sent per -pg-auth")
	pgAuth := flag.String("pg-auth", "", "PostgreSQL authentication: trust, password (cleartext) or scram (SCRAM-SHA-256, verifier derived from -pg-password); empty = password when -pg-password is set, else trust")
	pgTLSCert := flag.String("pg-tls-cert", "", "PEM certificate for PostgreSQL TLS (with -pg-tls-key; empty = SSLRequest answered 'N')")
	pgTLSKey := flag.String("pg-tls-key", "", "PEM private key for PostgreSQL TLS")
	pgTLSRequire := flag.Bool("pg-tls-require", false, "refuse unencrypted PostgreSQL connections (SQLSTATE 28000, like hostssl-only pg_hba.conf)")
	pprofAddr := flag.String("pprof", "", "serve Go profiling endpoints on this address (empty = off; loopback only unless -pprof-insecure)")
	pprofInsecure := flag.Bool("pprof-insecure", false, "allow -pprof on a non-loopback address")
	var hs server.SecurityConfig
	flag.StringVar(&hs.Tokens, "http-token", "", "comma-separated bearer tokens with full access to the HTTP API (empty = none)")
	flag.StringVar(&hs.ROTokens, "http-token-ro", "", "comma-separated bearer tokens limited to GET endpoints and POST /v1/query")
	flag.StringVar(&hs.TokenFile, "http-token-file", "", "file of bearer tokens, one per line (\"ro <token>\" = read-only); re-read when it changes and on SIGHUP")
	flag.BoolVar(&hs.Basic, "http-basic", false, "also accept HTTP Basic auth whose password is -pg-password (any user name), full access")
	flag.StringVar(&hs.TLSCert, "http-tls-cert", "", "PEM certificate: serve the HTTP API over TLS (with -http-tls-key)")
	flag.StringVar(&hs.TLSKey, "http-tls-key", "", "PEM private key for -http-tls-cert")
	flag.StringVar(&hs.TLSClientCA, "http-tls-client-ca", "", "PEM CA bundle: require HTTP clients to present a certificate it signed (mTLS; alone it is the authentication)")
	flag.BoolVar(&hs.HTTPInsecure, "http-insecure", false, "allow -addr on a non-loopback address with no authentication")
	commitWait := flag.Duration("commit-wait", time.Millisecond, "max wait for returning clients to join a commit batch (0 = off; adaptive, never longer than one fdatasync)")
	gcPercent := flag.Int("go-gc", 400, "Go GC target percent unless GOGC is set (higher = less GC CPU, more memory)")
	gcHeadroom := flag.Int64("go-gc-headroom", 2048, "MB the heap may grow past its live size between collections: lowers -go-gc on large heaps (0 = fixed -go-gc)")
	gcMin := flag.Int("go-gc-min", 25, "lowest GC percent -go-gc-headroom may set")
	var stmtTimeout, idleInTxTimeout, idleTimeout time.Duration
	flag.Var(pgTime{&stmtTimeout}, "statement-timeout", "default statement_timeout of PostgreSQL sessions: cancel statements running longer (SQLSTATE 57014); ms without a unit, or us/ms/s/min/h/d; 0 = off")
	flag.Var(pgTime{&idleInTxTimeout}, "idle-in-transaction-session-timeout", "default idle_in_transaction_session_timeout: end sessions idle inside a transaction this long (SQLSTATE 25P03), releasing their snapshot; 0 = off")
	flag.Var(pgTime{&idleTimeout}, "idle-session-timeout", "default idle_session_timeout: end sessions idle outside a transaction this long (SQLSTATE 57P05); 0 = off")
	slowQuery := flag.Duration("slow-query", 0, "log SQL statements that take at least this long, with user, pid and text (PostgreSQL's log_min_duration_statement; 0 = off). Passwords of CREATE/ALTER ROLE are never logged")
	pgRolesOnly := flag.Bool("pg-roles-only", false, "once a stored superuser role that can log in exists (CREATE ROLE ... SUPERUSER LOGIN PASSWORD), refuse the shared -pg-password login: only roles connect")
	scrubEvery := flag.Duration("scrub-interval", 0, "run the CRC scrub of log segments and checkpoints this often (0 = only on POST /v1/admin/scrub); errors are logged and counted in /metrics")
	scrubRate := flag.Int64("scrub-rate-mb", 32, "read cap of the background scrub (MB/s; 0 = unthrottled)")
	maxSnapAge := flag.Duration("max-snapshot-age", 24*time.Hour, "a stale transaction snapshot older than this stops holding back log reclamation; its transaction then fails with snapshot too old (0 = no cap)")
	pgKeepIdle := flag.Duration("pg-tcp-keepalives-idle", time.Minute, "idle time before TCP keepalive probes on PostgreSQL connections (0 = system default)")
	pgKeepInterval := flag.Duration("pg-tcp-keepalives-interval", 10*time.Second, "time between TCP keepalive probes (0 = system default)")
	pgKeepCount := flag.Int("pg-tcp-keepalives-count", 6, "unanswered keepalive probes before the connection is dropped (0 = system default)")
	pgUserTimeout := flag.Duration("pg-tcp-user-timeout", 0, "TCP_USER_TIMEOUT of PostgreSQL connections (Linux): drop a connection whose sent data stays unacknowledged this long (0 = system default)")
	archiveDir := flag.String("archive-dir", "", "copy every sealed log segment to this directory (continuous archiving, as PostgreSQL's archive_command); log reclamation waits for it")
	backupFrom := flag.String("backup-from", "", "take a base backup of the leader whose -repl-addr this is into -dir, then exit (uses the -repl-* security settings)")
	restoreArchive := flag.String("restore-archive", "", "point-in-time recovery: before opening -dir (a base backup), add the log segments it lacks from this archive and replay to the recovery target")
	targetTime := flag.String("recovery-target-time", "", "with -restore-archive: stop replay before the first commit after this time (RFC 3339)")
	targetLSN := flag.Uint64("recovery-target-lsn", 0, "with -restore-archive: stop replay after this LSN")
	queryMem := flag.Int64("query-mem-mb", 0, "memory one SQL query may hold in rows it materializes (scans, joins) before failing with 53200 (0 = half of -sql-mem-mb, -1 = no limit)")
	spillOn := flag.Bool("spill", true, "a query past its memory budget spills to temp files in -spill-dir (external sort, Grace hash join, partitioned aggregate and DISTINCT) instead of failing with 53200; false keeps the failure")
	spillDir := flag.String("spill-dir", "", "directory of temp files of spilling queries (default: <dir>/spill; emptied of leftovers at startup)")
	spillMax := flag.Int64("spill-max-mb", 0, "temp data one query may write (MB), then 53100 (0 = no limit)")
	spillOp := flag.Int64("spill-op-mb", 0, "memory one spilling operator (join, aggregate, sort) keeps before it spills (MB; 0 = a sixth of -query-mem-mb)")
	sqlMem := flag.Int64("sql-mem-mb", 0, "memory all SQL queries together may hold before new allocations fail with 53200 (0 = a quarter of the heap limit, -1 = no limit)")
	memLimit := flag.Int64("mem-limit-mb", 0, "soft Go heap limit unless GOMEMLIMIT is set (0 = half of the usable memory: RAM or the container's limit)")
	procs := flag.Int("procs", 0, "GOMAXPROCS (0 = Go default, the number of CPUs)")
	faults := flag.Bool("enable-faults", false, "enable the fault-injection admin endpoint (tests only)")
	collation := flag.String("collation", "C", "database default text collation: C (byte order, the default), POSIX, en_US.utf8, vi_VN.utf8, und-x-icu, or another CLDR locale (x_Y.utf8, x-x-icu). Orders ORDER BY, < <= > >=, BETWEEN, min/max; equality, GROUP BY, DISTINCT and unique keys stay bytewise. Indexes are byte-ordered, so ORDER BY and text ranges on a non-C collation do not use them (docs/14 Appendix W). COLLATE \"name\" overrides per expression")
	confPath := flag.String("config", os.Getenv("STASK_CONFIG"), "settings file: one \"name = value\" per line, names as these flags; flags on the command line win (default $STASK_CONFIG)")
	flag.Parse()
	if *confPath != "" {
		if err := conf.Load(flag.CommandLine, *confPath); err != nil {
			log.Fatalf("config: %v", err)
		}
		log.Printf("config: loaded %s", *confPath)
	}
	if err := sql.SetDefaultCollation(*collation); err != nil {
		log.Fatalf("-collation: %v", err)
	}
	hs.BasicPass = *pgPassword
	httpAuth, httpTLS := httpSecurity(hs, *addr, *pprofAddr, *pprofInsecure, *faults)
	if *procs > 0 {
		runtime.GOMAXPROCS(*procs)
	}
	// usable CPUs: affinity lowered by a container's cpu.max
	cpus := tune.WholeCPUs()
	switch {
	case *heavyWorkers == -2:
		*heavyWorkers = sql.AutoHeavyWorkers(cpus)
		log.Printf("heavy lane: -heavy-workers not set: automatic %d for %d CPUs", *heavyWorkers, cpus)
	case *heavyWorkers < 0:
		*heavyWorkers = cpus
	}
	sql.HeavyShare = max(0, min(1, *heavyShare))
	if sql.HeavyShare > 0 {
		niceSet := false
		flag.Visit(func(f *flag.Flag) { niceSet = niceSet || f.Name == "heavy-nice" })
		if !niceSet {
			*heavyNice = 0 // the scheduler, not the kernel priority, protects point statements
		}
	}
	if *scanWorkers <= 0 {
		*scanWorkers = cpus
	}
	if *heavyWorkers > 0 && *procs <= 0 {
		// heavy-lane threads hold Ps while descheduled by the kernel: keep
		// as many Ps again for point statements and the commit pipeline
		runtime.GOMAXPROCS(cpus + *heavyWorkers)
	}
	sql.ScanWorkers = max(1, *scanWorkers)
	sql.HelperBackoff = *helperBackoff
	sql.ScanMinHelpers, sql.SchedTarget, sql.RunnableThreshold, sql.HeavyCapCores = *scanMin, *schedTarget, *runnableThreshold, *heavyCap
	sql.HeavyCapPeriod = *heavyCapPeriod
	if *cpuGroups && *heavyWorkers != 0 {
		if err := sql.StartCPUGroups(*fgWeight, *heavyWeight); err != nil {
			log.Printf("cpu groups off (heavy lane uses nice %d): %v", *heavyNice, err)
		} else {
			log.Printf("cpu groups on: foreground weight %d, heavy weight %d", *fgWeight, *heavyWeight)
		}
	}
	sql.StartHeavyLane(*heavyWorkers, *heavyNice)
	if *admission {
		sql.StartAdmission(*admissionSlots)
		go func() { // what admission did, per class, every 10s
			var prev sql.AdmissionStats
			for range time.Tick(10 * time.Second) {
				st, _ := sql.GetAdmissionStats()
				qr, qw := st.Queued[sql.ClassRead]-prev.Queued[sql.ClassRead], st.Queued[sql.ClassWrite]-prev.Queued[sql.ClassWrite]
				if qr+qw > 0 {
					log.Printf("admission: slots=%d queued read=%d write=%d avg wait ms read=%.2f write=%.2f", st.Slots, qr, qw,
						(st.WaitMs[sql.ClassRead]-prev.WaitMs[sql.ClassRead])/float64(max(qr, 1)),
						(st.WaitMs[sql.ClassWrite]-prev.WaitMs[sql.ClassWrite])/float64(max(qw, 1)))
				}
				prev = st
			}
		}()
	}
	sql.StartScanControl()
	go sql.RuntimeStats(10*time.Second, log.Printf)
	if *slowQuery > 0 {
		sql.SetSlowQueryLog(*slowQuery, log.Printf)
	}
	// Fewer GC cycles shorten commit tail latency; the soft limit makes the
	// collector run more often instead of letting the heap grow past it.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(*gcPercent)
		if *gcHeadroom > 0 {
			(&tune.GCTuner{Headroom: *gcHeadroom << 20, MinPercent: *gcMin, MaxPercent: *gcPercent}).Start()
		}
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		limit := *memLimit << 20
		if limit == 0 {
			// RAM lowered by a container's memory.max: half of it
			limit = tune.Memory() / 2
		}
		if limit > 0 {
			debug.SetMemoryLimit(limit)
		}
	}
	if heap := debug.SetMemoryLimit(-1); heap > 0 && heap < math.MaxInt64 {
		switch {
		case *sqlMem == 0:
			sql.SQLMemLimit = heap / 4
		case *sqlMem > 0:
			sql.SQLMemLimit = *sqlMem << 20
		}
	}
	switch {
	case *queryMem == 0:
		sql.QueryMemLimit = sql.SQLMemLimit / 2
	case *queryMem > 0:
		sql.QueryMemLimit = *queryMem << 20
	}

	if *spillOn {
		sql.SpillDir = *spillDir
		if sql.SpillDir == "" {
			sql.SpillDir = filepath.Join(*dir, "spill")
		}
		if n := spill.CleanStale(sql.SpillDir, true); n > 0 {
			log.Printf("spill: removed %d leftover temp directories from %s", n, sql.SpillDir)
		}
		sql.SpillMaxBytes, sql.SpillOpMem = *spillMax<<20, *spillOp<<20
	}

	codec, err := wal.ParseCodec(*compress)
	if err != nil {
		log.Fatalf("-compress: %v", err)
	}
	wal.ZCacheBytes = *compressCache << 20
	gcRetention, rerr := core.ParseRetention(*gcRetentionS)
	if rerr != nil {
		log.Fatalf("-history-retention: %v", rerr)
	}
	proj.PackColumns = *colPack
	t0 := time.Now()
	var fault *server.Fault
	opts := core.Options{ViewSaveEvery: *viewEvery, NoViewFile: *noViewFile, Dir: *dir, SegmentSize: *seg << 20, MaxBatch: *batch,
		CheckpointMinBytes: *ckptMin << 20, NoCheckpoint: *noCkpt, PreZero: *preZero, ZeroRate: *zeroRate << 20,
		GC: *gc, GCRetention: gcRetention, GCInterval: *gcInterval, GCRateBytes: *gcRate << 20, GCMaxLiveRatio: *gcLive, CommitWait: *commitWait,
		Compression: codec, CompressKeepRaw: *compressKeep, SlotMaxLagBytes: *slotMaxLag << 20, MaxSnapshotAge: *maxSnapAge,
		ArchiveDir: *archiveDir, RestoreArchive: *restoreArchive, RecoveryTargetLSN: *targetLSN}
	if *faults {
		fault = &server.Fault{}
		opts.Fault = fault
	}
	replSettings := repl.Settings{Secret: *replSecret, SecretFile: *replSecretFile, CertFile: *replTLSCert, KeyFile: *replTLSKey,
		CAFile: *replTLSCA, ServerName: *replTLSName, ClientAuth: *replTLSClientAuth, Require: *replRequire}
	var leaderSec, replicaSec *repl.Security
	if *replAddr != "" {
		if leaderSec, err = replSettings.Leader(); err != nil {
			log.Fatalf("repl: %v", err)
		}
		if err := repl.CheckListen(*replAddr, leaderSec, *replInsecure); err != nil {
			log.Fatalf("repl: %v", err)
		}
	}
	if *replicaOf != "" {
		if replicaSec, err = replSettings.Replica(*replicaOf); err != nil {
			log.Fatalf("replica: %v", err)
		}
		segMax, err := repl.Prepare(*replicaOf, *dir, replicaSec, log.Printf)
		if err != nil {
			log.Fatalf("replica: %v", err)
		}
		opts.Replica, opts.SegmentSize, opts.GC = true, segMax, false
	}
	var cl *cluster.Cluster
	if *raftID != 0 {
		if *replicaOf != "" || *replAddr != "" || *backupFrom != "" || *restoreArchive != "" {
			log.Fatal("-raft-id cannot be combined with -replica-of, -repl-addr, -backup-from or -restore-archive")
		}
		peers, httpAddrs, err := parseRaftPeers(*raftPeers)
		if err != nil {
			log.Fatalf("-raft-peers: %v", err)
		}
		if *gc {
			log.Printf("raft: -gc is not supported in a cluster yet (docs/06 section 5.2): log reclamation is off")
		}
		if *archiveDir != "" {
			log.Fatal("-archive-dir is not supported in a cluster yet")
		}
		opts.GC = false
		cl, err = cluster.Prepare(cluster.Config{ID: *raftID, Peers: peers, HTTP: httpAddrs, Listen: *raftListen, Dir: *dir,
			SegMax: opts.SegmentSize, Settings: replSettings, Insecure: *raftInsecure, TickInterval: *raftTick,
			ElectionTicks: *raftElection, CompactEntries: *raftCompact, CompactKeep: *raftKeep, Logf: log.Printf})
		if err != nil {
			log.Fatalf("raft: %v", err)
		}
		opts.Cluster, opts.Cut, opts.CutLSN, opts.Quorum = true, true, cl.CutLSN(), cl.Quorum()
	}
	if *backupFrom != "" {
		sec, err := replSettings.Replica(*backupFrom)
		if err != nil {
			log.Fatalf("backup: %v", err)
		}
		if _, err := repl.BaseBackup(*backupFrom, *dir, sec, log.Printf); err != nil {
			log.Fatalf("backup: %v", err)
		}
		return
	}
	if b, err := os.ReadFile(repl.BackupPath(*dir)); err == nil {
		// a base backup: its log has the leader's segment size
		var bi repl.BackupInfo
		if err := json.Unmarshal(b, &bi); err != nil {
			log.Fatalf("%s: %v", repl.BackupPath(*dir), err)
		}
		opts.SegmentSize = bi.SegMax
	}
	if *targetTime != "" {
		t, err := time.Parse(time.RFC3339Nano, *targetTime)
		if err != nil {
			log.Fatalf("-recovery-target-time: %v", err)
		}
		opts.RecoveryTargetTS = t.UnixNano()
	}
	if (opts.RecoveryTargetTS != 0 || opts.RecoveryTargetLSN != 0) && *restoreArchive == "" {
		log.Fatal("a recovery target needs -restore-archive")
	}
	if *restoreArchive != "" && *restoreArchive == *archiveDir {
		log.Fatal("-archive-dir of a restored database must be a new directory: its log after the target differs from the archived one")
	}
	e, err := core.Open(opts)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	if cl != nil {
		if err := cl.Start(e); err != nil {
			log.Fatalf("raft: %v", err)
		}
	}
	replStop := make(chan struct{})
	if *replicaOf != "" {
		go repl.Follow(e, *replicaOf, replicaSec, replStop, log.Printf)
	}
	var rs *repl.Server
	if *replAddr != "" {
		if rs, err = repl.Serve(e, *replAddr, leaderSec, log.Printf); err != nil {
			log.Fatalf("repl listen: %v", err)
		}
		log.Printf("replicas connect to %s", rs.Addr())
	}
	p := planner.Start(e)
	var histSt *histstore.Store
	if *histStore {
		histSt = startHistStore(e, *dir, *histStoreTables, *histStoreRetentionS, *histStoreBucket, *replicaOf != "" || cl != nil)
	}
	go memoryStats(e, 10*time.Second)
	hsrv := server.New(p, fault)
	if cl != nil {
		hsrv.Cluster = func() any { return cl.Status() }
	}
	srv := &http.Server{Addr: *addr, Handler: httpAuth.Wrap(hsrv.Handler()), TLSConfig: httpTLS}
	if *pprofAddr != "" {
		runtime.SetMutexProfileFraction(20)
		runtime.SetBlockProfileRate(int(50 * time.Microsecond))
		// same credentials as the API when it has any (full access)
		go func() { log.Printf("pprof: %v", http.ListenAndServe(*pprofAddr, httpAuth.Wrap(http.DefaultServeMux))) }()
	}
	var pg *pgwire.Server
	if *pgAddr != "" {
		if err := server.CheckBind(*pgAddr, *pgPassword != "", *pgInsecure, "-pg-addr",
			"set -pg-password (with -pg-auth scram) or pass -pg-insecure"); err != nil {
			log.Fatal(err)
		}
		ln, err := net.Listen("tcp", *pgAddr)
		if err != nil {
			log.Fatalf("pg listen: %v", err)
		}
		pg = &pgwire.Server{E: e, Password: *pgPassword, Auth: *pgAuth, TLSRequire: *pgTLSRequire, MaxConns: *pgMaxConns, Views: p, RolesOnly: *pgRolesOnly,
			StatementTimeout: stmtTimeout, IdleInTxTimeout: idleInTxTimeout, IdleSessionTimeout: idleTimeout,
			KeepAliveIdle: *pgKeepIdle, KeepAliveInterval: *pgKeepInterval, KeepAliveCount: *pgKeepCount, TCPUserTimeout: *pgUserTimeout}
		if (*pgTLSCert == "") != (*pgTLSKey == "") {
			log.Fatalf("pg tls: -pg-tls-cert and -pg-tls-key go together")
		}
		if *pgTLSCert != "" {
			if pg.TLS, err = pgwire.LoadTLS(*pgTLSCert, *pgTLSKey); err != nil {
				log.Fatalf("pg tls: %v", err)
			}
		}
		go func() {
			if err := pg.Serve(ln); err != nil {
				log.Fatalf("pg: %v", err)
			}
		}()
		log.Printf("postgres protocol on %s", *pgAddr)
	}
	if *scrubEvery > 0 {
		go func() {
			for range time.Tick(*scrubEvery) {
				if e.StartScrub(*scrubRate << 20) {
					log.Printf("scrub: started")
				}
			}
		}()
	}
	log.Printf("staskd ready on %s (dir=%s, lsn=%d, startup=%v)", *addr, *dir, e.DurableLSN(), time.Since(t0))

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		if pg != nil {
			pg.Close()
		}
		close(replStop)
		if cl != nil {
			cl.Close()
		}
		if rs != nil {
			rs.Close()
		}
		srv.Close()
	}()
	if httpTLS != nil {
		err = srv.ListenAndServeTLS("", "")
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	e.Close()
	if histSt != nil {
		histSt.Close()
	}
}

// startHistStore opens the history store and follows the log with it. It is
// only reached with -hist-store; any error is fatal (a silently missing
// history would be worse than not starting).
func startHistStore(e *core.Engine, dir, tables, retention string, bucket time.Duration, replicaOrCluster bool) *histstore.Store {
	if replicaOrCluster {
		log.Fatal("-hist-store: not supported on a replica or a cluster node (the position it saves is a leader log position)")
	}
	var names []string
	for _, t := range strings.Split(tables, ",") {
		if t = strings.TrimSpace(t); t != "" {
			names = append(names, t)
		}
	}
	if len(names) == 0 {
		log.Fatal("-hist-store needs -hist-store-tables")
	}
	ret, err := core.ParseRetention(retention)
	if err != nil || ret <= 0 {
		log.Fatalf("-hist-store-retention: %q: %v", retention, err)
	}
	hs, err := histstore.Open(histstore.Options{Dir: filepath.Join(dir, "histstore"), Retention: ret, Bucket: bucket, Logf: log.Printf})
	if err != nil {
		log.Fatalf("history store: %v", err)
	}
	c := histstore.NewConsumer(hs, names...)
	e.Follow(c)
	c.Attach(e)
	log.Printf("history store on for %v, retention %v, dir %s", names, ret, filepath.Join(dir, "histstore"))
	return hs
}

// httpSecurity builds TLS and authentication of the HTTP API from the
// flags and refuses listeners reachable from the network with no
// authentication (docs/12_plan.md §P9 S2/S3).
func httpSecurity(hs server.SecurityConfig, addr, pprofAddr string, pprofInsecure, faults bool) (*server.Auth, *tls.Config) {
	auth, tc, err := hs.Build(log.Printf)
	if err != nil {
		log.Fatalf("http security: %v", err)
	}
	if err := server.CheckBind(addr, auth.Enabled(), hs.HTTPInsecure, "-addr",
		"configure -http-token, -http-token-file, -http-basic or -http-tls-client-ca, or pass -http-insecure"); err != nil {
		log.Fatal(err)
	}
	if err := server.CheckBind(pprofAddr, false, pprofInsecure, "-pprof", "bind it to 127.0.0.1 or pass -pprof-insecure"); err != nil {
		log.Fatal(err)
	}
	if !auth.Enabled() && !server.IsLoopback(addr) {
		log.Printf("WARNING: HTTP API on %s without authentication (-http-insecure)", addr)
	}
	if pprofAddr != "" && !server.IsLoopback(pprofAddr) {
		log.Printf("WARNING: profiling endpoints on non-loopback %s (-pprof-insecure)", pprofAddr)
	}
	if faults {
		log.Printf("WARNING: -enable-faults: POST /v1/admin/fault can make every write fail (tests only)")
	}
	if hs.TokenFile != "" {
		go auth.Watch(time.Second, nil)
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			for range hup {
				if err := auth.Reload(true); err != nil {
					log.Printf("http auth: SIGHUP reload: %v (previous tokens kept)", err)
				}
			}
		}()
	}
	log.Printf("http api: tls=%v mtls=%v auth=%v", tc != nil, hs.TLSClientCA != "", auth.Enabled())
	return auth, tc
}

// memoryStats logs, every interval, where reads find their data: page
// faults that had to read the disk (major), and how much of the log
// segments mapped for reading is in memory, next to the Go heap.
func memoryStats(e *core.Engine, every time.Duration) {
	majflt := func() int64 {
		b, err := os.ReadFile("/proc/self/stat")
		if err != nil {
			return 0
		}
		// fields after the parenthesized command name; majflt is field 12
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
		if len(f) < 10 {
			return 0
		}
		v, _ := strconv.ParseInt(f[9], 10, 64)
		return v
	}
	prev := majflt()
	var ms runtime.MemStats
	for range time.Tick(every) {
		cur := majflt()
		mapped, resident := e.Log().Residency()
		runtime.ReadMemStats(&ms)
		pct := 0.0
		if mapped > 0 {
			pct = 100 * float64(resident) / float64(mapped)
		}
		log.Printf("memory: heap %d MiB (limit %d MiB)  log mapped %d MiB, resident %.1f%%  major faults +%d  sql aggregates from views %d",
			ms.HeapAlloc>>20, debug.SetMemoryLimit(-1)>>20, mapped>>20, pct, cur-prev, sql.ViewAggregates.Load())
		prev = cur
	}
}

// pgTime is a flag holding a PostgreSQL time setting ('5s', '1500', '2min').
type pgTime struct{ d *time.Duration }

func (p pgTime) String() string {
	if p.d == nil {
		return "0"
	}
	return sql.FormatTimeout(*p.d)
}

func (p pgTime) Set(v string) error {
	d, err := sql.ParseTimeout(v)
	if err == nil {
		*p.d = d
	}
	return err
}

// parseRaftPeers reads "1=host:port@httphost:port,2=...".
func parseRaftPeers(s string) (raft map[uint64]string, http map[uint64]string, err error) {
	raft, http = map[uint64]string{}, map[uint64]string{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		id, addr, ok := strings.Cut(f, "=")
		n, perr := strconv.ParseUint(id, 10, 64)
		if !ok || perr != nil || n == 0 || addr == "" {
			return nil, nil, fmt.Errorf("%q is not id=host:port[@httphost:port]", f)
		}
		r, h, _ := strings.Cut(addr, "@")
		raft[n] = r
		if h != "" {
			http[n] = h
		}
	}
	if len(raft) == 0 {
		return nil, nil, errors.New("no peers")
	}
	return raft, http, nil
}
