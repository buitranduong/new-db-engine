// Package audit is STASK's append-only, tamper-evident audit trail (docs/22).
//
// Precedent: PostgreSQL log_connections / log_disconnections (a line per
// connection attempt and result), pgaudit (classes of statements logged per
// session, plus "object" logging for chosen relations; one line per statement
// with class, command, object; passwords never logged) and, for tamper
// evidence, the hash chain of Trillian / Certificate Transparency logs and
// auditd's "-e 2" immutable mode. STASK keeps the pgaudit vocabulary
// (classes, object list) and adds a chain: every record carries
// hash = H(prev_hash || record), H = HMAC-SHA256 when a key is configured
// (then an attacker who can write the files but lacks the key cannot
// re-chain them) and plain SHA-256 otherwise (detects edits, not a full
// rewrite). Records are JSON lines in audit-NNNNNNNN.log files; the chain
// continues across rotation (the first record of a new file is
// "audit.rotate"). A small head file (seq, hash, MAC) lets Verify notice a
// truncated tail.
package audit

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Classes of events, as pgaudit's log classes.
const (
	ClassConnection uint32 = 1 << iota // connect.ok, connect.fail, disconnect
	ClassRole                          // CREATE/ALTER/DROP ROLE, GRANT, REVOKE
	ClassDDL                           // CREATE/ALTER/DROP of everything else
	ClassRead                          // SELECT (session logging: every table)
	ClassWrite                         // INSERT/UPDATE/DELETE/MERGE/TRUNCATE/COPY (session logging)
	ClassDenied                        // permission denied (42501)
	ClassAdmin                         // HTTP/Raft admin actions and HTTP authentication failures
)

// DefaultClasses are on when -audit-dir is set and -audit-log is not.
const DefaultClasses = ClassConnection | ClassRole | ClassDDL | ClassDenied | ClassAdmin

var classNames = map[string]uint32{"connection": ClassConnection, "role": ClassRole, "ddl": ClassDDL,
	"read": ClassRead, "write": ClassWrite, "denied": ClassDenied, "admin": ClassAdmin}

// ParseClasses reads "connection,role,ddl,read,write,denied,admin", "all", "none".
func ParseClasses(s string) (uint32, error) {
	var c uint32
	for _, f := range strings.Split(s, ",") {
		f = strings.ToLower(strings.TrimSpace(f))
		switch f {
		case "":
		case "all":
			c |= ClassConnection | ClassRole | ClassDDL | ClassRead | ClassWrite | ClassDenied | ClassAdmin
		case "none":
		default:
			b, ok := classNames[f]
			if !ok {
				return 0, fmt.Errorf("unknown audit class %q", f)
			}
			c |= b
		}
	}
	return c, nil
}

// Record is one line of the log.
type Record struct {
	Seq    uint64 `json:"seq"`
	Time   string `json:"ts"`
	Ev     string `json:"ev"`
	User   string `json:"user,omitempty"`
	Role   string `json:"role,omitempty"`
	Addr   string `json:"addr,omitempty"`
	App    string `json:"app,omitempty"`
	Cmd    string `json:"cmd,omitempty"`
	Obj    string `json:"obj,omitempty"`
	Detail string `json:"detail,omitempty"`
	OK     bool   `json:"ok"`
	Alg    string `json:"alg,omitempty"` // on audit.start / audit.rotate: hmac-sha256 or sha256
	Prev   string `json:"prev"`
	Hash   string `json:"hash"`
}

// Config of a Logger.
type Config struct {
	Dir      string
	Key      []byte // optional HMAC key (never written to the log)
	MaxBytes int64  // rotate when the active file would exceed this (0 = 64 MiB)
	Keep     int    // keep this many files (0 = all)
	Classes  uint32
	Tables   map[string]uint32 // object logging: table -> ClassRead|ClassWrite bits
	Text     bool              // include statement text for read/write records
}

// Logger appends records. All methods are safe on a nil *Logger.
type Logger struct {
	cfg  Config
	mu   sync.Mutex
	f    *os.File
	size int64
	n    int // number of the active file
	seq  uint64
	prev string
	dirt bool // written, not yet synced
	stop chan struct{}
	wg   sync.WaitGroup
	err  error
	now  func() time.Time
}

var def atomic.Pointer[Logger]

// SetDefault installs the process-wide logger (nil removes it).
func SetDefault(l *Logger) { def.Store(l) }

