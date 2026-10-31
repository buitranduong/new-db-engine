package sql

import (
	"fmt"

	"stask/internal/core"
	"stask/internal/schema"
)

// WITH [CASCADED | LOCAL] CHECK OPTION, and the partition bound of a partition (a view in this
// engine, partition.go). Precedent: PostgreSQL docs, CREATE VIEW ("WITH CHECK OPTION"; the
// error is 44000 "new row violates check option for view"), and for a partition, section 5.11
// ("new row for relation violates partition constraint", 23514). The option is kept in a
// catalog object of kind "vchk" (no change of the view row format); the DML rewrite
// (pgres_viewdml.go) collects the view conditions and the executor tests the new row before it
// is written (insert: buildInsertRow, update: updateRow).

type viewCheck struct {
	view     string // the outermost view named by the statement
	part     bool   // a partition: 23514
	cascaded bool
	exprs    []Expr
}

// viewOption returns the check option of a view: "", "local", "cascaded" or "partition".
func (s *Session) viewOption(name string) string {
	if o, ok := s.E.Obj("vchk", name); ok {
		return o.Data
	}
	return ""
}

// noteViewCheck adds the condition of view name (its WHERE) to the checks of the running DML.
func (s *Session) noteViewCheck(name string, sv *simpleView) {
	opt := s.viewOption(name)
	vc := s.vchk
	if opt == "" && (vc == nil || !vc.cascaded) {
		return
	}
	if vc == nil {
		vc = &viewCheck{view: name}
		s.vchk = vc
	}
	if opt == "partition" {
		vc.part = true
	}
	if opt == "cascaded" || opt == "partition" {
		vc.cascaded = true
	}
	if sv.where != nil {
		vc.exprs = append(vc.exprs, sv.where)
	}
}

// check fails if the new row of the base table does not satisfy the view conditions.
func (vc *viewCheck) check(t *schema.Table, r schema.Row) error {
	ctx := &evalCtx{sc: &scope{t: t}, row: r, now: nowMicros()}
	for _, x := range vc.exprs {
		v, err := ctx.eval(x)
		if err != nil {
			return err
		}
		if b, err := coerce(v, KBool); v.IsNull() || err != nil || !b.Bool() {
			if vc.part {
				return errf("23514", "new row for relation %q violates partition constraint", vc.view)
			}
			return errf("44000", "new row violates check option for view %q", vc.view)
		}
	}
	return nil
}

// setViewOption stores (or clears) the check option of a view.
func (s *Session) setViewOption(name, opt string) error {
	if opt == "" {
		if _, ok := s.E.Obj("vchk", name); !ok {
			return nil
		}
		return AsErrNil(s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: "vchk", Name: name}, Delete: true}}))
	}
	return AsErrNil(s.E.PutObj(core.Obj{Kind: "vchk", Name: name, Data: opt}, true))
}

var _ = fmt.Sprintf
