package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"stask/internal/audit"
	"stask/internal/seccrypt"
)

// tlsFlags: how staskctl talks to an HTTPS API (docs/13): a CA to verify the
// server, and a client certificate when the node requires mTLS.
type tlsFlags struct{ ca, cert, key string }

func (f *tlsFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&f.ca, "ca", os.Getenv("STASK_CA"), "CA certificate (PEM) that signed the node's HTTPS certificate")
	fs.StringVar(&f.cert, "cert", os.Getenv("STASK_CERT"), "client certificate (PEM) for a node that requires mTLS (-http-tls-client-ca)")
	fs.StringVar(&f.key, "key", os.Getenv("STASK_KEY"), "private key of -cert")
}

func (f tlsFlags) client(timeout time.Duration) (*http.Client, error) {
	hc := &http.Client{Timeout: timeout}
	if f.ca == "" && f.cert == "" {
		return hc, nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if f.ca != "" {
		pem, err := os.ReadFile(f.ca)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no PEM certificates", f.ca)
		}
		tc.RootCAs = pool
	}
	if f.cert != "" || f.key != "" {
		c, err := tls.LoadX509KeyPair(f.cert, f.key)
		if err != nil {
			return nil, err
		}
		tc.Certificates = []tls.Certificate{c}
	}
	hc.Transport = &http.Transport{TLSClientConfig: tc}
	return hc, nil
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "write the key to this new file (mode 0600, refuses to overwrite); default: print to stdout")
	fs.Parse(args)
	k, err := seccrypt.GenerateKey()
	if err != nil {
		return err
	}
	if *out == "" {
		fmt.Println(k)
		return nil
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(k + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	b, _ := hex.DecodeString(k)
	logf("key written to %s (key id %s). Keep a copy away from the backups: without it they cannot be read.", *out, seccrypt.KeyID(b))
	return nil
}

func cmdAudit(args []string) error {
	if len(args) < 1 || (args[0] != "verify" && args[0] != "query") {
		return errors.New("usage: staskctl audit verify|query -dir DIR [-key-file F] [query filters]")
	}
	op := args[0]
	fs := flag.NewFlagSet("audit "+op, flag.ExitOnError)
	dir := fs.String("dir", "", "the -audit-dir of the node")
	keyFile := fs.String("key-file", "", "the node's -audit-key-file (needed to verify a keyed log)")
	ev := fs.String("ev", "", "query: event prefix (connect, connect.fail, ddl, role, sql.read, sql.write, sql.denied, http, cluster)")
	user := fs.String("user", "", "query: user or role")
	obj := fs.String("obj", "", "query: object (table, role, path)")
	cmd := fs.String("cmd", "", "query: command (GRANT, CREATE TABLE, SELECT, ...)")
	failed := fs.Bool("failed", false, "query: only records that failed")
	since := fs.String("since", "", "query: RFC 3339 time")
	until := fs.String("until", "", "query: RFC 3339 time")
	last := fs.Int("last", 0, "query: only the last N matches")
	asJSON := fs.Bool("json", false, "query: print JSON lines")
	fs.Parse(args[1:])
	if *dir == "" {
		return errors.New("-dir is required")
	}
	var key []byte
	if *keyFile != "" {
		var err error
		if key, err = audit.LoadKey(*keyFile); err != nil {
			return err
		}
	}
	if op == "verify" {
		res, err := audit.Verify(*dir, key)
		if err != nil {
			return fmt.Errorf("AUDIT LOG FAILED VERIFICATION: %w", err)
		}
		note := ""
		if !res.Anchored {
			note = " (older files were pruned: the first record's predecessor is trusted, not proven)"
		}
		if !res.HeadOK {
			note += " (no audit.head: a cut-off tail cannot be ruled out)"
		}
		fmt.Printf("audit log ok: %d records in %d files, seq %d..%d, last hash %s%s\n", res.Records, res.Files, res.FirstSeq, res.LastSeq, res.LastHash[:16], note)
		return nil
	}
	f := audit.Filter{Ev: *ev, User: *user, Obj: *obj, Cmd: *cmd, FailedOnly: *failed, Limit: *last}
	var err error
	if *since != "" {
		if f.Since, err = time.Parse(time.RFC3339, *since); err != nil {
			return err
		}
	}
	if *until != "" {
		if f.Until, err = time.Parse(time.RFC3339, *until); err != nil {
			return err
		}
	}
	rs, err := audit.Query(*dir, f)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if *asJSON {
			b, _ := json.Marshal(r)
			fmt.Println(string(b))
			continue
		}
		st := "ok  "
		if !r.OK {
			st = "FAIL"
		}
		fmt.Printf("%6d %s %s %-14s user=%s role=%s addr=%s cmd=%q obj=%q %s\n", r.Seq, r.Time, st, r.Ev, r.User, r.Role, r.Addr, r.Cmd, r.Obj, r.Detail)
	}
	return nil
}
