package sql

import (
	"strconv"
	"strings"
)

// Advisory locks: pg_advisory_lock and friends, on the soft lock table
// (locks.go). Session-level locks belong to the session and go with it;
// transaction-level locks go at COMMIT/ROLLBACK. Keys are a bigint or two
// int4, two separate key spaces as in PostgreSQL. A blocking call waits
// until the lock is free, the statement is cancelled, statement_timeout or
// lock_timeout expires (55P03), or a deadlock is found (40P01).

var advisoryFuncs = map[string]struct {
	xact, shared, try, unlock bool
	kind                      Kind
}{
	"pg_advisory_lock":                 {kind: KText},
	"pg_advisory_lock_shared":          {shared: true, kind: KText},
	"pg_try_advisory_lock":             {try: true, kind: KBool},
	"pg_try_advisory_lock_shared":      {try: true, shared: true, kind: KBool},
	"pg_advisory_unlock":               {unlock: true, kind: KBool},
	"pg_advisory_unlock_shared":        {unlock: true, shared: true, kind: KBool},
	"pg_advisory_xact_lock":            {xact: true, kind: KText},
	"pg_advisory_xact_lock_shared":     {xact: true, shared: true, kind: KText},
	"pg_try_advisory_xact_lock":        {xact: true, try: true, kind: KBool},
	"pg_try_advisory_xact_lock_shared": {xact: true, try: true, shared: true, kind: KBool},
}

// tyVoid is the type of the lock functions that return nothing.
const tyVoid = 2278

// voidFn: functions returning void (shown as an empty text); nameFn:
// functions returning the type name (current_user and its kin).
var (
	voidFn = map[string]bool{"pg_advisory_unlock_all": true}
	nameFn = map[string]bool{"current_user": true, "session_user": true, "current_role": true}
)

func init() {
	for n, f := range advisoryFuncs {
		sessionFuncs[n] = true
		catalogFuncKinds[n] = f.kind
		tableFuncCols[n] = []relCol{{name: n, kind: f.kind}}
		if f.kind == KText {
			voidFn[n] = true
		}
	}
	sessionFuncs["pg_advisory_unlock_all"] = true
	catalogFuncKinds["pg_advisory_unlock_all"] = KText
	tableFuncCols["pg_advisory_unlock_all"] = []relCol{{name: "pg_advisory_unlock_all", kind: KText}}
}

func advisoryKey(name string, args []Datum) (lockKey, error) {
	switch len(args) {
	case 1:
		if args[0].IsNull() {
			return lockKey{}, nil
		}
		v, err := coerce(args[0], KInt)
		if err != nil {
			return lockKey{}, err
		}
		return lockKey{sp: spAdvisory, a: "1:" + strconv.FormatInt(v.I, 10)}, nil
	case 2:
		if args[0].IsNull() || args[1].IsNull() {
			return lockKey{}, nil
		}
		a, err := coerce(args[0], KInt)
		if err != nil {
			return lockKey{}, err
		}
		b, err := coerce(args[1], KInt)
		if err != nil {
			return lockKey{}, err
		}
		if a.I < -1<<31 || a.I >= 1<<31 || b.I < -1<<31 || b.I >= 1<<31 {
			return lockKey{}, errf(codeUndefinedFunction, "function %s(integer, bigint) does not exist", name)
		}
		return lockKey{sp: spAdvisory, a: "2:" + strconv.FormatInt(a.I, 10) + ":" + strconv.FormatInt(b.I, 10)}, nil
	}
	return lockKey{}, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", name, len(args))
}

// extraSessionFunc runs the session functions of this file; ok=false for any other.
func (s *Session) extraSessionFunc(name string, args []Datum) ([][]Datum, bool, error) {
	if name == "pg_advisory_unlock_all" {
		if s.lk != nil {
			locksOf(s.E).releaseAll(s.lk.sess)
		}
		return [][]Datum{{TextD("")}}, true, nil
	}
	f, ok := advisoryFuncs[name]
	if !ok {
		return nil, false, nil
	}
	k, err := advisoryKey(name, args)
	if err != nil {
		return nil, true, err
	}
	if k.a == "" { // a NULL key: the function is strict
		return [][]Datum{{Null}}, true, nil
	}
	void := [][]Datum{{TextD("")}}
	mode := amExclusive
	if f.shared {
		mode = amShared
	}
	if f.unlock {
		var o *lockOwner
		if s.lk != nil {
			o = s.lk.sess
		}
		if o == nil || !locksOf(s.E).unlockOne(o, k, mode) {
			return [][]Datum{{BoolD(false)}}, true, nil
		}
		return [][]Datum{{BoolD(true)}}, true, nil
	}
	o := s.sessOwner()
	if f.xact {
		o = s.txOwner()
	}
	pol := waitBlock
	if f.try {
		pol = waitSkip
	}
	got, err := s.acquire(o, k, mode, pol)
	if err != nil {
		return nil, true, err
	}
	if f.try {
		return [][]Datum{{BoolD(got)}}, true, nil
	}
	return void, true, nil
}

// pg_locks: the advisory, table and row locks of the soft table.
func init() {
	defCat("pg_locks", "locktype:t database:i relation:t page:i tuple:i virtualxid:t transactionid:t classid:i objid:i objsubid:i "+
		"virtualtransaction:t pid:i mode:t granted:b fastpath:b",
		func(c *catSnap) [][]Datum {
			var rows [][]Datum
			for _, lr := range locksOf(c.s.E).snapshot() {
				pid := Null
				if sess := lr.o.node.s; sess != nil {
					pid = IntD(int64(sess.PID))
				}
				row := []Datum{Null, IntD(databaseOID), Null, Null, Null, Null, Null, Null, Null, Null, Null, pid, Null, BoolD(true), BoolD(false)}
				switch lr.k.sp {
				case spAdvisory:
					row[0] = TextD("advisory")
					parts := strings.Split(lr.k.a, ":")
					if parts[0] == "2" {
						row[7], row[8], row[9] = IntD(atoi64(parts[1])), IntD(atoi64(parts[2])), IntD(2)
					} else {
						v := atoi64(parts[1])
						row[7], row[8], row[9] = IntD(v>>32), IntD(v&0xffffffff), IntD(1)
					}
					row[12] = TextD(map[uint8]string{amShared: "ShareLock", amExclusive: "ExclusiveLock"}[lr.mode])
				case spTable:
					row[0], row[2] = TextD("relation"), TextD(lr.k.a)
					row[12] = TextD(tableModeLabel[lr.mode])
				default:
					row[0], row[2], row[4] = TextD("tuple"), TextD(lr.k.a), TextD(lr.k.b)
					row[12] = TextD(map[uint8]string{lmKeyShare: "RowShareLock", lmShare: "RowShareLock",
						lmNoKeyUpdate: "RowExclusiveLock", lmUpdate: "RowExclusiveLock"}[lr.mode])
				}
				rows = append(rows, row)
			}
			return rows
		})
}

var tableModeLabel = map[uint8]string{tmAccessShare: "AccessShareLock", tmRowShare: "RowShareLock", tmRowExclusive: "RowExclusiveLock",
	tmShareUpdateExclusive: "ShareUpdateExclusiveLock", tmShare: "ShareLock", tmShareRowExclusive: "ShareRowExclusiveLock",
	tmExclusive: "ExclusiveLock", tmAccessExclusive: "AccessExclusiveLock"}

func atoi64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
