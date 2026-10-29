package core

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// SCRAM-SHA-256 verifiers (RFC 5802, RFC 7677), stored in the form
// PostgreSQL keeps in pg_authid.rolpassword:
//
//	SCRAM-SHA-256$<iterations>:<salt b64>$<StoredKey b64>:<ServerKey b64>
//
// The password cannot be recovered from it, yet it is enough to check a
// SCRAM proof (pgwire) or a cleartext password.

const (
	scramPrefix     = "SCRAM-SHA-256$"
	ScramIterations = 4096 // PostgreSQL's default scram_iterations
	scramSaltLen    = 16
)

// SCRAM is a parsed verifier.
type SCRAM struct {
	Salt      []byte
	Iters     int
	StoredKey []byte
	ServerKey []byte
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// NewSCRAM derives a verifier from a password with a fresh salt. (PostgreSQL
// applies SASLprep first; for ASCII passwords without control characters
// that is the identity.)
func NewSCRAM(password string) (*SCRAM, error) {
	salt := make([]byte, scramSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return scramFrom(password, salt, ScramIterations)
}

func scramFrom(password string, salt []byte, iters int) (*SCRAM, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iters, sha256.Size)
	if err != nil {
		return nil, err
	}
	ck := hmacSHA256(salted, "Client Key")
	sk := sha256.Sum256(ck)
	return &SCRAM{Salt: salt, Iters: iters, StoredKey: sk[:], ServerKey: hmacSHA256(salted, "Server Key")}, nil
}

// CheckPassword reports whether password derives this verifier (constant
// time in the comparison).
func (v *SCRAM) CheckPassword(password string) bool {
	w, err := scramFrom(password, v.Salt, v.Iters)
	return err == nil && subtle.ConstantTimeCompare(w.StoredKey, v.StoredKey) == 1
}

// String is the pg_authid form.
func (v *SCRAM) String() string {
	b := base64.StdEncoding
	return scramPrefix + strconv.Itoa(v.Iters) + ":" + b.EncodeToString(v.Salt) + "$" +
		b.EncodeToString(v.StoredKey) + ":" + b.EncodeToString(v.ServerKey)
}

// IsSCRAMVerifier reports whether s looks like a stored verifier (a
// password given to CREATE ROLE in that form is stored as it is, as in
// PostgreSQL).
func IsSCRAMVerifier(s string) bool { return strings.HasPrefix(s, scramPrefix) }

// ParseSCRAM parses the pg_authid form.
func ParseSCRAM(s string) (*SCRAM, error) {
	bad := errors.New("invalid SCRAM verifier")
	rest, ok := strings.CutPrefix(s, scramPrefix)
	if !ok {
		return nil, bad
	}
	iterSalt, keys, ok := strings.Cut(rest, "$")
	if !ok {
		return nil, bad
	}
	iters, saltB, ok := strings.Cut(iterSalt, ":")
	stored, server, ok2 := strings.Cut(keys, ":")
	if !ok || !ok2 {
		return nil, bad
	}
	n, err := strconv.Atoi(iters)
	if err != nil || n < 1 || n > 1<<24 {
		return nil, bad
	}
	b := base64.StdEncoding
	v := &SCRAM{Iters: n}
	if v.Salt, err = b.DecodeString(saltB); err != nil {
		return nil, bad
	}
	if v.StoredKey, err = b.DecodeString(stored); err != nil || len(v.StoredKey) != sha256.Size {
		return nil, bad
	}
	if v.ServerKey, err = b.DecodeString(server); err != nil || len(v.ServerKey) != sha256.Size {
		return nil, bad
	}
	return v, nil
}

// HMACSHA256 is exported for the SASL exchange in pgwire.
func HMACSHA256(key []byte, s string) []byte { return hmacSHA256(key, s) }
