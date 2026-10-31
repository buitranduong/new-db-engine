// staskctl is the operator's tool for a STASK node or cluster (docs/21):
//
//	staskctl status  -url http://n1:8080,http://n2:8080     role, readiness, lag of each node (exit 1 if any is not ready)
//	staskctl backup  -from ADDR -out DIR [-cluster]         base backup of a leader (-repl-addr) or a cluster (Raft port) into DIR/<time>
//	staskctl verify  -backup DIR/<time>                     restore into a scratch directory, start staskd on it, scrub, count rows
//	staskctl restore -backup DIR/<time> -dir NEWDIR         put a backup into an empty data directory (cluster backup: a node directory)
//	staskctl restore -backup DIR/<time> -dir NEWDIR -standalone   a cluster backup as a single-node data directory (point-in-time recovery: start staskd on it with -restore-archive)
//	staskctl cluster add|remove -http URL[,URL...] -id N [-raft ADDR -node-http ADDR]   change the voters of a Raft cluster by one node
//	staskctl backup-loop -from ADDR -out DIR -interval 6h   backup, verify, prune, write a Prometheus textfile; forever or -once
//	staskctl keygen [-out FILE]                              a new 32-byte backup/archive encryption key (hex)
//	staskctl audit verify|query -dir DIR [-key-file F]       check the hash chain of the audit trail / list its records (docs/22)
//
// backup, verify, restore and backup-loop encrypt/decrypt with AES-256-GCM
// when given -key-file (or $STASK_BACKUP_KEY); the key is never stored in the
// backup (docs/23).
//
// A backup is a directory with a manifest (sha256 of every file) next to the
// files; verify checks the manifest first, then really starts the server on a
// copy, so a backup that cannot be restored is found by a machine, not on the
// day it is needed.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"stask/internal/cluster"
	"stask/internal/repl"
	"stask/internal/seccrypt"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "status":
		err = cmdStatus(args)
	case "backup":
		err = cmdBackup(args)
	case "verify":
		err = cmdVerify(args)
	case "restore":
		err = cmdRestore(args)
	case "cluster":
		err = cmdCluster(args)
	case "backup-loop":
		err = cmdLoop(args)
	case "keygen":
		err = cmdKeygen(args)
	case "audit":
		err = cmdAudit(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "staskctl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: staskctl status|backup|verify|restore|cluster|backup-loop [flags]  (staskctl <cmd> -h)")
	os.Exit(2)
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s staskctl: %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

// secFlags are the -repl-* settings of staskd (the backup connection is a
// replication connection and is secured the same way).
type secFlags struct{ s repl.Settings }

func (f *secFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&f.s.Secret, "repl-secret", "", "shared replication secret (prefer -repl-secret-file)")
	fs.StringVar(&f.s.SecretFile, "repl-secret-file", "", "file holding the shared replication secret")
	fs.StringVar(&f.s.CertFile, "repl-tls-cert", "", "client certificate (PEM)")
	fs.StringVar(&f.s.KeyFile, "repl-tls-key", "", "client key (PEM)")
	fs.StringVar(&f.s.CAFile, "repl-tls-ca", "", "CA that signed the server's certificate (turns TLS on, verify-full)")
	fs.StringVar(&f.s.ServerName, "repl-tls-server-name", "", "name the server's certificate must carry")
	fs.BoolVar(&f.s.Require, "repl-require-secure", false, "refuse a server without TLS")
}

