// Package seccrypt is chunked authenticated encryption (AES-256-GCM) for
// backups and archived log segments (docs/23).
//
// Precedent: pgBackRest (repo encryption: AES-256-CBC with a passphrase, key
// never stored in the repository), WAL-G (libsodium secretbox / OpenPGP, key
// from a file or env, per-file), age (STREAM construction: 64 KiB chunks,
// each sealed with ChaCha20-Poly1305 under a per-file key derived from the
// master key and a random per-file nonce, the last chunk flagged so that
// truncation is detected) and Google Tink's streaming AEAD (same
// construction with AES-GCM). STASK follows the age/Tink STREAM design with
// AES-256-GCM, because the Go standard library has it with hardware support.
//
// File format (all integers big endian):
//
//	"STSKENC1"            8   magic + version
//	chunk size            4   plaintext bytes per chunk (default 65536)
//	salt                  16  random per file
//	key check             16  HMAC-SHA256(fileKey, "stask-keycheck" || header)[:16]
//	chunk i               n+16 AES-256-GCM(fileKey, nonce_i, plaintext, aad=header)
//
// fileKey = HKDF-SHA256(master, salt, "stask-enc-v1"), unique per file, so a
// (key, nonce) pair is never reused even though the nonce is only the chunk
// counter: nonce = 3 zero bytes | final flag (1 byte, 1 on the last chunk) |
// uint64 chunk index. Reordering,
// duplicating, truncating or extending chunks fails authentication; a wrong
// master key fails at the key check with ErrWrongKey, before any data.
// The master key is never written anywhere by this package.
package seccrypt

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	magic        = "STSKENC1"
	DefaultChunk = 64 << 10
	headerLen    = 8 + 4 + 16 + 16
	tagLen       = 16
	KeyLen       = 32
	// EnvKey is the environment variable read when no key file is given.
	EnvKey = "STASK_BACKUP_KEY"
)

var (
	ErrWrongKey  = errors.New("wrong encryption key (the key check of the file does not match)")
	ErrCorrupt   = errors.New("encrypted data is corrupt or was modified (authentication failed)")
	ErrTruncated = errors.New("encrypted data is truncated")
	ErrNotEnc    = errors.New("not an encrypted STASK file")
)

// ParseKey accepts 64 hex digits, standard or URL base64 of 32 bytes, or
// exactly 32 raw bytes.
func ParseKey(b []byte) ([]byte, error) {
	t := strings.TrimSpace(string(b))
	if h, err := hex.DecodeString(t); err == nil && len(h) == KeyLen {
		return h, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if d, err := enc.DecodeString(t); err == nil && len(d) == KeyLen {
			return d, nil
		}
	}
	if len(b) == KeyLen {
		return append([]byte(nil), b...), nil
	}
	return nil, fmt.Errorf("encryption key must be 32 bytes: 64 hex digits, base64, or a 32-byte raw file (got %d bytes)", len(b))
}

// LoadKey reads the key from file, or, when file is empty, from $STASK_BACKUP_KEY.
// Neither set: (nil, nil), meaning "no encryption".
func LoadKey(file string) ([]byte, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("encryption key file: %w", err)
		}
		return ParseKey(b)
	}
	if v := os.Getenv(EnvKey); v != "" {
		return ParseKey([]byte(v))
	}
	return nil, nil
}

// GenerateKey returns a new random key as hex text.
func GenerateKey() (string, error) {
	k := make([]byte, KeyLen)
	if _, err := rand.Read(k); err != nil {
		return "", err
	}
	return hex.EncodeToString(k), nil
}

// KeyID is a short public identifier of a key (HMAC of a constant), stored in
// manifests to tell which key a backup needs. It reveals nothing usable.
func KeyID(key []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("stask-key-id"))
	return hex.EncodeToString(m.Sum(nil)[:6])
}

func fileKey(master, salt []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, master, salt, "stask-enc-v1", KeyLen)
}

func keyCheck(fk, hdr []byte) []byte {
	m := hmac.New(sha256.New, fk)
	m.Write([]byte("stask-keycheck"))
	m.Write(hdr)
	return m.Sum(nil)[:16]
}

func nonce(i uint64, last bool) []byte {
	n := make([]byte, 12)
	if last {
		n[3] = 1
	}
	binary.BigEndian.PutUint64(n[4:], i)
	return n
}

type writer struct {
	w     io.Writer
	aead  cipher.AEAD
	hdr   []byte
	buf   []byte
	n     int
	idx   uint64
	err   error
	close bool
}

// NewWriter starts an encrypted stream on w. Close writes the last chunk and
// must be called; it does not close w.
func NewWriter(w io.Writer, key []byte) (io.WriteCloser, error) {
	return newWriter(w, key, DefaultChunk)
}

