package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stask/internal/repl"
	"stask/internal/seccrypt"
)

// TestEncryptedBackup: backup with a key stores only ciphertext and no key;
// verify and restore need the right key and fail cleanly with a wrong or a
// missing key; a modified file or manifest is caught; the restored database
// has the rows.
func TestEncryptedBackup(t *testing.T) {
	if testing.Short() {
		t.Skip("process test")
	}
	bin := buildStaskd(t)
	rp, _ := freePort()
	replAddr := fmt.Sprintf("127.0.0.1:%d", rp)
	addr, stop := startNode(t, bin, t.TempDir(), "-repl-addr", replAddr)
	httpJSON(t, "POST", "http://"+addr+"/v1/tables", `{"name":"t","columns":[{"name":"id","type":"string"},{"name":"v","type":"string"}],"key":"id"}`)
	const secret = "customer-msisdn-84901234567-SECRET"
	for i := 0; i < 50; i++ {
		httpJSON(t, "POST", "http://"+addr+"/v1/write", fmt.Sprintf(`{"ops":[{"op":"put","table":"t","row":{"id":"k%d","v":"%s-%d"}}]}`, i, secret, i))
	}
	keyHex, _ := seccrypt.GenerateKey()
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte(keyHex), 0o600)
	key, err := seccrypt.LoadKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	dir, m, err := doBackup(replAddr, out, false, repl.Settings{}, key)
	if err != nil {
		t.Fatal(err)
	}
	// a plain backup of the same node, to see that the secret is in it
	pdir, _, err := doBackup(replAddr, t.TempDir(), false, repl.Settings{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	hasSecret := func(root string) (found bool, keyLeak bool) {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				b, _ := os.ReadFile(p)
				found = found || bytes.Contains(b, []byte(secret))
				keyLeak = keyLeak || bytes.Contains(b, []byte(keyHex))
			}
			return nil
		})
		return
	}
	if f, _ := hasSecret(pdir); !f {
		t.Fatal("test premise: the plain backup should contain the data")
	}
	if f, k := hasSecret(dir); f || k {
		t.Fatalf("encrypted backup leaks: data=%v key=%v", f, k)
	}
	if ents, _ := os.ReadDir(out); len(ents) != 1 {
		t.Fatalf("scratch plaintext left behind in %s: %v", out, ents)
	}
	if !m.Encrypted || m.KeyID != seccrypt.KeyID(key) || m.MAC == "" {
		t.Fatalf("manifest: %+v", m)
	}
	mb, _ := os.ReadFile(filepath.Join(dir, manifestName))
	if strings.Contains(string(mb), keyHex) {
		t.Fatal("key in the manifest")
	}

	rep := verifyBackup(dir, bin, key)
	if !rep.OK || rep.Tables["t"] != 50 {
		t.Fatalf("verify with the key: %+v", rep)
	}
	wrong := make([]byte, 32)
	for name, k := range map[string][]byte{"wrong key": wrong, "no key": nil} {
		rep := verifyBackup(dir, bin, k)
		if rep.OK || rep.StartedOK {
			t.Fatalf("%s: verify passed: %+v", name, rep)
		}
		t.Logf("%s: %s", name, rep.Error)
		if name == "wrong key" && !strings.Contains(rep.Error, "wrong encryption key") {
			t.Fatalf("unclear error: %s", rep.Error)
		}
	}
	// a modified ciphertext file: the manifest hash catches it; with the
	// manifest hash fixed up (and its MAC no longer valid), the MAC does
	bad := filepath.Join(t.TempDir(), "bad")
	if err := copyDir(dir, bad); err != nil {
		t.Fatal(err)
	}
	var target string
	filepath.Walk(filepath.Join(bad, "wal"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && target == "" {
			target = p
		}
		return nil
	})
	b, _ := os.ReadFile(target)
	b[len(b)/2] ^= 1
	os.WriteFile(target, b, 0o600)
	if rep := verifyBackup(bad, bin, key); rep.OK || rep.ManifestOK {
		t.Fatalf("modified file: %+v", rep)
	}
	var mm Manifest
	mb, _ = os.ReadFile(filepath.Join(bad, manifestName))
	json.Unmarshal(mb, &mm)
	mm.Files, mm.Bytes, _ = func() (map[string]string, int64, error) { return hashTree(bad) }()
	mb, _ = json.Marshal(mm)
	os.WriteFile(filepath.Join(bad, manifestName), mb, 0o644)
	if rep := verifyBackup(bad, bin, key); rep.OK || !strings.Contains(rep.Error, "manifest") {
		t.Fatalf("forged manifest: %+v", rep)
	}

	// restore
	dst := filepath.Join(t.TempDir(), "restored")
	if err := cmdRestore([]string{"-backup", dir, "-dir", dst, "-key-file", "/nonexistent"}); err == nil {
		t.Fatal("missing key file accepted")
	}
	if err := cmdRestore([]string{"-backup", dir, "-dir", dst}); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("restore without a key: %v", err)
	}
	wf := filepath.Join(t.TempDir(), "wrongkey")
	os.WriteFile(wf, []byte(strings.Repeat("ab", 32)), 0o600)
	if err := cmdRestore([]string{"-backup", dir, "-dir", dst, "-key-file", wf}); err == nil || !strings.Contains(err.Error(), "wrong encryption key") {
		t.Fatalf("restore with a wrong key: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(dst)); len(ents) != 0 {
		t.Fatalf("a failed restore left files: %v", ents)
	}
	t.Setenv(seccrypt.EnvKey, keyHex) // the key from the environment
	if err := cmdRestore([]string{"-backup", dir, "-dir", dst}); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(dst)); len(ents) != 1 {
		t.Fatalf("decrypt scratch left behind: %v", ents)
	}
	addr2, stop2 := startNode(t, bin, dst)
	if b := httpJSON(t, "GET", "http://"+addr2+"/v1/get?table=t&key=k49", ""); !strings.Contains(string(b), secret) {
		t.Fatalf("restored database lacks the last row: %s", b)
	}
	stop2()
}
