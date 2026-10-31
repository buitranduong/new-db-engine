package sql

import (
	"errors"
	"fmt"
	"strings"

	"stask/internal/core"
)

// ALTER INDEX / ALTER TABLE ... SET (index_storage = 'disk' | 'memory')
// (core/secdisk.go, docs/18). Secondary indexes only; the primary index
// stays in RAM (D29 b). Needs the server started with -secindex-disk; without
// it 'disk' is refused and nothing changes.
//
//	ALTER INDEX idx SET (index_storage = 'disk')
//	ALTER TABLE t SET (index_storage = 'disk')     -- every secondary index of t now
//	ALTER TABLE t RESET (index_storage)            -- back to memory

const indexStorageOpt = "index_storage"

// storageFromOpts reads the value of index_storage from opts, which must hold
// no other option.
func storageFromOpts(opts map[string]string) (string, error) {
	v, ok := opts[indexStorageOpt]
	if !ok || len(opts) != 1 {
		return "", errf(codeFeature, "ALTER INDEX supports only the option %s", indexStorageOpt)
	}
	switch v {
	case "disk", "memory", "":
		return v, nil
	}
	return "", errf(codeInvalidParameter, "invalid value for option %q: %q (use 'disk' or 'memory')", indexStorageOpt, v)
}

func storageErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, core.ErrDiskIndexOff):
		return errf(codeFeature, "%v", err)
	case errors.Is(err, core.ErrIndexBudget):
		return errf("53200", "%v", err)
	}
	return AsError(err)
}

func (s *Session) alterIndex(st *AlterIndex) (*Result, error) {
	res := &Result{Tag: "ALTER INDEX"}
	table, ok := s.E.FindIndex(st.Name)
	if !ok {
		if st.IfExists {
			res.Notice = fmt.Sprintf("relation %q does not exist, skipping", st.Name)
			return res, nil
		}
		return nil, errf("42704", "relation %q does not exist", st.Name)
	}
	if _, has := st.Opts[indexStorageOpt]; !has && pgStorageParams(st.Opts) {
		return res, nil // fillfactor, deduplicate_items, ...: accepted, no effect
	}
	v, err := storageFromOpts(st.Opts)
	if err != nil {
		return nil, err
	}
	if err := s.E.SetIndexStorage(table, st.Name, v); err != nil {
		return nil, storageErr(err)
	}
	return res, nil
}

// alterTableIndexStorage handles ALTER TABLE t SET/RESET (index_storage ...).
// ok is false when the statement has no such option.
func (s *Session) alterTableIndexStorage(st *AlterTable) (*Result, bool, error) {
	var opts map[string]string
	for _, a := range st.Actions {
		if _, has := a.Opts[indexStorageOpt]; a.Kind == "set_options" && has {
			opts = a.Opts
		}
	}
	if opts == nil {
		return nil, false, nil
	}
	if len(st.Actions) != 1 || len(opts) != 1 {
		return nil, true, errf(codeFeature, "%s cannot be combined with other ALTER TABLE actions or options", indexStorageOpt)
	}
	v, err := storageFromOpts(opts)
	if err != nil {
		return nil, true, err
	}
	if _, err := s.userTable(st.Name); err != nil {
		if st.IfExists {
			return &Result{Tag: "ALTER TABLE", Notice: fmt.Sprintf("relation %q does not exist, skipping", st.Name)}, true, nil
		}
		return nil, true, errf(codeUndefinedTable, "relation %q does not exist", st.Name)
	}
	if err := s.E.SetTableIndexStorage(st.Name, v); err != nil {
		return nil, true, storageErr(err)
	}
	return &Result{Tag: "ALTER TABLE"}, true, nil
}

// pgStorageParams reports whether every option is a PostgreSQL storage parameter that has no meaning
// here (rows are not heap tuples and there is no autovacuum): ALTER TABLE/INDEX .. SET (..) accepts them.
func pgStorageParams(opts map[string]string) bool {
	for k := range opts {
		k = strings.ToLower(k)
		switch {
		case k == "fillfactor", k == "parallel_workers", k == "user_catalog_table", k == "deduplicate_items",
			k == "buffering", k == "fastupdate", k == "gin_pending_list_limit", k == "pages_per_range", k == "autosummarize",
			k == "log_autovacuum_min_duration", k == "vacuum_truncate", k == "vacuum_index_cleanup", k == "toast_tuple_target",
			strings.HasPrefix(k, "autovacuum_"), strings.HasPrefix(k, "toast."):
		default:
			return false
		}
	}
	return len(opts) > 0
}
