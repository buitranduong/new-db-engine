package sql

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"stask/internal/core"
	"stask/internal/wal"
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
	codeDataCorrupted     = "XX001"
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
	// Detail and Hint are the D and H fields of the ErrorResponse (RAISE ... USING DETAIL / HINT).
	Detail, Hint string
	// Schema, Table, Column, DataType and Constraint are the s, t, c, d and n fields of the ErrorResponse
	// (constraint violations, 22xxx data errors): pgjdbc, Hibernate and psycopg read them (errfields.go).
	Schema, Table, Column, DataType, Constraint string
	// Routine is the R field of the ErrorResponse; pgjdbc retries a statement transparently when the server
	// answers 0A000 from RevalidateCachedQuery (cached plan must not change result type).
	Routine string
}

// Notice is one NoticeResponse the session queues while a statement runs (RAISE NOTICE,
// WARNING, INFO inside a function): the wire layer sends them with TakeNotices.
type Notice struct {
	Sev, Code, Msg, Detail, Hint string
}

func (e *Error) Error() string { return e.Msg }

func errf(code, format string, a ...any) *Error {
	msg := fmt.Sprintf(format, a...)
	if code != codeUndefinedTable && code != codeUndefinedObject && strings.Contains(msg, ".") {
		msg = relQualRE.ReplaceAllString(msg, `$1 "$3"`) // PostgreSQL names a relation in a message without its schema
	}
	return &Error{Code: code, Msg: msg}
}

var relQualRE = regexp.MustCompile(`\b(relation|table|index|sequence|view) "([a-z_][a-z0-9_$]*)\.([^"]+)"`)

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
	case errors.Is(err, core.ErrReadOnly):
		return errf("25006", "cannot execute a write on a read-only node (replica or cluster follower): %v", err)
	case errors.Is(err, core.ErrConflict):
		return errf(codeSerialization, "could not serialize access due to concurrent update")
	case errors.As(err, new(*core.KeyExistsError)):
		var ke *core.KeyExistsError
		errors.As(err, &ke)
		con := ke.Table + "_pkey"
		if ke.Constraint != "" {
			con = ke.Constraint
		}
		return uniqueViolation(ke.Table, con, ke.Column, ke.Key)
	case errors.Is(err, core.ErrExists):
		return errf(codeUniqueViolation, "duplicate key value violates unique constraint: %v", err)
	case errors.As(err, new(*core.CodedError)):
		ce, _ := core.IsCoded(err)
		return errf(ce.Code, "%s", ce.Msg)
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
	case errors.Is(err, wal.ErrCorrupt):
		return errf(codeDataCorrupted, "%v", err)
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
