package sql

import (
	"sort"
	"strings"
)

// SHOW ALL: name, setting, description of every setting the session knows.

func isShowAll(name string) bool { return strings.EqualFold(strings.TrimSpace(name), "all") }

func showAllCols() []Column {
	return []Column{{Name: "name", Kind: KText}, {Name: "setting", Kind: KText}, {Name: "description", Kind: KText}}
}

func (s *Session) showAll() *Result {
	seen := map[string]string{}
	for k, v := range settingDefaults {
		seen[k] = v
	}
	for k, v := range s.vars {
		if strings.HasPrefix(k, "stask.") {
			continue
		}
		seen[k] = v
	}
	names := make([]string, 0, len(seen))
	for k := range seen {
		names = append(names, k)
	}
	sort.Strings(names)
	res := &Result{Cols: showAllCols(), Tag: "SHOW", HasRows: true}
	for _, k := range names {
		res.Rows = append(res.Rows, []Datum{TextD(k), TextD(seen[k]), TextD(settingDescriptions[k])})
	}
	return res
}

var settingDescriptions = map[string]string{
	"application_name":              "Sets the application name to be reported in statistics and logs.",
	"client_encoding":               "Sets the client's character set encoding.",
	"datestyle":                     "Sets the display format for date and time values.",
	"default_transaction_isolation": "Sets the transaction isolation level of each new transaction.",
	"lock_timeout":                  "Sets the maximum allowed duration of any wait for a lock.",
	"search_path":                   "Sets the schema search order for names that are not schema-qualified.",
	"server_version":                "Shows the server version.",
	"statement_timeout":             "Sets the maximum allowed duration of any statement.",
	"timezone":                      "Sets the time zone for displaying and interpreting time stamps.",
	"role":                          "Sets the current role.",
	"session_authorization":         "Sets the session user name.",
}

// discardAllExtra is the part of DISCARD ALL that belongs to the cursor,
// lock and role state: cursors closed, advisory locks released, the role and
// session authorization back to the login, prepared statements and
// listeners dropped.
func (s *Session) discardAllExtra() {
	s.closeCursors()
	s.releaseAllLocks()
	s.resetRole()
	s.prepared = nil
	s.applyListen(listenOp{all: true})
}