// Get returns the process-wide logger or nil.
func Get() *Logger { return def.Load() }

const zeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

func fileName(dir string, n int) string { return filepath.Join(dir, fmt.Sprintf("audit-%08d.log", n)) }

func listFiles(dir string) ([]int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range ents {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "audit-%08d.log", &n); err == nil && e.Name() == filepath.Base(fileName(dir, n)) {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

func (c *Config) alg() string {
	if len(c.Key) > 0 {
		return "hmac-sha256"
	}
	return "sha256"
}

func mac(key []byte, parts ...[]byte) string {
	var h interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
	if len(key) > 0 {
		h = hmac.New(sha256.New, key)
	} else {
		h = sha256.New()
	}
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// body is the canonical form of a record for hashing: the JSON of the record
// with an empty hash.
func body(r Record) []byte {
	r.Hash = ""
	b, _ := json.Marshal(r)
	return b
}

// Open opens (creating) the log in cfg.Dir and continues its chain.
func Open(cfg Config) (*Logger, error) {
	if cfg.Dir == "" {
		return nil, errors.New("audit: no directory")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 64 << 20
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	l := &Logger{cfg: cfg, prev: zeroHash, now: time.Now, stop: make(chan struct{})}
	files, err := listFiles(cfg.Dir)
	if err != nil {
		return nil, err
	}
	torn := false
	if len(files) > 0 {
		l.n = files[len(files)-1]
		last, t, err := lastRecord(fileName(cfg.Dir, l.n))
		if err != nil {
			return nil, err
		}
		torn = t
		if last == nil && len(files) > 1 { // an empty newest file: continue from the one before
			last, _, err = lastRecord(fileName(cfg.Dir, files[len(files)-2]))
			if err != nil {
				return nil, err
			}
		}
		if last != nil {
			l.seq, l.prev = last.Seq, last.Hash
		}
	} else {
		l.n = 1
	}
	if err := l.openFile(l.n); err != nil {
		return nil, err
	}
	detail := ""
	if torn {
		detail = "an incomplete last line from a crash was discarded"
	}
	if l.seq == 0 {
		detail = "new log"
	}
	l.mu.Lock()
	err = l.appendLocked(Record{Ev: "audit.start", OK: true, Alg: cfg.alg(), Detail: detail}, true)
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	l.wg.Add(1)
	go l.syncer()
	return l, nil
}

// lastRecord returns the last complete record of a file; torn reports an
// incomplete trailing line, which is cut off (it was never acknowledged).
func lastRecord(path string) (rec *Record, torn bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		i := bytes.LastIndexByte(b, '\n')
		torn = true
		if err := os.Truncate(path, int64(i+1)); err != nil {
			return nil, true, err
		}
		b = b[:i+1]
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if len(lines[i]) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(lines[i], &r); err != nil {
			return nil, torn, fmt.Errorf("audit: %s: unreadable last record: %v (run staskctl audit verify)", path, err)
		}
		return &r, torn, nil
	}
	return nil, torn, nil
}

func (l *Logger) openFile(n int) error {
	f, err := os.OpenFile(fileName(l.cfg.Dir, n), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size, l.n = f, st.Size(), n
	return nil
}

func (l *Logger) appendLocked(r Record, sync bool) error {
	if l.err != nil {
		return l.err
	}
	if l.f == nil {
		return errors.New("audit: closed")
	}
	if l.size > 0 && r.Ev != "audit.rotate" && l.size >= l.cfg.MaxBytes {
		if err := l.rotateLocked(); err != nil {
			l.err = err
			return err
		}
	}
	l.seq++
	r.Seq, r.Prev = l.seq, l.prev
	r.Time = l.now().UTC().Format(time.RFC3339Nano)
	r.Hash = mac(l.cfg.Key, []byte(r.Prev), body(r))
	line, _ := json.Marshal(r)
	line = append(line, '\n')
	if _, err := l.f.Write(line); err != nil {
		l.seq--
		l.err = err
		return err
	}
	l.size += int64(len(line))
	l.prev = r.Hash
	l.dirt = true
	if sync {
		return l.syncLocked()
	}
	return nil
}

func (l *Logger) syncLocked() error {
	if !l.dirt {
		return nil
	}
	if err := l.f.Sync(); err != nil {
		l.err = err
		return err
	}
	l.dirt = false
	return l.writeHead()
}

type head struct {
	Seq  uint64 `json:"seq"`
	Hash string `json:"hash"`
	Mac  string `json:"mac"`
}

func headMac(key []byte, seq uint64, hash string) string {
	return mac(key, []byte(fmt.Sprintf("head|%d|%s", seq, hash)))
}

func (l *Logger) writeHead() error {
	h := head{l.seq, l.prev, headMac(l.cfg.Key, l.seq, l.prev)}
	b, _ := json.Marshal(h)
	p := filepath.Join(l.cfg.Dir, "audit.head")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// rotateLocked closes the active file and starts the next one, whose first
// record continues the chain.
func (l *Logger) rotateLocked() error {
	if err := l.syncLocked(); err != nil {
		return err
	}
	if err := l.f.Close(); err != nil {
		return err
	}
	if err := l.openFile(l.n + 1); err != nil {
		return err
	}
	if err := l.appendLocked(Record{Ev: "audit.rotate", OK: true, Alg: l.cfg.alg(), Detail: fmt.Sprintf("file %d", l.n)}, true); err != nil {
		return err
	}
	if k := l.cfg.Keep; k > 0 {
		files, _ := listFiles(l.cfg.Dir)
		for len(files) > k {
			os.Remove(fileName(l.cfg.Dir, files[0]))
			files = files[1:]
		}
	}
	return nil
}

func (l *Logger) syncer() {
	defer l.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.mu.Lock()
			if l.f != nil {
				l.syncLocked()
			}
			l.mu.Unlock()
		}
	}
}

// Close syncs and closes the log.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	close(l.stop)
	l.wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.syncLocked()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// Err is the first write error; the log is then closed for writing and the
// caller decides (staskd keeps serving and counts, as PostgreSQL does when
// the log destination fails).
func (l *Logger) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Log appends a record; sync forces it to disk before returning (used for
// connection, role, DDL and admin events; read/write records are synced
// within a second).
func (l *Logger) Log(r Record, sync bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.appendLocked(r, sync)
	l.mu.Unlock()
}

// Has reports whether any of the classes is enabled.
func (l *Logger) Has(c uint32) bool { return l != nil && l.cfg.Classes&c != 0 }

// TextOn: include statement text for read/write records.
func (l *Logger) TextOn() bool { return l != nil && l.cfg.Text }

// WantTable reports whether a read (class ClassRead) or write (ClassWrite)
// of table is logged: session logging (class on) or object logging (listed).
func (l *Logger) WantTable(table string, class uint32) bool {
	if l == nil {
		return false
	}
	if l.cfg.Classes&class != 0 {
		return true
	}
	return l.cfg.Tables[table]&class != 0
}

// AnyObject reports whether object logging or read/write session logging is on.
func (l *Logger) AnyObject() bool {
	return l != nil && (l.cfg.Classes&(ClassRead|ClassWrite) != 0 || len(l.cfg.Tables) > 0)
}

// ParseTables reads "t1,t2:read,t3:write,t4:read+write" (default both).
func ParseTables(s string) (map[string]uint32, error) {
	m := map[string]uint32{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		name, ops, has := strings.Cut(f, ":")
		bits := ClassRead | ClassWrite
		if has {
			bits = 0
			for _, o := range strings.Split(ops, "+") {
				switch strings.ToLower(o) {
				case "read":
					bits |= ClassRead
				case "write":
					bits |= ClassWrite
				default:
					return nil, fmt.Errorf("audit table %q: unknown operation %q (read, write, read+write)", name, o)
				}
			}
		}
		m[name] |= bits
	}
	return m, nil
}

// ---- verification and query ----

// VerifyResult describes a verified log.
type VerifyResult struct {
	Files, Records uint64
	FirstSeq       uint64
	LastSeq        uint64
	LastHash       string
	// Anchored is false when the oldest file's first record does not start
	// the chain (older files were pruned): its prev hash is trusted, not proven.
	Anchored bool
	HeadOK   bool // audit.head was present, authentic and not ahead of the log
}

// Verify walks every file in order and checks sequence numbers, the chain,
// every hash, the file order and the head file. The first problem is returned
// as an error naming file and seq.
func Verify(dir string, key []byte) (VerifyResult, error) {
	var res VerifyResult
	files, err := listFiles(dir)
	if err != nil {
		return res, err
	}
	if len(files) == 0 {
		return res, errors.New("audit: no log files")
	}
	var prev string
	var seq uint64
	for i, n := range files {
		if i > 0 && n != files[i-1]+1 {
			return res, fmt.Errorf("audit: file %d is missing (found %d after %d)", files[i-1]+1, n, files[i-1])
		}
		f, err := os.Open(fileName(dir, n))
		if err != nil {
			return res, err
		}
		res.Files++
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		first := true
		for sc.Scan() {
			var r Record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				f.Close()
				return res, fmt.Errorf("audit: file %d: unreadable record after seq %d: %v", n, seq, err)
			}
			if res.Records == 0 {
				res.FirstSeq = r.Seq
				res.Anchored = r.Seq == 1 && r.Prev == zeroHash
				prev, seq = r.Prev, r.Seq-1
			}
			if r.Seq != seq+1 {
				f.Close()
				return res, fmt.Errorf("audit: file %d: sequence jumps from %d to %d (records deleted or inserted)", n, seq, r.Seq)
			}
			if r.Prev != prev {
				f.Close()
				return res, fmt.Errorf("audit: file %d seq %d: broken chain (prev hash does not match the record before)", n, r.Seq)
			}
			if want := mac(key, []byte(r.Prev), body(r)); r.Hash != want {
				f.Close()
				if r.Alg == "hmac-sha256" && len(key) == 0 {
					return res, fmt.Errorf("audit: file %d seq %d: the log is keyed (hmac-sha256): give the key file", n, r.Seq)
				}
				return res, fmt.Errorf("audit: file %d seq %d: hash mismatch (record changed, or wrong key)", n, r.Seq)
			}
			if first && i > 0 && r.Ev != "audit.rotate" {
				f.Close()
				return res, fmt.Errorf("audit: file %d does not start with audit.rotate", n)
			}
			first = false
			prev, seq = r.Hash, r.Seq
			res.Records++
		}
		if err := sc.Err(); err != nil {
			f.Close()
			return res, fmt.Errorf("audit: file %d: %v", n, err)
		}
		f.Close()
	}
	res.LastSeq, res.LastHash = seq, prev
	if b, err := os.ReadFile(filepath.Join(dir, "audit.head")); err == nil {
		var h head
		if json.Unmarshal(b, &h) != nil || h.Mac != headMac(key, h.Seq, h.Hash) {
			return res, errors.New("audit: audit.head is not authentic (edited, or wrong key)")
		}
		if h.Seq > seq {
			return res, fmt.Errorf("audit: the log was truncated: head says seq %d, the files end at %d", h.Seq, seq)
		}
		res.HeadOK = true
	}
	return res, nil
}

// Filter selects records for Query; zero fields match everything.
type Filter struct {
	Ev, User, Obj, Cmd string
	FailedOnly         bool
	Since, Until       time.Time
	FromSeq            uint64
	Limit              int // the last Limit matches (0 = all)
}

// Query returns matching records in order. It does not verify; run Verify.
func Query(dir string, f Filter) ([]Record, error) {
	files, err := listFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, n := range files {
		fh, err := os.Open(fileName(dir, n))
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			if f.Match(r) {
				out = append(out, r)
			}
		}
		fh.Close()
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, nil
}

// Match applies the filter. Ev matches as a prefix ("connect" matches
// connect.ok and connect.fail).
func (f Filter) Match(r Record) bool {
	if f.Ev != "" && !strings.HasPrefix(r.Ev, f.Ev) {
		return false
	}
	if f.User != "" && r.User != f.User && r.Role != f.User {
		return false
	}
	if f.Obj != "" && r.Obj != f.Obj {
		return false
	}
	if f.Cmd != "" && !strings.EqualFold(r.Cmd, f.Cmd) {
		return false
	}
	if f.FailedOnly && r.OK {
		return false
	}
	if r.Seq < f.FromSeq {
		return false
	}
	if !f.Since.IsZero() || !f.Until.IsZero() {
		t, err := time.Parse(time.RFC3339Nano, r.Time)
		if err != nil || (!f.Since.IsZero() && t.Before(f.Since)) || (!f.Until.IsZero() && !t.Before(f.Until)) {
			return false
		}
	}
	return true
}

// LoadKey reads a key file: raw bytes (at least 16), or hex text.
func LoadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t := bytes.TrimSpace(b)
	if h, err := hex.DecodeString(string(t)); err == nil && len(h) >= 16 {
		return h, nil
	}
	if len(b) < 16 {
		return nil, fmt.Errorf("audit key %s: too short (need at least 16 bytes)", path)
	}
	return b, nil
}
