package sql

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
	"time"
	"unicode"
)

// Functions of the supported extensions, mapped to built-in code (branch
// ddlobjs). A function of an extension needs CREATE EXTENSION first, as in
// PostgreSQL (the older uuid_generate_v4 is accepted without it).
//
//	pgcrypto:  digest, hmac, gen_random_bytes  (crypt, gen_salt: not implemented)
//	uuid-ossp: uuid_generate_v1, _v1mc, _v3, _v5, uuid_nil, uuid_ns_*
//	pg_trgm:   similarity

var extFuncNeeds = map[string]string{
	"digest": "pgcrypto", "hmac": "pgcrypto", "gen_random_bytes": "pgcrypto", "crypt": "pgcrypto", "gen_salt": "pgcrypto",
	"uuid_generate_v1": "uuid-ossp", "uuid_generate_v1mc": "uuid-ossp", "uuid_generate_v3": "uuid-ossp",
	"uuid_generate_v5": "uuid-ossp", "uuid_nil": "uuid-ossp", "uuid_ns_dns": "uuid-ossp", "uuid_ns_url": "uuid-ossp",
	"uuid_ns_oid": "uuid-ossp", "uuid_ns_x500": "uuid-ossp",
	"similarity": "pg_trgm", "unaccent": "unaccent",
}

// extFuncKinds are the result kinds of the functions of this file, and of
// the core functions it adds (encode, decode, sha256...).
var extFuncKinds = map[string]Kind{
	"digest": KBytea, "hmac": KBytea, "gen_random_bytes": KBytea, "decode": KBytea, "encode": KText,
	"sha224": KBytea, "sha256": KBytea, "sha384": KBytea, "sha512": KBytea,
	"uuid_generate_v1": KUUID, "uuid_generate_v1mc": KUUID, "uuid_generate_v3": KUUID, "uuid_generate_v5": KUUID,
	"uuid_nil": KUUID, "uuid_ns_dns": KUUID, "uuid_ns_url": KUUID, "uuid_ns_oid": KUUID, "uuid_ns_x500": KUUID,
	"similarity": KFloat, "unaccent": KText,
}

func rawBytes(d Datum) string {
	if d.K == KBytea || d.K == KText {
		return d.S
	}
	return d.String()
}

func newHash(name string) func() hash.Hash {
	switch strings.ToLower(name) {
	case "md5":
		return md5.New
	case "sha1":
		return sha1.New
	case "sha224":
		return sha256.New224
	case "sha256":
		return sha256.New
	case "sha384":
		return sha512.New384
	case "sha512":
		return sha512.New
	}
	return nil
}

func uuidD(b [16]byte) Datum {
	return Datum{K: KUUID, S: fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])}
}

func parseUUID(s string) ([16]byte, bool) {
	var b [16]byte
	h := strings.ReplaceAll(strings.Trim(s, "{}"), "-", "")
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw) != 16 {
		return b, false
	}
	copy(b[:], raw)
	return b, true
}

var uuidNS = map[string]string{
	"uuid_nil": "00000000-0000-0000-0000-000000000000", "uuid_ns_dns": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	"uuid_ns_url": "6ba7b811-9dad-11d1-80b4-00c04fd430c8", "uuid_ns_oid": "6ba7b812-9dad-11d1-80b4-00c04fd430c8",
	"uuid_ns_x500": "6ba7b814-9dad-11d1-80b4-00c04fd430c8",
}

// trigrams of a string, as pg_trgm makes them: lower-cased words of
// letters and digits, padded with two blanks before and one after.
func trigrams(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		rs := []rune("  " + w + " ")
		for i := 0; i+3 <= len(rs); i++ {
			out[string(rs[i:i+3])] = true
		}
	}
	return out
}

func similarity(a, b string) float64 {
	x, y := trigrams(a), trigrams(b)
	if len(x) == 0 && len(y) == 0 {
		return 0
	}
	both := 0
	for t := range x {
		if y[t] {
			both++
		}
	}
	return float64(float32(float64(both) / float64(len(x)+len(y)-both)))
}

