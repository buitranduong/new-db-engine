package seccrypt

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func key(t *testing.T) []byte {
	k := make([]byte, KeyLen)
	rand.Read(k)
	return k
}

func TestRoundTripSizes(t *testing.T) {
	k := key(t)
	for _, chunk := range []int{16, 1000, DefaultChunk} {
		for _, n := range []int{0, 1, 15, 16, 17, chunk - 1, chunk, chunk + 1, 3 * chunk, 3*chunk + 7} {
			data := make([]byte, n)
			rand.Read(data)
			var b bytes.Buffer
			w, err := newWriter(&b, k, chunk)
			if err != nil {
				t.Fatal(err)
			}
			// uneven writes
			for off := 0; off < len(data); {
				m := min(len(data)-off, 1+off%37)
				w.Write(data[off : off+m])
				off += m
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := NewReader(iotest.OneByteReader(bytes.NewReader(b.Bytes())), k)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("chunk %d n %d: err %v equal %v", chunk, n, err, bytes.Equal(got, data))
			}
		}
	}
}

func TestWrongKeyAndTamper(t *testing.T) {
	k := key(t)
	data := bytes.Repeat([]byte("customer-sim-data "), 20000)
	var b bytes.Buffer
	w, _ := newWriter(&b, k, 4096)
	w.Write(data)
	w.Close()
	ct := b.Bytes()
	if bytes.Contains(ct, []byte("customer-sim-data")) {
		t.Fatal("plaintext visible in the ciphertext")
	}
	if _, err := Open(key(t), ct); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong key: %v", err)
	}
	if got, err := Open(k, ct); err != nil || !bytes.Equal(got, data) {
		t.Fatal("good key", err)
	}
	if _, err := Open(k, []byte("plain text, not encrypted at all, long enough to hold a header...")); !errors.Is(err, ErrNotEnc) {
		t.Fatalf("plaintext: %v", err)
	}
	cs := 4096 + tagLen
	flip := func(off int) []byte { c := append([]byte(nil), ct...); c[off] ^= 1; return c }
	cases := map[string][]byte{
		"flip in the first chunk":  flip(headerLen + 5),
		"flip in the last chunk":   flip(len(ct) - 3),
		"flip in the tag":          flip(headerLen + cs - 1),
		"flip in the header chunk": flip(9),
		"truncate in a chunk":      ct[:len(ct)-10],
		"truncate at a boundary":   ct[:headerLen+3*cs],
		"drop the last chunk":      ct[:headerLen+(len(ct)-headerLen)/cs*cs],
		"append garbage":           append(append([]byte(nil), ct...), 1, 2, 3),
		"append a copied chunk":    append(append([]byte(nil), ct...), ct[headerLen:headerLen+cs]...),
		"swap two chunks": func() []byte {
			c := append([]byte(nil), ct...)
			a, b := c[headerLen:headerLen+cs], c[headerLen+cs:headerLen+2*cs]
			t := append([]byte(nil), a...)
			copy(a, b)
			copy(b, t)
			return c
		}(),
		"header only": ct[:headerLen],
	}
	for name, c := range cases {
		got, err := Open(k, c)
		if err == nil {
			t.Errorf("%s: accepted (%d bytes)", name, len(got))
			continue
		}
		if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrWrongKey) {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}
	// two encryptions of the same data differ (random per-file salt)
	c2, _ := Seal(k, []byte("x"))
	c3, _ := Seal(k, []byte("x"))
	if bytes.Equal(c2, c3) {
		t.Fatal("deterministic ciphertext")
	}
}

func TestKeyParsingAndFiles(t *testing.T) {
	hexk, _ := GenerateKey()
	if k, err := ParseKey([]byte(hexk + "\n")); err != nil || len(k) != 32 {
		t.Fatal(err)
	}
	dir := t.TempDir()
	kf := filepath.Join(dir, "key")
	os.WriteFile(kf, []byte(hexk), 0o600)
	k, err := LoadKey(kf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKey([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	t.Setenv(EnvKey, hexk)
	if k2, err := LoadKey(""); err != nil || !bytes.Equal(k, k2) {
		t.Fatal("env key", err)
	}
	t.Setenv(EnvKey, "")
	if k2, err := LoadKey(""); err != nil || k2 != nil {
		t.Fatal("no key must be (nil,nil)")
	}
	src := filepath.Join(dir, "a")
	os.WriteFile(src, []byte(strings.Repeat("abc", 100000)), 0o644)
	if err := EncryptFile(src, src+".enc", k); err != nil {
		t.Fatal(err)
	}
	if err := DecryptFile(src+".enc", src+".dec", k); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(src + ".dec")
	if !bytes.Equal(a, b) {
		t.Fatal("round trip")
	}
	other := make([]byte, 32)
	if err := DecryptFile(src+".enc", src+".bad", other); !errors.Is(err, ErrWrongKey) {
		t.Fatal(err)
	}
	if _, err := os.Stat(src + ".bad"); err == nil {
		t.Fatal("output left behind on a wrong key")
	}
	if KeyID(k) == KeyID(other) || strings.Contains(hexk, KeyID(k)) {
		t.Fatal("key id")
	}
}
