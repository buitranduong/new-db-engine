package sql

import (
	"net"
	"net/netip"
	"strings"
)

// Network address types, minimal (docs/14 appendix P): inet, cidr and macaddr are text
// columns (pg spec "inet", "cidr", "macaddr") whose input is validated and written in
// PostgreSQL's canonical form, so equality, DISTINCT, GROUP BY, UNIQUE and text output agree
// with PostgreSQL. Functions host, family, masklen, netmask, network, text and the operators
// << and >> (containment) work on them. Ordering and range comparison are byte order of the
// canonical text, NOT address order (10.0.0.10 sorts before 10.0.0.9); address-order
// comparison, <<=, >>=, &&, +/- on addresses and abbreviations such as abbrev() are not done.
// The binary wire format is supported for results (inet, cidr).

const (
	tyInet    = 869
	tyCidr    = 650
	tyMacaddr = 829
)

func init() {
	for n, k := range map[string]Kind{"host": KText, "family": KInt, "masklen": KInt, "netmask": KText, "network": KText} {
		extFuncKinds[n] = k
	}
}

// inetCanon returns the canonical text of v as an inet (cidr=false) or cidr value.
func inetCanon(v string, cidr bool) (string, bool, error) {
	v = strings.TrimSpace(v)
	bad := func() (string, bool, error) {
		kind := "inet"
		if cidr {
			kind = "cidr"
		}
		return "", false, errf("22P02", "invalid input syntax for type %s: %q", kind, v)
	}
	var pfx netip.Prefix
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		if err != nil || p.Addr().Zone() != "" {
			return bad()
		}
		pfx = p
	} else {
		a, err := netip.ParseAddr(v)
		if err != nil || a.Zone() != "" {
			return bad()
		}
		pfx = netip.PrefixFrom(a, a.BitLen())
	}
	if pfx.Addr().Is4In6() && !strings.Contains(v, ":") {
		return bad()
	}
	if cidr {
		if pfx.Masked() != pfx {
			return "", false, errf("22P02", "invalid cidr value: %q", v)
		}
		return pfx.String(), true, nil
	}
	if pfx.Bits() == pfx.Addr().BitLen() {
		return pfx.Addr().String(), true, nil
	}
	return pfx.String(), true, nil
}

func macCanon(v string) (string, error) {
	s := strings.TrimSpace(v)
	hw, err := net.ParseMAC(s)
	if err != nil {
		// PostgreSQL also takes 08002b010203, 08002b:010203 and 0800.2b01.0203
		t := strings.NewReplacer(":", "", "-", "", ".", "").Replace(s)
		if len(t) == 12 {
			hw, err = net.ParseMAC(t[0:2] + ":" + t[2:4] + ":" + t[4:6] + ":" + t[6:8] + ":" + t[8:10] + ":" + t[10:12])
		}
	}
	if err != nil || len(hw) != 6 {
		return "", errf("22P02", "invalid input syntax for type macaddr: %q", v)
	}
	return hw.String(), nil
}

// applyInetPG normalizes d for the inet-family spec name.
func applyInetPG(name string, d Datum) (Datum, error) {
	if d.K != KText {
		return d, nil
	}
	switch name {
	case "inet", "cidr":
		s, _, err := inetCanon(d.S, name == "cidr")
		if err != nil {
			return Null, err
		}
		d.S = s
	case "macaddr":
		s, err := macCanon(d.S)
		if err != nil {
			return Null, err
		}
		d.S = s
	}
	return d, nil
}

func parsePrefixText(v string) (netip.Prefix, bool) {
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		return p, err == nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, a.BitLen()), true
}

// inetContains is a << b (strict=true) or b << a: the subnet relation of two networks.
func inetSubnet(a, b string) (bool, bool) {
	pa, ok1 := parsePrefixText(a)
	pb, ok2 := parsePrefixText(b)
	if !ok1 || !ok2 {
		return false, false
	}
	// a << b: a is strictly inside b
	return pa.Bits() > pb.Bits() && pb.Contains(pa.Addr()) && pa.Addr().BitLen() == pb.Addr().BitLen(), true
}

// inetOp evaluates a << b and a >> b on two text values that are networks; ok=false if they
// are not.
func inetOp(op string, l, r Datum) (Datum, bool) {
	if l.K != KText || r.K != KText || !strings.ContainsAny(l.S+r.S, ".:") {
		return Null, false
	}
	var in, out string
	if op == "<<" {
		in, out = l.S, r.S
	} else {
		in, out = r.S, l.S
	}
	v, ok := inetSubnet(in, out)
	if !ok {
		return Null, false
	}
	return BoolD(v), true
}

// inetFunc implements host, family, masklen, netmask, network on an inet/cidr text value.
func inetFunc(name string, args []Datum) (Datum, bool, error) {
	if len(args) != 1 {
		return Null, false, nil
	}
	if args[0].IsNull() {
		return Null, true, nil
	}
	p, ok := parsePrefixText(args[0].String())
	if !ok {
		return Null, true, errf("22P02", "invalid input syntax for type inet: %q", args[0].String())
	}
	switch name {
	case "host":
		return TextD(p.Addr().String()), true, nil
	case "family":
		if p.Addr().Is4() {
			return IntD(4), true, nil
		}
		return IntD(6), true, nil
	case "masklen":
		return IntD(int64(p.Bits())), true, nil
	case "netmask":
		bits := 32
		if p.Addr().Is6() {
			bits = 128
		}
		m := net.CIDRMask(p.Bits(), bits)
		a, _ := netip.AddrFromSlice(m)
		return TextD(a.String()), true, nil
	case "network":
		return TextD(p.Masked().String()), true, nil
	}
	return Null, false, nil
}

// inetBinary is the binary form of an inet or cidr value: family, bits, is_cidr, length, address.
func inetBinary(d Datum, cidr bool) ([]byte, bool) {
	if d.K != KText {
		return nil, false
	}
	p, ok := parsePrefixText(d.S)
	if !ok {
		return nil, false
	}
	fam := byte(2)
	if p.Addr().Is6() {
		fam = 3
	}
	ip := p.Addr().AsSlice()
	c := byte(0)
	if cidr {
		c = 1
	}
	return append([]byte{fam, byte(p.Bits()), c, byte(len(ip))}, ip...), true
}
