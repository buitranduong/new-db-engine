package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stask/internal/repl"
)

func buildStaskd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "staskd")
	if out, err := exec.Command("go", "build", "-o", bin, "../staskd").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func startNode(t *testing.T, bin, dir string, extra ...string) (httpAddr string, stop func()) {
	t.Helper()
	p1, _ := freePort()
	httpAddr = fmt.Sprintf("127.0.0.1:%d", p1)
	args := append([]string{"-dir", dir, "-addr", httpAddr, "-pg-addr", "", "-segment-mb", "8", "-prezero=false", "-gc=false"}, extra...)
	cmd := exec.Command(bin, args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop = func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		if t.Failed() {
			t.Logf("staskd log:\n%s", buf.String())
		}
	})
	for i := 0; i < 200; i++ {
		if r, err := http.Get("http://" + httpAddr + "/v1/ready"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("staskd did not become ready")
	return
}

func httpJSON(t *testing.T, method, url, body string) []byte {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(r.Body)
	if r.StatusCode != 200 {
		t.Fatalf("%s %s: %d %s", method, url, r.StatusCode, b.String())
	}
	return b.Bytes()
}

// TestBackupVerifyRestore: backup of a running leader, restore test passes,
// a damaged backup is caught (by the manifest, and, when the manifest is
// forged to match, by the scrub), and restore gives a database with the rows.
func TestBackupVerifyRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("process test")
	}
	bin := buildStaskd(t)
	rp, _ := freePort()
	replAddr := fmt.Sprintf("127.0.0.1:%d", rp)
	src := t.TempDir()
	addr, stop := startNode(t, bin, src, "-repl-addr", replAddr)
	httpJSON(t, "POST", "http://"+addr+"/v1/tables", `{"name":"t","columns":[{"name":"id","type":"string"},{"name":"v","type":"int"}],"key":"id"}`)
	for i := 0; i < 50; i++ {
		httpJSON(t, "POST", "http://"+addr+"/v1/write", fmt.Sprintf(`{"ops":[{"op":"put","table":"t","row":{"id":"k%d","v":%d}}]}`, i, i))
	}
	out := t.TempDir()
	dir, m, err := doBackup(replAddr, out, false, repl.Settings{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if m.SegMax != 8<<20 || len(m.Files) == 0 {
		t.Fatalf("manifest: %+v", m)
	}
	rep := verifyBackup(dir, bin, nil)
	if !rep.OK || rep.Tables["t"] != 50 || !rep.ScrubOK || !rep.ManifestOK {
		t.Fatalf("verify of a good backup: %+v", rep)
	}
	t.Logf("good backup: started in %.2fs, tables %v, lsn %d", rep.StartSecs, rep.Tables, rep.RestoredLSN)

	// a file changed after the backup is caught by the manifest; damage that
	// existed when the backup was taken (manifest forged to match) must still
	// fail the restore test, for the checkpoint and for the log
	for _, what := range []string{"ckpt", "wal"} {
		bad := filepath.Join(t.TempDir(), "bad-"+what)
		if err := copyDir(dir, bad); err != nil {
			t.Fatal(err)
		}
		ents, _ := os.ReadDir(filepath.Join(bad, what))
		var target string
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".ckpt") || strings.HasSuffix(e.Name(), ".wal") {
				target = filepath.Join(bad, what, e.Name())
			}
		}
		b, _ := os.ReadFile(target)
		at := len(b) / 2
		if what == "wal" {
			at = 100
		}
		b[at] ^= 0xff
		os.WriteFile(target, b, 0o644)
		if rep := verifyBackup(bad, bin, nil); rep.OK || rep.ManifestOK {
			t.Fatalf("%s: a changed file passed the manifest: %+v", what, rep)
		}
		files, n, _ := hashTree(bad)
		m.Files, m.Bytes = files, n
		mb, _ := json.Marshal(m)
		os.WriteFile(filepath.Join(bad, manifestName), mb, 0o644)
		rep = verifyBackup(bad, bin, nil)
		if rep.OK {
			t.Fatalf("%s: a damaged backup with a matching manifest passed the restore test: %+v", what, rep)
		}
		t.Logf("damaged %s with a matching manifest: %s", what, rep.Error)
	}

	// 3. restore into a fresh directory and start on it
	dst := filepath.Join(t.TempDir(), "restored")
	if err := cmdRestore([]string{"-backup", dir, "-dir", dst}); err != nil {
		t.Fatal(err)
	}
	if err := cmdRestore([]string{"-backup", dir, "-dir", dst}); err == nil {
		t.Fatal("restore into a non-empty directory was accepted")
	}
	addr2, stop2 := startNode(t, bin, dst)
	b := httpJSON(t, "GET", "http://"+addr2+"/v1/get?table=t&key=k49", "")
	if !strings.Contains(string(b), "49") {
		t.Fatalf("restored database lacks the last row: %s", b)
	}
	stop2()
}

// TestBackupLoopMetrics: one run of the scheduled backup writes the
// Prometheus textfile and prunes old backups.
func TestBackupLoopMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("process test")
	}
	bin := buildStaskd(t)
	rp, _ := freePort()
	replAddr := fmt.Sprintf("127.0.0.1:%d", rp)
	addr, _ := startNode(t, bin, t.TempDir(), "-repl-addr", replAddr)
	httpJSON(t, "POST", "http://"+addr+"/v1/tables", `{"name":"t","columns":[{"name":"id","type":"string"}],"key":"id"}`)
	out, prom := t.TempDir(), filepath.Join(t.TempDir(), "stask_backup.prom")
	for i := 0; i < 3; i++ {
		if err := cmdLoop([]string{"-from", replAddr, "-out", out, "-once", "-keep", "2", "-staskd", bin, "-metrics-file", prom}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // backup names are per second
	}
	ents, _ := os.ReadDir(out)
	if len(ents) != 2 {
		t.Fatalf("-keep 2 left %d backups", len(ents))
	}
	b, _ := os.ReadFile(prom)
	for _, want := range []string{"stask_backup_last_run_ok 1", "stask_backup_restore_test_ok 1", "stask_backup_consecutive_failures 0"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics file lacks %q:\n%s", want, b)
		}
	}
	// an unreachable source: failure is reported, exit status is an error
	if err := cmdLoop([]string{"-from", "127.0.0.1:1", "-out", out, "-once", "-staskd", bin, "-metrics-file", prom}); err == nil {
		t.Fatal("a failed backup returned success")
	}
	b, _ = os.ReadFile(prom)
	if !strings.Contains(string(b), "stask_backup_last_run_ok 0") || !strings.Contains(string(b), "stask_backup_consecutive_failures 1") {
		t.Errorf("failure not in the metrics file:\n%s", b)
	}
}
