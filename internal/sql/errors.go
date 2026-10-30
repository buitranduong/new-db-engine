package sql

import (
	"errors"
	"fmt"

	"stask/internal/core"
)

// SQLSTATE codes (PostgreSQL appendix A) used by STASK.
const (
	codeSyntax            = "42601"
	codeUndefinedTable    = "42P01"
	codeUndefinedColumn   = "42703"
	codeDuplicateTable    = "42P07"
	codeAmbiguousColumn   = "42702"
	codeGrouping          = "42803"
	codeDatatypeMismatch  = "42804"
	codeUndefinedFunction = "42883"
	codeUndefinedParam    = "42P02"
	codeUniqueViolation   = "23505"
	codeNotNullViolation  = "23502"
	codeInvalidText       = "22P02"
	codeInvalidParameter  = "22023"
	codeWindowing         = "42P20"
	codeCardinality       = "21000"
	codeUndefinedObject   = "42704"
	codeInvalidRegex      = "2201B"
	codeDivisionByZero    = "22012"
	codeNumericOutOfRange = "22003"
	codeSerialization     = "40001"
	codeInFailedTx        = "25P02"
	codeNoActiveTx        = "25P01"
	codeActiveTx          = "25001"
	codeFeature           = "0A000"
	codeSnapshotTooOld    = "72000"
	codeIOError           = "58030"
	codeInternal          = "XX000"
	codeObjectInUse       = "55006"
	codeDuplicateObject   = "42710"
	codeWrongObjectType   = "42809"
	codeObjectNotInState  = "55000"
	codeQueryCanceled     = "57014"
)

// Error is an SQL error with a SQLSTATE code.
type Error struct {
	Code string
	Msg  string
	Pos  int // 1-based character position in the query, 0 = unknown
}

func (e *Error) Error() string { return e.Msg }

func errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

func syntaxErr(pos int, msg string) *Error {
	return &Error{Code: codeSyntax, Msg: msg, Pos: pos + 1}
}

// AsError maps any error to an SQL error.
func AsError(err error) *Error {
	var se *Error
	if ce := fkErrorOf(err); ce != nil {
		return ce
	}
	switch {
	case errors.As(err, &se):
		return se
	case errors.Is(err, core.ErrConflict):
		return errf(codeSerialization, "could not serialize access due to concurrent update")
	case errors.As(err, new(*core.KeyExistsError)):
		var ke *core.KeyExistsError
		errors.As(err, &ke)
		con := ke.Table + "_pkey"
		if ke.Constraint != "" {
			con = ke.Constraint
		}
		return errf(codeUniqueViolation, "duplicate key value violates unique constraint \"%s\": Key (%s)=(%s) already exists", con, ke.Column, ke.Key)
	case errors.Is(err, core.ErrExists):
		return errf(codeUniqueViolation, "duplicate key value violates unique constraint: %v", err)
	case errors.Is(err, core.ErrNoSequence):
		return errf(codeUndefinedTable, "%v", err)
	case errors.Is(err, core.ErrSequenceExists):
		return errf(codeDuplicateTable, "%v", err)
	case errors.Is(err, core.ErrSequenceLimit):
		return errf("2200H", "%v", err)
	case errors.Is(err, core.ErrSequenceBounds):
		return errf("22003", "%v", err)
	case errors.Is(err, core.ErrViewExists):
		return errf(codeDuplicateTable, "%v", err)
	case errors.Is(err, core.ErrNoTable):
		return errf(codeUndefinedTable, "%v", err)
	case errors.Is(err, core.ErrSnapshotTooOld):
		return errf(codeSnapshotTooOld, "%v", err)
	case errors.Is(err, core.ErrUnknownOutcome):
		return errf(codeIOError, "%v", err)
	case errors.Is(err, core.ErrAppendOnly):
		return errf(codeFeature, "%v", err)
	case errors.Is(err, core.ErrInterrupted):
		return errf(codeQueryCanceled, "canceling statement due to user request")
	case errors.Is(err, core.ErrScanSerializable):
		return errf(codeFeature, "%v", err)
	}
	return errf(codeInternal, "%v", err)
}
