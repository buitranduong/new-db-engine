package core

import (
	"errors"
	"fmt"
)

// CodedError is an engine error that has a SQLSTATE, for the conditions a
// client can reach and must not see as an internal error (XX000). The SQL
// layer turns it into its own error (sql.AsError).
type CodedError struct {
	Code string
	Msg  string
}

func (e *CodedError) Error() string { return e.Msg }

func coded(code, format string, a ...any) error {
	return &CodedError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// IsCoded returns the CodedError in err's chain.
func IsCoded(err error) (*CodedError, bool) {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}
