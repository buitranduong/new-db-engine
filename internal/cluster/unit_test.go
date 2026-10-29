package cluster

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stask/internal/wal"
)

func TestBatchCodec(t *testing.T) {
	b := &batch{inc: 9, lastLSN: 77, chunks: []chunk{
		{wal.Pos{Seg: 3, Off: 100}, []byte("hello")},
		{wal.Pos{Seg: 4, Off: 0}, []byte("world!")},
	}}
	d, err := decodeBatch(encodeBatch(b))
	if err != nil || d.inc != 9 || d.lastLSN != 77 || len(d.chunks) != 2 || string(d.chunks[1].data) != "world!" || d.chunks[1].pos != (wal.Pos{Seg: 4}) {
		t.Fatalf("round trip: %+v %v", d, err)
	}
	if e := d.end(); e != (wal.Pos{Seg: 4, Off: 6}) {
		t.Fatalf("end %v", e)
	}
	enc := encodeBatch(b)
	for _, n := range []int{0, 5, 20, len(enc) - 1} {
		if _, err := decodeBatch(enc[:n]); err == nil {
			t.Fatalf("a batch cut at %d bytes decoded", n)
		}
	}
}

// TestAppliedFile: the newest valid of two slots wins, and a torn newer
// record falls back to the older one.
func TestAppliedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "applied.bin")
	af, a, err := openApplied(p)
	if err != nil || a.Index != 0 {
		t.Fatal(err, a)
	}
	af.write(applied{5, 50, wal.Pos{Seg: 1, Off: 10}}, true)
	af.write(applied{6, 60, wal.Pos{Seg: 1, Off: 20}}, true)
	af.Close()
	_, a, _ = openApplied(p)
	if a != (applied{6, 60, wal.Pos{Seg: 1, Off: 20}}) {
		t.Fatalf("got %+v", a)
	}
	// tear the slot holding index 6
	b, _ := os.ReadFile(p)
	slot := 0
	if b[0] != 6 { // index is little endian in the first byte
		slot = 1
	}
	b[slot*slotSize+9] ^= 0xff
	os.WriteFile(p, b, 0o600)
	af, a, _ = openApplied(p)
	if a.Index != 5 {
		t.Fatalf("a torn newest record must fall back to the older one, got %+v", a)
	}
	// the next write must not overwrite the surviving record
	af.write(applied{7, 70, wal.Pos{Seg: 1, Off: 30}}, true)
	af.Close()
	_, a, _ = openApplied(p)
	if a.Index != 7 {
		t.Fatalf("got %+v", a)
	}
}

// ---- test PKI ----

func writePKI(t *testing.T) (ca, cert, key string) {
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "stask test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, _ := x509.CreateCertificate(crand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	nKey, _ := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	nTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "node"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	nDER, _ := x509.CreateCertificate(crand.Reader, nTmpl, caCert, &nKey.PublicKey, caKey)
	kb, _ := x509.MarshalECPrivateKey(nKey)
	w := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600)
		return p
	}
	return w("ca.pem", "CERTIFICATE", caDER), w("node.pem", "CERTIFICATE", nDER), w("node.key", "EC PRIVATE KEY", kb)
}

// TestProcSecure: the node-to-node port uses the replication security
// settings. With mTLS and a secret the cluster forms and commits; a node
// holding a different secret is kept out; a plain TCP client is refused.
func TestProcSecure(t *testing.T) {
	ca, cert, key := writePKI(t)
	sf := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(sf, []byte("correct horse\n"), 0o600)
	c := newCluster(t, 3, "-repl-secret-file", sf, "-repl-tls-cert", cert, "-repl-tls-key", key, "-repl-tls-ca", ca, "-repl-tls-client-auth", "-repl-require-secure")
	c.createTable()
	for i := 0; i < 20; i++ {
		if c.write("sec"+string(rune('a'+i)), i, 10*time.Second) != acked {
			t.Fatal("write on a secured cluster failed")
		}
	}
	// a plain client is refused (the handshake needs TLS and the secret)
	conn, err := net.DialTimeout("tcp", c.nodes[0].raft, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("STASKREPL2"))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	// the refusal can arrive in several segments (frame header, then text): read to the end
	var got []byte
	buf := make([]byte, 200)
	for {
		n, err := conn.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil || bytes.Contains(got, []byte("requires TLS")) {
			break
		}
	}
	if !bytes.Contains(got, []byte("requires TLS")) {
		t.Fatalf("a plaintext client was not refused: %q", got)
	}
	conn.Close()
	// node 3 restarts with another secret: it cannot talk to the others
	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("wrong"), 0o600)
	// what node 3 applied while it still had the right secret: the baseline it must not exceed
	var base uint64
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if st, ok := c.status(c.nodes[2]); ok && st.Applied >= 20 {
			base = st.Applied
			break
		}
	}
	c.kill(c.nodes[2])
	for i, a := range c.nodes[2].args {
		if a == sf {
			c.nodes[2].args[i] = bad
		}
	}
	c.start(c.nodes[2])
	for i := 0; i < 20; i++ {
		if c.write("after"+string(rune('a'+i)), i, 10*time.Second) != acked {
			t.Fatal("the two good nodes must still commit")
		}
	}
	time.Sleep(2 * time.Second)
	if s, ok := c.status(c.nodes[2]); ok && base > 0 && s.Applied > base {
		t.Fatalf("the node with the wrong secret applied %d entries after the change (it had %d before)", s.Applied, base)
	}
	b, _ := os.ReadFile(c.nodes[0].log)
	if !strings.Contains(string(b), "authentication failed") && !strings.Contains(string(b), "failed to prove") {
		t.Log("(no authentication failure line in node 1's log; node 3 may have failed first)")
	}
}
