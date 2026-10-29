package shadow

import (
	"fmt"
	"strings"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
)

// val is one column of a pgoutput tuple (text format).
type val struct {
	b  []byte
	st uint8 // 0 value, 1 NULL, 2 unchanged TOAST (value not sent)
}

const (
	vVal uint8 = iota
	vNull
	vUnchanged
)

func tupleVals(td *pglogrepl.TupleData) ([]val, error) {
	if td == nil {
		return nil, nil
	}
	out := make([]val, len(td.Columns))
	for i, c := range td.Columns {
		switch c.DataType {
		case pglogrepl.TupleDataTypeText:
			out[i] = val{b: c.Data}
		case pglogrepl.TupleDataTypeNull:
			out[i] = val{st: vNull}
		case pglogrepl.TupleDataTypeToast:
			out[i] = val{st: vUnchanged}
		default:
			return nil, fatalf("pgoutput sent a binary tuple column (type %q); this tool needs text format", c.DataType)
		}
	}
	return out, nil
}

// relCol is a column of a relation as announced by a pgoutput Relation message.
type relCol struct {
	Name string
	Kind Kind
}

// rel is the apply-side description of one replicated table.
type rel struct {
	tbl  *Table
	cols []relCol
	pk   []int // positions in cols; empty = no primary key (whole-row matching)
}

// stmt is one statement sent to STASK. post, when set, receives the command tag
// and may queue follow-up statements; such statements are flushed immediately.
type stmt struct {
	sql  string
	args [][]byte
	post func(a *Applier, tag pgconn.CommandTag) error
}

type argList struct{ args [][]byte }

func (a *argList) add(v val) string {
	if v.st == vNull {
		a.args = append(a.args, nil)
	} else {
		a.args = append(a.args, v.b)
	}
	return fmt.Sprintf("$%d", len(a.args))
}

func (r *rel) table() string { return qi(r.tbl.Name) }

// insert builds INSERT for a full new tuple. With upsert and a primary key it
// becomes INSERT .. ON CONFLICT (pk) DO UPDATE (repair mode).
func (r *rel) insert(v []val, upsert bool) (stmt, error) {
	if len(v) != len(r.cols) {
		return stmt{}, fatalf("table %s: tuple has %d columns, relation has %d", r.tbl.Name, len(v), len(r.cols))
	}
	var al argList
	var cols, ph []string
	for i, c := range r.cols {
		if v[i].st == vUnchanged {
			continue
		}
		cols = append(cols, qi(c.Name))
		ph = append(ph, al.add(v[i]))
	}
	sql := "INSERT INTO " + r.table() + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(ph, ", ") + ")"
	if upsert && len(r.pk) > 0 {
		isPK := map[int]bool{}
		var pkn []string
		for _, p := range r.pk {
			isPK[p] = true
			pkn = append(pkn, qi(r.cols[p].Name))
		}
		var set []string
		for i, c := range r.cols {
			if !isPK[i] && v[i].st != vUnchanged {
				set = append(set, qi(c.Name)+" = EXCLUDED."+qi(c.Name))
			}
		}
		if len(set) > 0 {
			sql += " ON CONFLICT (" + strings.Join(pkn, ", ") + ") DO UPDATE SET " + strings.Join(set, ", ")
		} else {
			sql += " ON CONFLICT (" + strings.Join(pkn, ", ") + ") DO NOTHING"
		}
	}
	return stmt{sql: sql, args: al.args}, nil
}

func (r *rel) whereKey(al *argList, key []val) (string, error) {
	var w []string
	for _, p := range r.pk {
		if p >= len(key) || key[p].st != vVal {
			return "", fatalf("table %s: key column %s missing from the old tuple (is the replica identity the primary key?)", r.tbl.Name, r.cols[p].Name)
		}
		w = append(w, qi(r.cols[p].Name)+" = "+al.add(key[p]))
	}
	return strings.Join(w, " AND "), nil
}