// extCall runs the functions of this file (funcs.go callPre).
func (c *evalCtx) extCall(f *Func, args []Datum) (Datum, bool, error) {
	_, isExt := extFuncKinds[f.Name]
	need, gated := extFuncNeeds[f.Name]
	if !isExt && !gated {
		return Null, false, nil
	}
	if gated {
		if s := c.session(); s != nil && !s.extInstalled(need) {
			ts := make([]string, len(args))
			for i, a := range args {
				ts[i] = "unknown"
				if l, ok := f.Args[i].(*Lit); !ok || l.V.K != KText {
					ts[i] = typeNameOfKind(a.K)
				}
			}
			return Null, true, errf(codeUndefinedFunction, "function %s(%s) does not exist", f.Name, strings.Join(ts, ", "))
		}
	}
	nullIn := func() bool {
		for _, a := range args {
			if a.IsNull() {
				return true
			}
		}
		return false
	}
	bad := func(n int) (Datum, bool, error) {
		return Null, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", f.Name, n)
	}
	switch f.Name {
	case "host", "family", "masklen", "netmask", "network":
		d, ok, err := inetFunc(f.Name, args)
		if !ok && err == nil {
			return bad(len(args))
		}
		return d, true, err
	case "crypt", "gen_salt":
		return Null, true, errf(codeFeature, "%s is not implemented", f.Name)
	case "digest":
		if len(args) != 2 {
			return bad(len(args))
		}
		if nullIn() {
			return Null, true, nil
		}
		nh := newHash(args[1].String())
		if nh == nil {
			return Null, true, errf("39000", "digest: Unknown algorithm: %s", args[1].String())
		}
		h := nh()
		h.Write([]byte(rawBytes(args[0])))
		return Datum{K: KBytea, S: string(h.Sum(nil))}, true, nil
	case "sha224", "sha256", "sha384", "sha512":
		if len(args) != 1 {
			return bad(len(args))
		}
		if nullIn() {
			return Null, true, nil
		}
		h := newHash(f.Name)()
		h.Write([]byte(rawBytes(args[0])))
		return Datum{K: KBytea, S: string(h.Sum(nil))}, true, nil
	case "hmac":
		if len(args) != 3 {
			return bad(len(args))
		}
		if nullIn() {
			return Null, true, nil
		}
		nh := newHash(args[2].String())
		if nh == nil {
			return Null, true, errf("39000", "hmac: Unknown algorithm: %s", args[2].String())
		}
		m := hmac.New(nh, []byte(rawBytes(args[1])))
		m.Write([]byte(rawBytes(args[0])))
		return Datum{K: KBytea, S: string(m.Sum(nil))}, true, nil
	case "gen_random_bytes":
		if len(args) != 1 {
			return bad(len(args))
		}
		if nullIn() {
			return Null, true, nil
		}
		n, err := coerce(args[0], KInt)
		if err != nil {
			return Null, true, err
		}
		if n.I < 1 || n.I > 1024 {
			return Null, true, errf("22023", "Length not in range")
		}
		b := make([]byte, n.I)
		rand.Read(b)
		return Datum{K: KBytea, S: string(b)}, true, nil
	case "encode":
		if len(args) != 2 {
			return bad(len(args))
		}
		if nullIn() {
			return Null, true, nil
		}
		raw := []byte(rawBytes(args[0]))
		switch strings.ToLower(args[1].String()) {
		case "hex":
			return TextD(hex.EncodeToString(raw)), true, nil
		case "base64":
			return TextD(base64.StdEncoding.EncodeToString(raw)), true, nil
		case "escape":
			var b strings.Builder
			for _, x := range raw {
				switch {
				case x == '\\':
					b.WriteString(`\\`)
				case x < 32 || x > 126:
					fmt.Fprintf(&b, `\%03o`, x)
				default:
					b.WriteByte(x)
				}
			}
			return TextD(b.String()), true, nil
		}
		return Null, true, errf("22023", "unrecognized encoding: %q", args[1].String())
	case "decode":
		if len(args) != 2 {
			return bad(len(args))
		}
		if nullIn() {
			return Null, true, nil
		}
		in := args[0].String()
		switch strings.ToLower(args[1].String()) {
		case "hex":
			raw, err := hex.DecodeString(in)
			if err != nil {
				return Null, true, errf("22023", "invalid hexadecimal data: %s", in)
			}
			return Datum{K: KBytea, S: string(raw)}, true, nil
		case "base64":
			raw, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(in))
			if err != nil {
				return Null, true, errf("22023", "invalid base64 end sequence")
			}
			return Datum{K: KBytea, S: string(raw)}, true, nil
		case "escape":
			return Datum{K: KBytea, S: in}, true, nil
		}
		return Null, true, errf("22023", "unrecognized encoding: %q", args[1].String())
	case "uuid_nil", "uuid_ns_dns", "uuid_ns_url", "uuid_ns_oid", "uuid_ns_x500":
		b, _ := parseUUID(uuidNS[f.Name])
		return uuidD(b), true, nil
	case "uuid_generate_v1", "uuid_generate_v1mc":
		// 100 ns ticks since 1582-10-15, a random clock sequence and node
		const gregorian = 122192928000000000
		t := uint64(time.Now().UnixNano()/100) + gregorian
		var b [16]byte
		binary.BigEndian.PutUint32(b[0:], uint32(t))
		binary.BigEndian.PutUint16(b[4:], uint16(t>>32))
		binary.BigEndian.PutUint16(b[6:], uint16(t>>48)&0x0fff|0x1000)
		rand.Read(b[8:])
		b[8] = b[8]&0x3f | 0x80
		b[10] |= 0x01 // multicast bit: a random node, not a MAC address (v1 and v1mc alike)
		return uuidD(b), true, nil
	case "uuid_generate_v3", "uuid_generate_v5":
		if len(args) != 2 {
			return bad(len(args))
		}
		if nullIn() {
			return Null, true, nil
		}
		ns, ok := parseUUID(args[0].String())
		if !ok {
			return Null, true, errf(codeInvalidText, "invalid input syntax for type uuid: %q", args[0].String())
		}
		var sum []byte
		ver := byte(0x30)
		if f.Name == "uuid_generate_v3" {
			h := md5.New()
			h.Write(ns[:])
			h.Write([]byte(args[1].String()))
			sum = h.Sum(nil)
		} else {
			h := sha1.New()
			h.Write(ns[:])
			h.Write([]byte(args[1].String()))
			sum = h.Sum(nil)
			ver = 0x50
		}
		var b [16]byte
		copy(b[:], sum)
		b[6], b[8] = b[6]&0x0f|ver, b[8]&0x3f|0x80
		return uuidD(b), true, nil
	case "unaccent":
		if len(args) != 1 {
			return bad(len(args))
		}
		if nullIn() {
			return Null, true, nil
		}
		return TextD(unaccent(args[0].String())), true, nil
	case "similarity":
		if len(args) != 2 {
			return bad(len(args))
		}
		if nullIn() {
			return Null, true, nil
		}
		r := FloatD(similarity(args[0].String(), args[1].String()))
		r.S = f4Mark // real
		return r, true, nil
	}
	return Null, false, nil
}
