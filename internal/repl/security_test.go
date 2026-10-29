package repl

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stask/internal/core"
)

// pki writes PEM files to a directory: CA certificates and keys, and leaf
// certificates signed by them.
type pki struct {
	t   *testing.T
	dir string
	n   int64
}

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	file string
}

func (p *pki) write(name, typ string, der []byte) string {
	path := filepath.Join(p.dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		p.t.Fatal(err)
	}
	return path
}

func (p *pki) template(cn string) *x509.Certificate {
	p.n++
	return &x509.Certificate{SerialNumber: big.NewInt(p.n), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
}

func (p *pki) ca(name string) *ca {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	tpl := p.template(name)
	tpl.IsCA, tpl.BasicConstraintsValid, tpl.KeyUsage = true, true, x509.KeyUsageCertSign
	der, err := x509.CreateCertificate(crand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		p.t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &ca{cert: cert, key: key, file: p.write(name+".crt", "CERTIFICATE", der)}
}

// leaf issues a certificate for 127.0.0.1 and leader.test usable by both
// servers and clients (as etcd peer certificates); it returns cert and key
// files.
func (p *pki) leaf(c *ca, name string) (string, string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	tpl := p.template(name)
	tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	tpl.KeyUsage = x509.KeyUsageDigitalSignature
	tpl.IPAddresses, tpl.DNSNames = []net.IP{net.ParseIP("127.0.0.1")}, []string{"leader.test"}
	der, err := x509.CreateCertificate(crand.Reader, tpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		p.t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return p.write(name+".crt", "CERTIFICATE", der), p.write(name+".key", "EC PRIVATE KEY", kb)
}

func newPKI(t *testing.T) *pki { return &pki{t: t, dir: t.TempDir()} }

func smallLeader(t *testing.T) *core.Engine {
	t.Helper()
	e, err := core.Open(core.Options{Dir: filepath.Join(t.TempDir(), "leader"), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := e.Write([]core.WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), 0)}}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func mustLeader(t *testing.T, st Settings) *Security {
	t.Helper()
	sec, err := st.Leader()
	if err != nil {
		t.Fatal(err)
	}
	return sec
}

func mustReplica(t *testing.T, st Settings, addr string) *Security {
	t.Helper()
	sec, err := st.Replica(addr)
	if err != nil {
		t.Fatal(err)
	}
	return sec
}

// basebackup serves leader with lsec and fetches a base backup with the
// replica settings rst; it returns Prepare's error.
func basebackup(t *testing.T, leader *core.Engine, lsec *Security, rst Settings) error {
	t.Helper()
	srv, err := Serve(leader, "127.0.0.1:0", lsec, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rsec, err := rst.Replica(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Prepare(srv.Addr(), filepath.Join(t.TempDir(), "replica"), rsec, t.Logf)
	return err
}

func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want an error containing %q, got %v", sub, err)
	}
	t.Logf("refused as expected: %v", err)
}

func TestSecret(t *testing.T) {
	leader := smallLeader(t)
	lsec := mustLeader(t, Settings{Secret: "s3cret"})
	if err := basebackup(t, leader, lsec, Settings{Secret: "s3cret"}); err != nil {
		t.Fatalf("right secret: %v", err)
	}
	wantErr(t, basebackup(t, leader, lsec, Settings{Secret: "wrong"}), "authentication failed")
	wantErr(t, basebackup(t, leader, lsec, Settings{}), "authentication required")
	// a replica with a secret does not follow a leader that cannot prove it
	wantErr(t, basebackup(t, leader, nil, Settings{Secret: "s3cret"}), "does not authenticate")

	// the secret from a file, trailing newline ignored
	f := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(f, []byte("s3cret\n"), 0o600)
	if err := basebackup(t, leader, lsec, Settings{SecretFile: f}); err != nil {
		t.Fatalf("secret file: %v", err)
	}
}

func TestTLS(t *testing.T) {
	leader := smallLeader(t)
	p := newPKI(t)
	ca1, ca2 := p.ca("ca1"), p.ca("ca2")
	cert, key := p.leaf(ca1, "leader")
	lsec := mustLeader(t, Settings{CertFile: cert, KeyFile: key})
	if err := basebackup(t, leader, lsec, Settings{CAFile: ca1.file}); err != nil {
		t.Fatalf("TLS verified by IP: %v", err)
	}
	if err := basebackup(t, leader, lsec, Settings{CAFile: ca1.file, ServerName: "leader.test"}); err != nil {
		t.Fatalf("TLS verified by name: %v", err)
	}
	wantErr(t, basebackup(t, leader, lsec, Settings{CAFile: ca2.file}), "certificate signed by unknown authority")
	// verify-full: the right CA but another name
	wantErr(t, basebackup(t, leader, lsec, Settings{CAFile: ca1.file, ServerName: "other.test"}), "certificate is valid for")
	// a leader without a certificate cannot be verified
	wantErr(t, basebackup(t, leader, nil, Settings{CAFile: ca1.file}), "TLS to leader")
}

func TestRequire(t *testing.T) {
	leader := smallLeader(t)
	p := newPKI(t)
	ca1, ca2 := p.ca("ca1"), p.ca("ca2")
	cert, key := p.leaf(ca1, "leader")
	rcert, rkey := p.leaf(ca1, "replica")
	rogue, roguekey := p.leaf(ca2, "rogue")

	// requiring security needs both TLS and authentication
	if _, err := (Settings{Secret: "x", Require: true}).Leader(); err == nil {
		t.Fatal("require without TLS accepted")
	}
	if _, err := (Settings{CertFile: cert, KeyFile: key, Require: true}).Leader(); err == nil {
		t.Fatal("require without authentication accepted")
	}
	if _, err := (Settings{Require: true}).Replica("127.0.0.1:1"); err == nil {
		t.Fatal("replica require without a CA accepted")
	}

	lsec := mustLeader(t, Settings{Secret: "s3cret", CertFile: cert, KeyFile: key, Require: true})
	wantErr(t, basebackup(t, leader, lsec, Settings{Secret: "s3cret"}), "requires TLS")
	if err := basebackup(t, leader, lsec, Settings{Secret: "s3cret", CAFile: ca1.file}); err != nil {
		t.Fatalf("TLS and secret: %v", err)
	}
	wantErr(t, basebackup(t, leader, lsec, Settings{Secret: "wrong", CAFile: ca1.file}), "authentication failed")

	// mTLS instead of a secret
	msec := mustLeader(t, Settings{CertFile: cert, KeyFile: key, CAFile: ca1.file, ClientAuth: true, Require: true})
	if err := basebackup(t, leader, msec, Settings{CAFile: ca1.file, CertFile: rcert, KeyFile: rkey}); err != nil {
		t.Fatalf("client certificate: %v", err)
	}
	if err := basebackup(t, leader, msec, Settings{CAFile: ca1.file}); err == nil {
		t.Fatal("no client certificate accepted")
	} else {
		t.Logf("refused as expected: %v", err)
	}
	if err := basebackup(t, leader, msec, Settings{CAFile: ca1.file, CertFile: rogue, KeyFile: roguekey}); err == nil {
		t.Fatal("client certificate of another CA accepted")
	} else {
		t.Logf("refused as expected: %v", err)
	}
	// client certificates without Require still refuse plaintext replicas
	csec := mustLeader(t, Settings{CertFile: cert, KeyFile: key, CAFile: ca1.file, ClientAuth: true})
	wantErr(t, basebackup(t, leader, csec, Settings{}), "requires TLS")
}

func TestCheckListen(t *testing.T) {
	secret := &Security{Secret: []byte("x")}
	for _, c := range []struct {
		addr     string
		sec      *Security
		insecure bool
		ok       bool
	}{
		{"127.0.0.1:7000", nil, false, true},
		{"[::1]:7000", nil, false, true},
		{"localhost:7000", nil, false, true},
		{":7000", nil, false, false},
		{"0.0.0.0:7000", nil, false, false},
		{"10.0.0.5:7000", nil, false, false},
		{"10.0.0.5:7000", &Security{TLS: nil}, false, false},
		{"10.0.0.5:7000", nil, true, true},
		{"10.0.0.5:7000", secret, false, true},
	} {
		if err := CheckListen(c.addr, c.sec, c.insecure); (err == nil) != c.ok {
			t.Errorf("%s insecure=%v: %v", c.addr, c.insecure, err)
		}
	}
}

// TestSecureFollow runs a replica over mTLS with a secret: base backup,
// then streaming of new writes.
func TestSecureFollow(t *testing.T) {
	leader := smallLeader(t)
	p := newPKI(t)
	ca1 := p.ca("ca1")
	cert, key := p.leaf(ca1, "leader")
	rcert, rkey := p.leaf(ca1, "replica")
	lsec := mustLeader(t, Settings{Secret: "s3cret", CertFile: cert, KeyFile: key, CAFile: ca1.file, ClientAuth: true, Require: true})
	srv, err := Serve(leader, "127.0.0.1:0", lsec, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rsec := mustReplica(t, Settings{Secret: "s3cret", CAFile: ca1.file, CertFile: rcert, KeyFile: rkey, Require: true}, srv.Addr())
	rdir := filepath.Join(t.TempDir(), "replica")
	segMax, err := Prepare(srv.Addr(), rdir, rsec, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Replica: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); Follow(replica, srv.Addr(), rsec, stop, t.Logf) }()
	defer func() { close(stop); wg.Wait() }()
	for i := 100; i < 300; i++ {
		if _, err := leader.Write([]core.WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	same(t, leader, replica)
}
