package sql

import (
	"strconv"
	"strings"
)

// intervalTypmod reads what follows INTERVAL in a type name: "(p)", "YEAR", "DAY TO SECOND(p)" and so on, and
// returns the column spec (interval.go ivSpecParse).
func (p *parser) intervalTypmod() string {
	tm := ivTypmod{mask: ivFAll, prec: -1}
	prec := func() {
		if p.peek().s == "(" && p.peekN(1).k == tNumber && p.peekN(2).s == ")" {
			p.next()
			tm.prec, _ = strconv.Atoi(p.next().s)
			p.next()
		}
	}
	prec()
	order := []struct {
		name string
		bit  int
	}{{"year", ivFYear}, {"month", ivFMonth}, {"day", ivFDay}, {"hour", ivFHour}, {"minute", ivFMinute}, {"second", ivFSecond}}
	field := func() int {
		if p.peek().k != tIdent {
			return -1
		}
		s := p.peek().s
		for i, o := range order {
			if s == o.name || s == o.name+"s" {
				p.next()
				return i
			}
		}
		return -1
	}
	if a := field(); a >= 0 {
		b := a
		if p.acceptKw("to") {
			if x := field(); x >= 0 {
				b = x
			}
		}
		tm.mask = 0
		for i := a; i <= b; i++ {
			tm.mask |= order[i].bit
		}
		prec()
	}
	return ivSpecString(tm)
}

// seriesTimeKind is the result kind of generate_series over time arguments given as typed literals or casts (KNull: not time).
func seriesTimeKind(args []Expr) Kind {
	if len(args) < 3 {
		return KNull
	}
	if c, ok := args[0].(*Cast); ok {
		switch kindOfType(c.Type) {
		case KTimestamp:
			return KTimestamp
		case KTime, KDate:
			return KTime
		}
	}
	return KNull
}

// ivBareNumber: v is a number with no unit ('2', '-1.5').
func ivBareNumber(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if v[0] == '+' || v[0] == '-' {
		v = v[1:]
	}
	dot := false
	for i := 0; i < len(v); i++ {
		switch {
		case v[i] == '.' && !dot:
			dot = true
		case v[i] < '0' || v[i] > '9':
			return false
		}
	}
	return v != "" && v != "."
}

// ivLastUnit names the finest field of a field mask.
func ivLastUnit(mask int) string {
	switch {
	case mask&ivFSecond != 0:
		return "second"
	case mask&ivFMinute != 0:
		return "minute"
	case mask&ivFHour != 0:
		return "hour"
	case mask&ivFDay != 0:
		return "day"
	case mask&ivFMonth != 0:
		return "month"
	}
	return "year"
}
