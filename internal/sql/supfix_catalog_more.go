package sql

import (
	"time"

	"stask/internal/core"
)

func init() {
	catalogFuncKinds["pg_sleep"] = KText
	sessionFuncs["pg_sleep"] = true
	settingDefaults["max_connections"] = "100"
}

// supfixSleep is pg_sleep(seconds): it waits, in short steps so that a cancel or a statement
// timeout interrupts it, and returns void (printed as an empty string).
func (s *Session) supfixSleep(args []Datum) (Datum, error) {
	if len(args) != 1 || args[0].IsNull() {
		return TextD(""), nil
	}
	f, err := coerce(args[0], KFloat)
	if err != nil {
		return Null, err
	}
	end := time.Now().Add(time.Duration(f.F * float64(time.Second)))
	for {
		left := time.Until(end)
		if left <= 0 {
			return TextD(""), nil
		}
		if s.interrupted() {
			return Null, core.ErrInterrupted
		}
		time.Sleep(min(left, 20*time.Millisecond))
	}
}

// The five domains of information_schema as pg_type rows (typtype 'd'), which SQLAlchemy's
// get_columns lists with every reflection.
const domainOIDBase = 13001

var infoDomains = []struct {
	name, cat string
	base      int64
	mod       int64
	def       Datum
}{
	{"cardinal_number", "N", 23, -1, Null},
	{"character_data", "S", 1043, -1, Null},
	{"sql_identifier", "S", 19, -1, Null},
	{"time_stamp", "D", 1184, 2, TextD("CURRENT_TIMESTAMP(2)")},
	{"yes_or_no", "S", 1043, 7, Null},
}

func supfixDomainRows(rows [][]Datum) [][]Datum {
	for i, d := range infoDomains {
		rows = append(rows, []Datum{IntD(domainOIDBase + int64(i)), TextD(d.name), IntD(nsInfoSchema), IntD(ownerOID), IntD(-1),
			BoolD(false), TextD("d"), TextD(d.cat), BoolD(false), BoolD(true), TextD(","), IntD(0),
			IntD(0), IntD(0), IntD(0), TextD("domain_in"), TextD("domain_recv"), TextD("-"), TextD("-"), TextD("-"), TextD("-"), TextD("-"),
			TextD("i"), TextD("p"), BoolD(false), IntD(d.base), IntD(d.mod), IntD(0), IntD(0), Null, d.def, Null})
	}
	return rows
}

func isInfoDomain(oid int64) bool {
	return oid >= domainOIDBase && oid < domainOIDBase+int64(len(infoDomains))
}