// ---- status ----

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	urls := fs.String("url", "http://127.0.0.1:8080", "comma-separated HTTP API URLs of the nodes")
	token := fs.String("token", os.Getenv("STASK_TOKEN"), "API token (for /v1/cluster and /v1/status)")
	var tf tlsFlags
	tf.add(fs)
	fs.Parse(args)
	hc, herr := tf.client(5 * time.Second)
	if herr != nil {
		return herr
	}
	get := func(u, path string, out any) (int, error) {
		req, _ := http.NewRequest("GET", strings.TrimRight(u, "/")+path, nil)
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}
		r, err := hc.Do(req)
		if err != nil {
			return 0, err
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if out != nil {
			json.Unmarshal(b, out)
		}
		return r.StatusCode, nil
	}
	bad := 0
	for _, u := range strings.Split(*urls, ",") {
		u = strings.TrimSpace(u)
		var rd struct {
			Ready    bool     `json:"ready"`
			ReadOnly bool     `json:"read_only"`
			Reasons  []string `json:"reasons"`
		}
		code, err := get(u, "/v1/ready", &rd)
		if err != nil {
			fmt.Printf("%-28s DOWN (%v)\n", u, err)
			bad++
			continue
		}
		role := "primary"
		if rd.ReadOnly {
			role = "read-only"
		}
		line := fmt.Sprintf("%-28s ready=%v role=%s", u, rd.Ready, role)
		var cl struct {
			Role            string
			Term, Leader    uint64
			Commit, Applied uint64
			Writable        bool
		}
		if c, _ := get(u, "/v1/cluster", &cl); c == 200 {
			line += fmt.Sprintf(" raft=%s term=%d leader=%d commit=%d applied=%d", cl.Role, cl.Term, cl.Leader, cl.Commit, cl.Applied)
		}
		var st struct {
			DurableLSN uint64 `json:"durable_lsn"`
			Wal        int    `json:"wal_segments"`
		}
		if c, _ := get(u, "/v1/status", &st); c == 200 {
			line += fmt.Sprintf(" lsn=%d segments=%d", st.DurableLSN, st.Wal)
		}
		if code != 200 {
			line += " NOT READY: " + strings.Join(rd.Reasons, "; ")
			bad++
		}
		fmt.Println(line)
	}
	if bad > 0 {
		return fmt.Errorf("%d node(s) down or not ready", bad)
	}
	return nil
}

// ---- backup ----

