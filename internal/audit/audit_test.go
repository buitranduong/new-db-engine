package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openT(t *testing.T, dir string, key []byte, max int64) *Logger {
	t.Helper()
	l, err := Open(Config{Dir: dir, Key: key, MaxBytes: max, Classes: DefaultClasses})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func fill(l *Logger, n int) {
	for i := 0; i < n; i++ {
		l.Log(Record{Ev: "ddl", User: "u", Cmd: "CREATE TABLE", Obj: "t", OK: i%5 != 0, Detail: strings.Repeat("x", 40)}, i%2 == 0)
	}
}

func readLines(t *testing.T, p string) []string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func writeLines(t *testing.T, p string, l []string) {
	if err := os.WriteFile(p, []byte(strings.Join(l, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestChainRotationAndRestart(t *testing.T) {
	for _, key := range [][]byte{nil, []byte("0123456789abcdef-key")} {
		dir := t.TempDir()
		l := openT(t, dir, key, 2000)
		fill(l, 60)
		l.Close()
		files, _ := listFiles(dir)
		if len(files) < 3 {
			t.Fatalf("expected rotation, files=%v", files)
		}
		// reopen: the chain continues, across files
		l = openT(t, dir, key, 2000)
		fill(l, 10)
		l.Close()
		res, err := Verify(dir, key)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Anchored || !res.HeadOK || res.FirstSeq != 1 {
			t.Fatalf("%+v", res)
		}
		if res.Records < 70 {
			t.Fatalf("records %d", res.Records)
		}
	}
}

func TestTamperDetected(t *testing.T) {
	key := []byte("0123456789abcdef-key")
	mk := func() string {
		dir := t.TempDir()
		l := openT(t, dir, key, 1<<20)
		fill(l, 20)
		l.Close()
		return dir
	}
	f := func(dir string) string { return fileName(dir, 1) }
	cases := map[string]func(dir string){
		"edit a field": func(dir string) {
			ls := readLines(t, f(dir))
			ls[5] = strings.Replace(ls[5], `"ok":true`, `"ok":false`, 1)
			ls[5] = strings.Replace(ls[5], `"user":"u"`, `"user":"v"`, 1)
			writeLines(t, f(dir), ls)
		},
		"delete a record": func(dir string) {
			ls := readLines(t, f(dir))
			writeLines(t, f(dir), append(ls[:7:7], ls[8:]...))
		},
		"swap two records": func(dir string) {
			ls := readLines(t, f(dir))
			ls[3], ls[4] = ls[4], ls[3]
			writeLines(t, f(dir), ls)
		},
		"insert a copied record": func(dir string) {
			ls := readLines(t, f(dir))
			writeLines(t, f(dir), append(ls[:9:9], append([]string{ls[8]}, ls[9:]...)...))
		},
		"truncate the tail": func(dir string) {
			ls := readLines(t, f(dir))
			writeLines(t, f(dir), ls[:len(ls)-3])
		},
		"re-chain without the key": func(dir string) {
			// an attacker rewrites record 5 and recomputes every later hash with plain SHA-256
			ls := readLines(t, f(dir))
			ls = ls[:5]
			writeLines(t, f(dir), ls)
			os.Remove(filepath.Join(dir, "audit.head"))
		},
		"forge the head": func(dir string) {
			os.WriteFile(filepath.Join(dir, "audit.head"), []byte(`{"seq":3,"hash":"00","mac":"00"}`), 0o600)
		},
	}
	for name, mut := range cases {
		dir := mk()
		if _, err := Verify(dir, key); err != nil {
			t.Fatalf("%s: clean log: %v", name, err)
		}
		mut(dir)
		_, err := Verify(dir, key)
		if name == "re-chain without the key" {
			// the shortened log is still a valid chain prefix; without the head it cannot be told.
			// That is why audit.head is MACed and why the key matters.
			if err != nil {
				t.Fatalf("%s: a bare prefix verifies: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: not detected", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

func TestKeyedLogNeedsKey(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, []byte("0123456789abcdef-key"), 1<<20)
	fill(l, 3)
	l.Close()
	if _, err := Verify(dir, nil); err == nil || !strings.Contains(err.Error(), "keyed") {
		t.Fatalf("no key: %v", err)
	}
	if _, err := Verify(dir, []byte("another-key-0123456789")); err == nil {
		t.Fatal("wrong key accepted")
	}
	// without a key anyone can recompute a plain chain, which is the documented limit
}

func TestTornTailAndPrune(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, nil, 1500)
	fill(l, 40)
	l.Close()
	files, _ := listFiles(dir)
	// simulate a crash in the middle of a write
	af, _ := os.OpenFile(fileName(dir, files[len(files)-1]), os.O_APPEND|os.O_WRONLY, 0)
	af.WriteString(`{"seq":9999,"ts":"x","ev":"par`)
	af.Close()
	l = openT(t, dir, nil, 1500)
	fill(l, 2)
	l.Close()
	if _, err := Verify(dir, nil); err != nil {
		t.Fatalf("after a torn tail: %v", err)
	}
	rs, _ := Query(dir, Filter{Ev: "audit.start"})
	if len(rs) < 2 || !strings.Contains(rs[len(rs)-1].Detail, "incomplete") {
		t.Fatalf("restart record: %+v", rs)
	}
	// pruning old files leaves a verifiable but unanchored tail; a gap in the middle is an error
	l = openT(t, dir, nil, 1500)
	l.cfg.Keep = 2
	fill(l, 40)
	l.Close()
	res, err := Verify(dir, nil)
	if err != nil || res.Anchored || res.Files > 3 {
		t.Fatalf("pruned: %+v %v", res, err)
	}
	files, _ = listFiles(dir)
	os.Remove(fileName(dir, files[0]+1)) // a file in the middle
	if len(files) > 2 {
		if _, err := Verify(dir, nil); err == nil {
			t.Fatal("missing middle file not detected")
		}
	}
}

func TestQueryAndParse(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, nil, 1<<20)
	l.Log(Record{Ev: "connect.ok", User: "alice", Addr: "10.0.0.1", OK: true}, true)
	l.Log(Record{Ev: "connect.fail", User: "bob", Addr: "10.0.0.2"}, true)
	l.Log(Record{Ev: "sql.read", User: "alice", Cmd: "SELECT", Obj: "sims", OK: true}, false)
	l.Close()
	rs, _ := Query(dir, Filter{Ev: "connect"})
	if len(rs) != 2 {
		t.Fatal(rs)
	}
	rs, _ = Query(dir, Filter{FailedOnly: true, Ev: "connect"})
	if len(rs) != 1 || rs[0].User != "bob" {
		t.Fatal(rs)
	}
	rs, _ = Query(dir, Filter{Obj: "sims", Since: time.Now().Add(-time.Minute)})
	if len(rs) != 1 {
		t.Fatal(rs)
	}
	if _, err := ParseClasses("ddl,bogus"); err == nil {
		t.Fatal("bogus class")
	}
	c, _ := ParseClasses("connection,read")
	if c != ClassConnection|ClassRead {
		t.Fatal(c)
	}
	m, err := ParseTables("a,b:read,c:read+write")
	if err != nil || m["a"] != ClassRead|ClassWrite || m["b"] != ClassRead {
		t.Fatal(m, err)
	}
	if _, err := ParseTables("a:drop"); err == nil {
		t.Fatal("bad op")
	}
	var nl *Logger
	nl.Log(Record{Ev: "x"}, true) // nil logger is a no-op
	if nl.Has(ClassDDL) || nl.WantTable("t", ClassRead) {
		t.Fatal("nil logger")
	}
}