func (r *rel) whereAll(al *argList, old []val) (string, error) {
	if len(old) != len(r.cols) {
		return "", fatalf("table %s: old tuple has %d columns, relation has %d (REPLICA IDENTITY FULL needed)", r.tbl.Name, len(old), len(r.cols))
	}
	var w []string
	for i, c := range r.cols {
		switch old[i].st {
		case vNull:
			w = append(w, qi(c.Name)+" IS NULL")
		case vUnchanged:
			return "", fatalf("table %s: old tuple column %s is an unchanged TOAST value; cannot match a row without a primary key", r.tbl.Name, c.Name)
		default:
			w = append(w, qi(c.Name)+" = "+al.add(old[i]))
		}
	}
	return strings.Join(w, " AND "), nil
}

// update builds the statements for an UPDATE message. old is nil when the key
// did not change (and the table has a primary key).
func (r *rel) update(nv, old []val) ([]stmt, error) {
	if len(nv) != len(r.cols) {
		return nil, fatalf("table %s: tuple has %d columns, relation has %d", r.tbl.Name, len(nv), len(r.cols))
	}
	if len(r.pk) == 0 {
		// No primary key: replace one copy of the old row. STASK deletes
		// every identical row, so re-insert the other n-1 copies.
		var al argList
		w, err := r.whereAll(&al, old)
		if err != nil {
			return nil, err
		}
		full := make([]val, len(nv))
		for i := range nv {
			full[i] = nv[i]
			if nv[i].st == vUnchanged {
				full[i] = old[i]
			}
		}
		ins, err := r.insert(full, false)
		if err != nil {
			return nil, err
		}
		del := stmt{sql: "DELETE FROM " + r.table() + " WHERE " + w, args: al.args, post: r.noPKPost(old, "update")}
		return []stmt{del, ins}, nil
	}
	var al argList
	var set []string
	for i, c := range r.cols {
		if nv[i].st == vUnchanged {
			continue
		}
		set = append(set, qi(c.Name)+" = "+al.add(nv[i]))
	}
	if len(set) == 0 {
		return nil, nil
	}
	key := nv
	if old != nil {
		key = old
	}
	w, err := r.whereKey(&al, key)
	if err != nil {
		return nil, err
	}
	return []stmt{{sql: "UPDATE " + r.table() + " SET " + strings.Join(set, ", ") + " WHERE " + w, args: al.args, post: expectOne("update")}}, nil
}

func (r *rel) delete(old []val) ([]stmt, error) {
	var al argList
	if len(r.pk) == 0 {
		w, err := r.whereAll(&al, old)
		if err != nil {
			return nil, err
		}
		return []stmt{{sql: "DELETE FROM " + r.table() + " WHERE " + w, args: al.args, post: r.noPKPost(old, "delete")}}, nil
	}
	w, err := r.whereKey(&al, old)
	if err != nil {
		return nil, err
	}
	return []stmt{{sql: "DELETE FROM " + r.table() + " WHERE " + w, args: al.args, post: expectOne("delete")}}, nil
}

func expectOne(what string) func(*Applier, pgconn.CommandTag) error {
	return func(a *Applier, tag pgconn.CommandTag) error {
		if n := tag.RowsAffected(); n != 1 {
			return a.anomaly(fmt.Sprintf("%s affected %d rows, expected 1 (%s)", what, n, tag))
		}
		return nil
	}
}

// noPKPost handles the result of the DELETE that stands for one row of a
// table without primary key: 0 rows is an anomaly, n>1 means identical rows
// were deleted together and n-1 copies are put back.
func (r *rel) noPKPost(old []val, what string) func(*Applier, pgconn.CommandTag) error {
	return func(a *Applier, tag pgconn.CommandTag) error {
		n := tag.RowsAffected()
		if n == 0 {
			return a.anomaly(fmt.Sprintf("table %s: %s of a row that is not in STASK", r.tbl.Name, what))
		}
		for i := int64(1); i < n; i++ {
			ins, err := r.insert(old, false)
			if err != nil {
				return err
			}
			a.queue(ins)
		}
		return nil
	}
}
