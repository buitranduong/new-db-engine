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