func newWriter(w io.Writer, key []byte, chunk int) (io.WriteCloser, error) {
	if len(key) != KeyLen {
		return nil, errors.New("seccrypt: key must be 32 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	fk, err := fileKey(key, salt)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 0, headerLen)
	hdr = append(hdr, magic...)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(chunk))
	hdr = append(hdr, salt...)
	hdr = append(hdr, keyCheck(fk, hdr)...)
	blk, err := aes.NewCipher(fk)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return &writer{w: w, aead: aead, hdr: hdr, buf: make([]byte, chunk)}, nil
}

func (e *writer) seal(last bool) error {
	out := e.aead.Seal(nil, nonce(e.idx, last), e.buf[:e.n], e.hdr)
	e.idx++
	e.n = 0
	_, err := e.w.Write(out)
	return err
}

func (e *writer) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	total := 0
	for len(p) > 0 {
		// a full buffer is sealed only when more data follows, so that the
		// last chunk (possibly full, possibly empty) is the one flagged final
		if e.n == len(e.buf) {
			if e.err = e.seal(false); e.err != nil {
				return total, e.err
			}
		}
		c := copy(e.buf[e.n:], p)
		e.n += c
		p = p[c:]
		total += c
	}
	return total, nil
}

func (e *writer) Close() error {
	if e.close {
		return e.err
	}
	e.close = true
	if e.err == nil {
		e.err = e.seal(true)
	}
	return e.err
}

type reader struct {
	r     io.Reader
	aead  cipher.AEAD
	hdr   []byte
	in    []byte // sealed chunk buffer
	out   []byte // decrypted, unread
	next  []byte // read-ahead of the next chunk's first bytes
	idx   uint64
	final bool
	err   error
}

// NewReader checks the header and the key and returns the plaintext stream.
// Errors: ErrNotEnc, ErrWrongKey while reading the header; ErrCorrupt or
// ErrTruncated while reading data.
func NewReader(r io.Reader, key []byte) (io.Reader, error) {
	if len(key) != KeyLen {
		return nil, errors.New("seccrypt: key must be 32 bytes")
	}
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, ErrNotEnc
		}
		return nil, err
	}
	if string(hdr[:8]) != magic {
		return nil, ErrNotEnc
	}
	chunk := binary.BigEndian.Uint32(hdr[8:12])
	if chunk == 0 || chunk > 64<<20 {
		return nil, ErrNotEnc
	}
	fk, err := fileKey(key, hdr[12:28])
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(keyCheck(fk, hdr[:28]), hdr[28:]) {
		return nil, ErrWrongKey
	}
	blk, err := aes.NewCipher(fk)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &reader{r: r, aead: aead, hdr: hdr, in: make([]byte, int(chunk)+tagLen+1)}, nil
}

func (d *reader) fill() error {
	// read one sealed chunk plus one byte of look-ahead to know whether it is the last
	size := len(d.in) - 1
	buf := d.in
	n := copy(buf, d.next)
	d.next = d.next[:0]
	m, err := io.ReadFull(d.r, buf[n:])
	n += m
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	last := n <= size
	if last && n < tagLen {
		return ErrTruncated
	}
	body := n
	if !last {
		body = size
		d.next = append(d.next[:0], buf[size])
	}
	pt, oerr := d.aead.Open(nil, nonce(d.idx, last), buf[:body], d.hdr)
	if oerr != nil {
		if !last { // maybe it was the last chunk and trailing garbage follows, or a middle chunk was cut
			if _, e2 := d.aead.Open(nil, nonce(d.idx, true), buf[:body], d.hdr); e2 == nil {
				return ErrCorrupt
			}
		} else if _, e2 := d.aead.Open(nil, nonce(d.idx, false), buf[:body], d.hdr); e2 == nil {
			return ErrTruncated // a middle chunk with nothing after it
		}
		return ErrCorrupt
	}
	d.idx++
	d.out = pt
	d.final = last
	return nil
}

func (d *reader) Read(p []byte) (int, error) {
	for len(d.out) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.final {
			d.err = io.EOF
			continue
		}
		if err := d.fill(); err != nil {
			d.err = err
			return 0, err
		}
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	return n, nil
}

// Seal encrypts data in memory.
func Seal(key, data []byte) ([]byte, error) {
	var b bytes.Buffer
	w, err := NewWriter(&b, key)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Open decrypts data in memory.
func Open(key, data []byte) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(data), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// IsEncrypted reports whether b begins with the file magic.
func IsEncrypted(b []byte) bool { return bytes.HasPrefix(b, []byte(magic)) }

// EncryptFile writes src encrypted to dst (created 0600, fsynced).
func EncryptFile(src, dst string, key []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w, err := NewWriter(out, key)
	if err == nil {
		_, err = io.Copy(w, in)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// DecryptFile writes the plaintext of src to dst; on any error dst is removed.
func DecryptFile(src, dst string, key []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r, err := NewReader(in, key)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