// Manifest describes a backup directory.
type Manifest struct {
	Kind     string            `json:"kind"` // single or cluster
	Time     time.Time         `json:"time"`
	From     string            `json:"from"`
	LSN      uint64            `json:"lsn,omitempty"`
	RaftIdx  uint64            `json:"raft_index,omitempty"`
	SegMax   int64             `json:"segment_size"`
	Bytes    int64             `json:"bytes"`
	Files    map[string]string `json:"sha256"` // of the files as stored (ciphertext when Encrypted)
	Duration float64           `json:"seconds"`
	// Encrypted: every file is AES-256-GCM sealed (internal/seccrypt).
	// KeyID identifies the key (never the key); MAC is an HMAC of this
	// manifest under the key, so the file list cannot be edited unnoticed.
	Encrypted bool   `json:"encrypted,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	MAC       string `json:"manifest_mac,omitempty"`
}

func (m Manifest) mac(key []byte) string {
	m.MAC = ""
	b, _ := json.Marshal(m)
	h := hmac.New(sha256.New, key)
	h.Write([]byte("stask-manifest\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// keyFlags: where the encryption key comes from.
type keyFlags struct{ file string }

func (k *keyFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&k.file, "key-file", "", "AES-256 key file (64 hex digits, base64 or 32 raw bytes) for encrypted backups; default $STASK_BACKUP_KEY. Never stored in the backup")
}

func (k keyFlags) load() ([]byte, error) { return seccrypt.LoadKey(k.file) }

// encryptTree seals every file of src into dst (same relative names).
func encryptTree(src, dst string, key []byte) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		return seccrypt.EncryptFile(p, filepath.Join(dst, rel), key)
	})
}

// decryptTree opens every file of src into dst; the first failure (wrong key,
// modified file) aborts.
func decryptTree(src, dst string, key []byte) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if rel == manifestName {
			return nil
		}
		if err := seccrypt.DecryptFile(p, filepath.Join(dst, rel), key); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		return nil
	})
}

// checkKey compares the key with the one the backup was made with, before
// any data is touched.
func (m *Manifest) checkKey(key []byte) error {
	switch {
	case m.Encrypted && len(key) == 0:
		return fmt.Errorf("the backup is encrypted (key id %s): give -key-file or set $%s", m.KeyID, seccrypt.EnvKey)
	case m.Encrypted && seccrypt.KeyID(key) != m.KeyID:
		return fmt.Errorf("wrong encryption key: the backup was made with key id %s, this key is %s", m.KeyID, seccrypt.KeyID(key))
	case m.Encrypted && m.MAC != m.mac(key):
		return errors.New("the manifest of the encrypted backup was modified (authentication failed)")
	}
	return nil
}

const manifestName = "manifest.json"

func hashTree(dir string) (map[string]string, int64, error) {
	out := map[string]string{}
	var total int64
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == manifestName {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		total += n
		out[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return out, total, err
}

func doBackup(from, out string, cl bool, sec repl.Settings, key []byte) (string, *Manifest, error) {
	t0 := time.Now()
	final := filepath.Join(out, t0.UTC().Format("20060102T150405Z"))
	dir := final
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", nil, err
	}
	if len(key) > 0 {
		// the base backup lands in a private scratch directory next to the
		// result and is sealed file by file; the plaintext is removed at once
		parent := final + ".plain"
		os.RemoveAll(parent)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return "", nil, err
		}
		defer os.RemoveAll(parent)
		dir = filepath.Join(parent, "b") // BaseBackup / cluster.Backup create it
	}
	m := &Manifest{Kind: "single", Time: t0.UTC(), From: from}
	if cl {
		m.Kind = "cluster"
		var last error
		for _, addr := range strings.Split(from, ",") {
			addr = strings.TrimSpace(addr)
			os.RemoveAll(dir)
			info, err := cluster.Backup(addr, sec, dir, logf)
			if err == nil {
				m.From, m.LSN, m.RaftIdx, m.SegMax = addr, info.LSN, info.Index, info.SegMax
				last = nil
				break
			}
			last = fmt.Errorf("%s: %w", addr, err)
			logf("%v", last)
		}
		if last != nil {
			os.RemoveAll(dir)
			return "", nil, last
		}
	} else {
		s, err := sec.Replica(from)
		if err != nil {
			return "", nil, err
		}
		if _, err := repl.BaseBackup(from, dir, s, logf); err != nil {
			os.RemoveAll(dir)
			return "", nil, err
		}
		if b, err := os.ReadFile(repl.BackupPath(dir)); err == nil {
			var bi repl.BackupInfo
			if json.Unmarshal(b, &bi) == nil {
				m.SegMax = bi.SegMax
			}
		}
	}
	if len(key) > 0 {
		if err := encryptTree(dir, final, key); err != nil {
			os.RemoveAll(final)
			return "", nil, fmt.Errorf("encrypt: %w", err)
		}
		os.RemoveAll(dir)
		dir = final
		m.Encrypted, m.KeyID = true, seccrypt.KeyID(key)
	}
	files, n, err := hashTree(dir)
	if err != nil {
		return "", nil, err
	}
	m.Files, m.Bytes, m.Duration = files, n, time.Since(t0).Seconds()
	if m.Encrypted {
		m.MAC = m.mac(key)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, manifestName), b, 0o644); err != nil {
		return "", nil, err
	}
	return dir, m, nil
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	from := fs.String("from", "", "single node: the leader's -repl-addr; with -cluster: the Raft addresses (-raft-peers) of the nodes, comma separated (the leader is found)")
	out := fs.String("out", "", "directory that receives the backup (as a subdirectory named by time)")
	cl := fs.Bool("cluster", false, "back up a Raft cluster")
	var sf secFlags
	sf.add(fs)
	var kf keyFlags
	kf.add(fs)
	fs.Parse(args)
	if *from == "" || *out == "" {
		return errors.New("-from and -out are required")
	}
	key, err := kf.load()
	if err != nil {
		return err
	}
	dir, m, err := doBackup(*from, *out, *cl, sf.s, key)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", dir)
	enc := "not encrypted"
	if m.Encrypted {
		enc = "AES-256-GCM, key id " + m.KeyID
	}
	logf("backup %s: %d files, %d MiB, lsn %d, %.1fs, %s", m.Kind, len(m.Files), m.Bytes>>20, m.LSN, m.Duration, enc)
	return nil
}

// ---- verify ----

// VerifyReport is what verify prints.
type VerifyReport struct {
	Backup      string           `json:"backup"`
	OK          bool             `json:"ok"`
	Error       string           `json:"error,omitempty"`
	Kind        string           `json:"kind"`
	ManifestOK  bool             `json:"manifest_ok"`
	StartedOK   bool             `json:"started_ok"`
	ScrubOK     bool             `json:"scrub_ok"`
	RestoredLSN uint64           `json:"restored_lsn"`
	Tables      map[string]int64 `json:"tables"`
	StartSecs   float64          `json:"start_seconds"`
	TotalSecs   float64          `json:"total_seconds"`
	Time        time.Time        `json:"time"`
}

func readManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func verifyBackup(dir, staskd string, key []byte) (rep VerifyReport) {
	t0 := time.Now()
	rep = VerifyReport{Backup: dir, Time: t0.UTC(), Tables: map[string]int64{}}
	fail := func(format string, a ...any) VerifyReport {
		rep.Error = fmt.Sprintf(format, a...)
		rep.TotalSecs = time.Since(t0).Seconds()
		return rep
	}
	m, err := readManifest(dir)
	if err != nil {
		return fail("manifest: %v", err)
	}
	rep.Kind = m.Kind
	if err := m.checkKey(key); err != nil {
		return fail("%v", err)
	}
	now, _, err := hashTree(dir)
	if err != nil {
		return fail("hash: %v", err)
	}
	if len(now) != len(m.Files) {
		return fail("manifest lists %d files, the directory holds %d", len(m.Files), len(now))
	}
	for f, h := range m.Files {
		if now[f] != h {
			return fail("file %s differs from the manifest (corrupt or changed backup)", f)
		}
	}
	rep.ManifestOK = true
	// restore into a scratch directory
	scratch, err := os.MkdirTemp("", "staskctl-verify-")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(scratch)
	data := filepath.Join(scratch, "data")
	if m.Encrypted { // open the whole backup into the scratch directory
		plain := filepath.Join(scratch, "plain")
		if err := decryptTree(dir, plain, key); err != nil {
			return fail("decrypt: %v", err)
		}
		dir = plain
	}
	for _, sub := range []string{"wal", "ckpt"} {
		if err := copyDir(filepath.Join(dir, sub), filepath.Join(data, sub)); err != nil {
			return fail("restore %s: %v", sub, err)
		}
	}
	if m.Kind == "single" { // the remaining files (backup.json, view state) too
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if e.IsDir() || e.Name() == manifestName {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return fail("%v", err)
			}
			if err := os.WriteFile(filepath.Join(data, e.Name()), b, 0o644); err != nil {
				return fail("%v", err)
			}
		}
	}
	if m.SegMax == 0 {
		return fail("the manifest has no segment size")
	}
	port, err := freePort()
	if err != nil {
		return fail("%v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	logFile, _ := os.Create(filepath.Join(scratch, "staskd.log"))
	cmd := exec.Command(staskd, "-dir", data, "-addr", addr, "-pg-addr", "", "-segment-mb", fmt.Sprint(m.SegMax>>20), "-prezero=false", "-gc=false")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	ts := time.Now()
	if err := cmd.Start(); err != nil {
		return fail("start %s: %v", staskd, err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(20 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
	}()
	hc := &http.Client{Timeout: 10 * time.Minute}
	end := time.Now().Add(10 * time.Minute)
	for {
		r, err := hc.Get("http://" + addr + "/v1/ready")
		if err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				break
			}
		}
		select {
		case <-exited:
			b, _ := os.ReadFile(filepath.Join(scratch, "staskd.log"))
			if len(b) > 2000 {
				b = b[len(b)-2000:]
			}
			return fail("the restored database exited at start: %s", b)
		default:
		}
		if time.Now().After(end) {
			b, _ := os.ReadFile(filepath.Join(scratch, "staskd.log"))
			if len(b) > 2000 {
				b = b[len(b)-2000:]
			}
			return fail("the restored database did not become ready: %s", b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	rep.StartedOK, rep.StartSecs = true, time.Since(ts).Seconds()
	// scrub: every CRC in the restored log and checkpoints
	r, err := hc.Post("http://"+addr+"/v1/admin/scrub?wait=1&rate_mb=0", "application/json", nil)
	if err != nil {
		return fail("scrub: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		return fail("scrub of the restored database found corruption (HTTP %d)", r.StatusCode)
	}
	rep.ScrubOK = true
	r, err = hc.Get("http://" + addr + "/v1/status")
	if err != nil {
		return fail("status: %v", err)
	}
	var st struct {
		DurableLSN uint64 `json:"durable_lsn"`
		Tables     map[string]struct {
			Live int64 `json:"live_rows"`
		} `json:"tables"`
	}
	json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	rep.RestoredLSN = st.DurableLSN
	for n, t := range st.Tables {
		rep.Tables[n] = t.Live
	}
	if m.LSN != 0 && rep.RestoredLSN < m.LSN {
		return fail("restored lsn %d is behind the backup's lsn %d", rep.RestoredLSN, m.LSN)
	}
	rep.OK = true
	rep.TotalSecs = time.Since(t0).Seconds()
	return rep
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(s, d string) error {
	in, err := os.Open(s)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(d)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func findStaskd(given string) string {
	if given != "" {
		return given
	}
	if self, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(self), "staskd"); fileExists(p) {
			return p
		}
	}
	if p, err := exec.LookPath("staskd"); err == nil {
		return p
	}
	return "staskd"
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("backup", "", "backup directory (with manifest.json)")
	staskd := fs.String("staskd", "", "staskd binary (default: next to staskctl, then PATH)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	var kf keyFlags
	kf.add(fs)
	fs.Parse(args)
	if *dir == "" {
		return errors.New("-backup is required")
	}
	key, err := kf.load()
	if err != nil {
		return err
	}
	rep := verifyBackup(*dir, findStaskd(*staskd), key)
	if *asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	} else if rep.OK {
		var names []string
		for n := range rep.Tables {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Printf("backup %s verified: manifest ok, started in %.1fs, scrub ok, lsn %d, %d tables\n", rep.Backup, rep.StartSecs, rep.RestoredLSN, len(names))
		for _, n := range names {
			fmt.Printf("  %-30s %d rows\n", n, rep.Tables[n])
		}
	}
	if !rep.OK {
		return fmt.Errorf("backup %s FAILED verification: %s", rep.Backup, rep.Error)
	}
	return nil
}

// ---- restore ----

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	dir := fs.String("backup", "", "backup directory (with manifest.json)")
	to := fs.String("dir", "", "empty data directory to restore into (a cluster backup: the directory of one node; run once per node)")
	standalone := fs.Bool("standalone", false, "a cluster backup: make a single-node data directory (for point-in-time recovery from the cluster's archive) instead of a cluster node directory")
	var kf keyFlags
	kf.add(fs)
	fs.Parse(args)
	if *dir == "" || *to == "" {
		return errors.New("-backup and -dir are required")
	}
	key, err := kf.load()
	if err != nil {
		return err
	}
	m, err := readManifest(*dir)
	if err != nil {
		return err
	}
	if err := m.checkKey(key); err != nil {
		return err
	}
	now, _, err := hashTree(*dir)
	if err != nil {
		return err
	}
	for f, h := range m.Files {
		if now[f] != h {
			return fmt.Errorf("file %s differs from the manifest: not restoring a damaged backup", f)
		}
	}
	if m.Encrypted {
		// open into a private directory beside the target, copy from it, remove it
		tmp := *to + ".decrypt-tmp"
		os.RemoveAll(tmp)
		if err := decryptTree(*dir, tmp, key); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("decrypt: %w", err)
		}
		defer os.RemoveAll(tmp)
		*dir = tmp
	}
	if m.Kind == "cluster" && !*standalone {
		if err := cluster.RestoreNode(*dir, *to); err != nil {
			return err
		}
		logf("node directory %s prepared: start staskd with the same -raft-* flags as the other nodes (each node restored from this same backup); it adopts the backup at start", *to)
		return nil
	}
	if ents, _ := os.ReadDir(*to); len(ents) > 0 {
		return fmt.Errorf("%s is not empty", *to)
	}
	for _, sub := range []string{"wal", "ckpt"} {
		if err := copyDir(filepath.Join(*dir, sub), filepath.Join(*to, sub)); err != nil {
			return err
		}
	}
	ents, _ := os.ReadDir(*dir)
	for _, e := range ents {
		if !e.IsDir() && e.Name() != manifestName && !(m.Kind == "cluster" && e.Name() == "meta.json") {
			if err := copyFile(filepath.Join(*dir, e.Name()), filepath.Join(*to, e.Name())); err != nil {
				return err
			}
		}
	}
	logf("restored into %s: start staskd -dir %s -segment-mb %d (and -restore-archive for point-in-time recovery)", *to, *to, m.SegMax>>20)
	return nil
}

// ---- backup-loop ----

func cmdLoop(args []string) error {
	fs := flag.NewFlagSet("backup-loop", flag.ExitOnError)
	from := fs.String("from", "", "as backup -from")
	out := fs.String("out", "", "backup directory")
	cl := fs.Bool("cluster", false, "back up a Raft cluster")
	every := fs.Duration("interval", 6*time.Hour, "time between backups")
	keep := fs.Int("keep", 7, "verified backups to keep (older ones are deleted; a failed one is never counted)")
	staskd := fs.String("staskd", "", "staskd binary used to verify")
	noVerify := fs.Bool("no-verify", false, "skip the restore test (not recommended)")
	prom := fs.String("metrics-file", "", "write Prometheus text (node_exporter textfile collector) here after every run")
	once := fs.Bool("once", false, "one backup, then exit (status 1 if it or its verification failed)")
	var sf secFlags
	sf.add(fs)
	var kf keyFlags
	kf.add(fs)
	fs.Parse(args)
	if *from == "" || *out == "" {
		return errors.New("-from and -out are required")
	}
	key, kerr := kf.load()
	if kerr != nil {
		return kerr
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var lastOK time.Time
	var failures int
	run := func() error {
		t0 := time.Now()
		dir, m, err := doBackup(*from, *out, *cl, sf.s, key)
		var rep VerifyReport
		if err == nil && !*noVerify {
			rep = verifyBackup(dir, findStaskd(*staskd), key)
			if !rep.OK {
				err = fmt.Errorf("verification failed: %s", rep.Error)
				os.Rename(dir, dir+".FAILED")
			}
		}
		ok := err == nil
		if ok {
			lastOK = time.Now()
			failures = 0
			logf("backup %s ok (%d MiB, lsn %d); restore test %v in %.1fs", dir, m.Bytes>>20, m.LSN, !*noVerify, time.Since(t0).Seconds())
			prune(*out, *keep)
		} else {
			failures++
			logf("BACKUP FAILED: %v", err)
		}
		if *prom != "" {
			writeProm(*prom, ok, !*noVerify && rep.OK, lastOK, failures, m, rep, time.Since(t0))
		}
		return err
	}
	if *once {
		return run()
	}
	for {
		run()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*every):
		}
	}
}

func prune(out string, keep int) {
	ents, _ := os.ReadDir(out)
	var ok []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".FAILED") && fileExists(filepath.Join(out, e.Name(), manifestName)) {
			ok = append(ok, e.Name())
		}
	}
	sort.Strings(ok)
	for len(ok) > keep {
		os.RemoveAll(filepath.Join(out, ok[0]))
		ok = ok[1:]
	}
}

func writeProm(path string, ok, verified bool, lastOK time.Time, failures int, m *Manifest, rep VerifyReport, took time.Duration) {
	var b strings.Builder
	g := func(name, help string, v any) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, v)
	}
	b2 := func(x bool) int {
		if x {
			return 1
		}
		return 0
	}
	g("stask_backup_last_run_ok", "1 if the last backup run (and its restore test) succeeded.", b2(ok))
	g("stask_backup_last_success_timestamp_seconds", "Unix time of the last successful, verified backup.", lastOK.Unix())
	g("stask_backup_consecutive_failures", "Backup runs that failed in a row.", failures)
	g("stask_backup_restore_test_ok", "1 if the last restore test passed.", b2(verified))
	g("stask_backup_duration_seconds", "Duration of the last run, backup and restore test.", took.Seconds())
	if m != nil {
		g("stask_backup_bytes", "Size of the last backup.", m.Bytes)
		g("stask_backup_lsn", "LSN the last backup ends at.", m.LSN)
	}
	if rep.OK {
		g("stask_backup_restore_start_seconds", "Time the restored database took to become ready.", rep.StartSecs)
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(b.String()), 0o644) == nil {
		os.Rename(tmp, path)
	}
}

// ---- cluster membership ----

func cmdCluster(args []string) error {
	if len(args) < 1 || (args[0] != "add" && args[0] != "remove") {
		return errors.New("usage: staskctl cluster add|remove -http URL[,URL...] -id N [-raft ADDR] [-node-http ADDR]")
	}
	op := args[0]
	fs := flag.NewFlagSet("cluster "+op, flag.ExitOnError)
	urls := fs.String("http", "http://127.0.0.1:8080", "comma-separated HTTP API URLs of the nodes (the leader is found among them)")
	id := fs.Uint64("id", 0, "id of the node to add or remove")
	raftAddr := fs.String("raft", "", "add: the new node's Raft address (it must already run with -raft-join)")
	nodeHTTP := fs.String("node-http", "", "add: the new node's HTTP address (informational)")
	token := fs.String("token", os.Getenv("STASK_TOKEN"), "API token")
	var tf tlsFlags
	tf.add(fs)
	fs.Parse(args[1:])
	if *id == 0 {
		return errors.New("-id is required")
	}
	body, _ := json.Marshal(map[string]any{"op": op, "id": *id, "raft": *raftAddr, "http": *nodeHTTP})
	hc, herr := tf.client(45 * time.Second)
	if herr != nil {
		return herr
	}
	var last error
	for try := 0; try < 10; try++ {
		for _, u := range strings.Split(*urls, ",") {
			u = strings.TrimSpace(u)
			req, _ := http.NewRequest("POST", strings.TrimRight(u, "/")+"/v1/admin/cluster/members", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			if *token != "" {
				req.Header.Set("Authorization", "Bearer "+*token)
			}
			r, err := hc.Do(req)
			if err != nil {
				last = err
				continue
			}
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode == 200 {
				fmt.Printf("%s node %d: committed\n", op, *id)
				return nil
			}
			last = fmt.Errorf("%s: %s", u, strings.TrimSpace(string(b)))
			if !strings.Contains(string(b), "not the leader") && !strings.Contains(string(b), "retry") {
				return last // refused for a reason a retry does not fix
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last
}
