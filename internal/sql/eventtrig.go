package sql

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"stask/internal/core"
)

// Event triggers (PostgreSQL docs "Event Triggers", "Event Trigger Behavior",
// "Event Trigger Firing Matrix", CREATE EVENT TRIGGER).
//
//   - CREATE EVENT TRIGGER name ON ddl_command_start | ddl_command_end |
//     sql_drop | table_rewrite [WHEN TAG IN (..)] EXECUTE FUNCTION f(),
//     ALTER EVENT TRIGGER .. ENABLE | DISABLE | RENAME TO, DROP EVENT TRIGGER.
//     A definition is a catalog object of kind "evtrig" (journaled for DDL in
//     a transaction like the other object DDL).
//   - The function is plpgsql RETURNS event_trigger and sees TG_EVENT and
//     TG_TAG. ddl_command_start fires before the command, ddl_command_end
//     after it, sql_drop after a DROP that dropped something and before
//     ddl_command_end. The whole thing runs in the command's transaction
//     (an autocommit command gets an implicit one), so an error in a trigger
//     or in the command undoes everything, as in PostgreSQL.
//   - Fired for the DDL commands of ddlEventTag (tables, indexes, views,
//     sequences, functions, triggers, types, domains, schemas, extensions,
//     COMMENT, GRANT/REVOKE, publications); not for TRUNCATE, shared objects
//     (roles) or event trigger commands, as in PostgreSQL. DDL run by an event
//     trigger function does not fire event triggers again.
//   - pg_event_trigger_dropped_objects() lists the objects the command named
//     that existed (not dependents: PostgreSQL also lists the row type,
//     sequences, indexes, constraints.. of a dropped table). pg_event_trigger_ddl_commands()
//     lists one row for the command. table_rewrite is accepted and never fires.

const evKind = "evtrig"

type evtrigDef struct {
	Event   string   `json:"event"`
	Tags    []string `json:"tags,omitempty"`
	Func    string   `json:"func"`
	Enabled string   `json:"enabled,omitempty"` // "" or O: enabled; D: disabled
}

type evtrig struct {
	Name string
	evtrigDef
}

// evState is what an event trigger function sees while it runs.
type evState struct {
	event, tag string
	dropped    [][]Datum
	cmds       [][]Datum
}

var evEvents = map[string]bool{"ddl_command_start": true, "ddl_command_end": true, "sql_drop": true, "table_rewrite": true}

// evTags are the command tags a WHEN TAG filter accepts.
var evTags = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range strings.Split("CREATE TABLE,ALTER TABLE,DROP TABLE,CREATE TABLE AS,SELECT INTO,CREATE INDEX,ALTER INDEX,DROP INDEX,"+
		"CREATE VIEW,DROP VIEW,CREATE MATERIALIZED VIEW,DROP MATERIALIZED VIEW,REFRESH MATERIALIZED VIEW,CREATE SEQUENCE,ALTER SEQUENCE,DROP SEQUENCE,"+
		"CREATE FUNCTION,DROP FUNCTION,CREATE PROCEDURE,DROP PROCEDURE,CREATE TRIGGER,DROP TRIGGER,CREATE TYPE,ALTER TYPE,DROP TYPE,"+
		"CREATE DOMAIN,ALTER DOMAIN,DROP DOMAIN,CREATE SCHEMA,ALTER SCHEMA,DROP SCHEMA,CREATE EXTENSION,ALTER EXTENSION,DROP EXTENSION,"+
		"CREATE COLLATION,ALTER COLLATION,DROP COLLATION,COMMENT,GRANT,REVOKE,CREATE PUBLICATION,DROP PUBLICATION,ALTER PUBLICATION", ",") {
		m[t] = true
	}
	return m
}()

// ddlEventTag is the command tag event triggers see for a statement; ok is
// false for statements that do not fire them.
func ddlEventTag(st Stmt) (string, bool) {
	switch st := st.(type) {
	case *CreateTable, *CreatePartition:
		return "CREATE TABLE", true
	case *AlterTable, *PartAlter, *AlterColRestart, *AlterReplicaIdentity:
		return "ALTER TABLE", true
	case *DropTable:
		if st.Mat {
			return "DROP MATERIALIZED VIEW", true
		}
		return "DROP TABLE", true
	case *CreateAs:
		if st.Mat {
			return "CREATE MATERIALIZED VIEW", true
		}
		if st.FromInto {
			return "SELECT INTO", true
		}
		return "CREATE TABLE AS", true
	case *Select:
		if st.Into != nil {
			return "SELECT INTO", true
		}
	case *RefreshMat:
		return "REFRESH MATERIALIZED VIEW", true
	case *DropMat:
		return "DROP MATERIALIZED VIEW", true
	case *CreateIndex, *CreateSubstrIndex:
		return "CREATE INDEX", true
	case *DropIndex, *DropSubstrIndex:
		return "DROP INDEX", true
	case *AlterIndex, *RenameIndex:
		return "ALTER INDEX", true
	case *CreateView:
		return "CREATE VIEW", true
	case *DropView:
		return "DROP VIEW", true
	case *CreateSequence:
		return "CREATE SEQUENCE", true
	case *AlterSequence:
		return "ALTER SEQUENCE", true
	case *DropSequence:
		return "DROP SEQUENCE", true
	case *CreateFunc:
		if st.Def.Procedure {
			return "CREATE PROCEDURE", true
		}
		return "CREATE FUNCTION", true
	case *DropFunc:
		if st.Procedure {
			return "DROP PROCEDURE", true
		}
		return "DROP FUNCTION", true
	case *CreateTrigger:
		return "CREATE TRIGGER", true
	case *DropTrigger:
		return "DROP TRIGGER", true
	case *CreatePublication:
		return "CREATE PUBLICATION", true
	case *DropPublication:
		return "DROP PUBLICATION", true
	case *CommentOn:
		return "COMMENT", true
	case *Grant:
		if st.Noop {
			return "", false
		}
		if st.Revoke {
			return "REVOKE", true
		}
		return "GRANT", true
	case *ObjDDL:
		if st.Class == "event trigger" {
			return "", false
		}
		return tagOf(st), true
	}
	return "", false
}

// eventTriggers lists the definitions, by name.
func (s *Session) eventTriggers() []evtrig {
	objs := s.E.Objs(evKind)
	if len(objs) == 0 {
		return nil
	}
	out := make([]evtrig, 0, len(objs))
	for _, o := range objs {
		var d evtrigDef
		if json.Unmarshal([]byte(o.Data), &d) == nil {
			out = append(out, evtrig{Name: o.Name, evtrigDef: d})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (t evtrig) matches(event, tag string) bool {
	if t.Event != event || t.Enabled == "D" {
		return false
	}
	if len(t.Tags) == 0 {
		return true
	}
	for _, x := range t.Tags {
		if x == tag {
			return true
		}
	}
	return false
}

// execWithEvents runs a DDL statement between its event triggers.
func (s *Session) execWithEvents(st Stmt, params []Datum, tag string, trigs []evtrig) (*Result, error) {
	var start, end, drop []evtrig
	for _, t := range trigs {
		switch {
		case t.matches("ddl_command_start", tag):
			start = append(start, t)
		case t.matches("ddl_command_end", tag):
			end = append(end, t)
		case t.matches("sql_drop", tag):
			drop = append(drop, t)
		}
	}
	if len(start)+len(end)+len(drop) == 0 {
		return s.execInner(st, params)
	}
	if err := s.beginFnTx(); err != nil {
		return nil, err
	}
	fail := func(err error) (*Result, error) {
		if s.explicit {
			s.markFailed()
		}
		return nil, err
	}
	s.evBusy = true
	defer func() { s.evBusy, s.ev = false, nil }()
	for _, t := range start {
		if err := s.runEventFunc(t, "ddl_command_start", tag, nil, nil); err != nil {
			return fail(err)
		}
	}
	var dropped [][]Datum
	if len(drop) > 0 {
		dropped = s.droppedObjects(st)
	}
	res, err := s.execInner(st, params)
	if err != nil {
		return nil, err
	}
	if len(dropped) > 0 {
		for _, t := range drop {
			if err := s.runEventFunc(t, "sql_drop", tag, dropped, nil); err != nil {
				return fail(err)
			}
		}
	}
	if len(end) > 0 {
		cmds := s.commandRows(st, tag)
		for _, t := range end {
			if err := s.runEventFunc(t, "ddl_command_end", tag, nil, cmds); err != nil {
				return fail(err)
			}
		}
	}
	return res, nil
}

// runEventFunc calls the function of trigger t with TG_EVENT and TG_TAG.
func (s *Session) runEventFunc(t evtrig, event, tag string, dropped, cmds [][]Datum) error {
	d, ok := s.loadFunc(t.Func)
	if !ok {
		return errf(codeUndefinedFunction, "function %s() does not exist", t.Func)
	}
	if d.Ret != "event_trigger" {
		return errf("42P17", "function %s must return type event_trigger", t.Func)
	}
	blk, err := cachedPlParse(d.Body, nil)
	if err != nil {
		return err
	}
	if s.plDepth >= maxPLDepth {
		return errf("54001", "stack depth limit exceeded")
	}
	s.plDepth++
	defer func() { s.plDepth-- }()
	prev := s.ev
	s.ev = &evState{event: event, tag: tag, dropped: dropped, cmds: cmds}
	defer func() { s.ev = prev }()
	r := &plRun{s: s, vars: map[string]Datum{"tg_event": TextD(event), "tg_tag": TextD(tag)}, fn: d}
	_, err = r.block(blk, nil)
	return err
}

// ---- objects named by a command ----

func schemaName(n string) (string, string) {
	if sch, bare, ok := splitSchema(n); ok {
		return sch, bare
	}
	return "public", n
}

func objRow(typ, name string) []Datum {
	sch, bare := schemaName(name)
	ident := sch + "." + bare
	if typ == "schema" {
		sch, ident = "", bare
	}
	var schD Datum = Null
	if sch != "" {
		schD = TextD(sch)
	}
	return []Datum{IntD(0), IntD(0), IntD(0), BoolD(true), BoolD(false), BoolD(false), TextD(typ), schD, TextD(bare), TextD(ident), Null, Null}
}

// droppedObjects lists, before a DROP runs, the objects it will drop.
func (s *Session) droppedObjects(st Stmt) [][]Datum {
	var rows [][]Datum
	switch st := st.(type) {
	case *DropTable:
		for _, n := range st.Names {
			if _, err := s.E.UserTable(s.resolveRel(n)); err == nil {
				typ := "table"
				if st.Mat {
					typ = "materialized view"
				}
				rows = append(rows, objRow(typ, s.resolveRel(n)))
			}
		}
	case *DropView:
		for _, n := range st.Names {
			if _, ok, err := s.E.View(nil, n); err == nil && ok {
				rows = append(rows, objRow("view", s.resolveRel(n)))
			}
		}
	case *DropSequence:
		for _, n := range st.Names {
			if s.relationExists(s.resolveRel(n)) {
				rows = append(rows, objRow("sequence", s.resolveRel(n)))
			}
		}
	case *DropIndex:
		for _, n := range st.Names {
			if _, taken := s.E.IndexNameTaken(n); taken {
				rows = append(rows, objRow("index", n))
			}
		}
	case *DropFunc:
		if len(s.funcsNamed(st.Name)) > 0 {
			rows = append(rows, objRow("function", st.Name))
		}
	case *ObjDDL:
		if st.Op != "drop" {
			break
		}
		for _, n := range st.Names {
			switch st.Class {
			case "type", "domain":
				if ut, err := s.findType(n); err == nil {
					typ := "type"
					if ut.Kind == "domain" && !ut.isComposite() {
						typ = "domain"
					}
					rows = append(rows, objRow(typ, ut.Name))
				}
			case "schema":
				if _, ok := s.E.Obj("schema", n); ok {
					rows = append(rows, objRow("schema", n))
				}
			}
		}
	}
	return rows
}

// commandRows is pg_event_trigger_ddl_commands() for the command that ran:
// one row naming its main object.
func (s *Session) commandRows(st Stmt, tag string) [][]Datum {
	typ, name := "", ""
	switch st := st.(type) {
	case *CreateTable:
		typ, name = "table", st.Name
	case *CreateAs:
		typ, name = "table", st.Name
		if st.Mat {
			typ = "materialized view"
		}
	case *AlterTable:
		typ, name = "table", st.Name
	case *CreateView:
		typ, name = "view", st.Name
	case *CreateSequence:
		typ, name = "sequence", st.Name
	case *CreateIndex:
		typ, name = "index", st.Name
	case *CreateFunc:
		typ, name = "function", st.Def.Name
	case *ObjDDL:
		if len(st.Names) > 0 && st.Op != "drop" {
			typ, name = st.Class, st.Names[0]
		}
	}
	if name == "" {
		return nil
	}
	o := objRow(typ, name)
	return [][]Datum{{IntD(0), IntD(0), IntD(0), TextD(tag), o[6], o[7], o[9], BoolD(false), Null}}
}

func init() {
	for _, n := range []string{"pg_event_trigger_dropped_objects", "pg_event_trigger_ddl_commands"} {
		sessionFuncs[n] = true
	}
	tableFuncCols["pg_event_trigger_dropped_objects"] = []relCol{{name: "classid", kind: KInt}, {name: "objid", kind: KInt}, {name: "objsubid", kind: KInt},
		{name: "original", kind: KBool}, {name: "normal", kind: KBool}, {name: "is_temporary", kind: KBool}, {name: "object_type", kind: KText},
		{name: "schema_name", kind: KText}, {name: "object_name", kind: KText}, {name: "object_identity", kind: KText},
		{name: "address_names", kind: KText}, {name: "address_args", kind: KText}}
	tableFuncCols["pg_event_trigger_ddl_commands"] = []relCol{{name: "classid", kind: KInt}, {name: "objid", kind: KInt}, {name: "objsubid", kind: KInt},
		{name: "command_tag", kind: KText}, {name: "object_type", kind: KText}, {name: "schema_name", kind: KText}, {name: "object_identity", kind: KText},
		{name: "in_extension", kind: KBool}, {name: "command", kind: KText}}
	sessionFuncs["pg_event_trigger"] = true
	virtualTables["pg_event_trigger"] = true
	tableFuncCols["pg_event_trigger"] = []relCol{{name: "oid", kind: KInt}, {name: "evtname", kind: KText, ty: pgTy{oid: tyName, mod: -1}}, {name: "evtevent", kind: KText, ty: pgTy{oid: tyName, mod: -1}},
		{name: "evtowner", kind: KInt}, {name: "evtfoid", kind: KInt}, {name: "evtenabled", kind: KText, ty: pgTy{oid: 18, mod: -1}}, {name: "evttags", kind: KText}}
}

// eventFunc serves the event trigger table functions and the pg_event_trigger view.
func (s *Session) eventFunc(name string) ([][]Datum, bool, error) {
	switch name {
	case "pg_event_trigger_dropped_objects":
		if s.ev == nil || s.ev.event != "sql_drop" {
			return nil, true, errf("39P03", "%s() can only be called in a sql_drop event trigger function", name)
		}
		return s.ev.dropped, true, nil
	case "pg_event_trigger_ddl_commands":
		if s.ev == nil || s.ev.event != "ddl_command_end" {
			return nil, true, errf("39P03", "%s() can only be called in a ddl_command_end event trigger function", name)
		}
		return s.ev.cmds, true, nil
	case "pg_event_trigger":
		var rows [][]Datum
		for i, t := range s.eventTriggers() {
			en := "O"
			if t.Enabled == "D" {
				en = "D"
			}
			var tags Datum = Null
			if len(t.Tags) > 0 {
				tags = Datum{K: KArray, S: "{" + strings.Join(t.Tags, ",") + "}", I: int64(KText)}
			}
			rows = append(rows, []Datum{IntD(int64(40000 + i)), TextD(t.Name), TextD(t.Event), IntD(10), IntD(0), TextD(en), tags})
		}
		return rows, true, nil
	}
	return nil, false, nil
}

// ---- parsing ----

func (p *parser) createEventTrigger() (Stmt, error) {
	c := &ObjDDL{Op: "create", Class: "event trigger"}
	n, err := p.ident()
	if err != nil {
		return nil, err
	}
	c.Names = []string{n}
	if err := p.expectKw("on"); err != nil {
		return nil, err
	}
	ev, err := p.ident()
	if err != nil {
		return nil, err
	}
	ev = strings.ToLower(ev)
	if !evEvents[ev] {
		return nil, errf(codeSyntax, "unrecognized event name %q", ev)
	}
	c.Ev = &evtrigDef{Event: ev}
	if p.acceptKw("when") {
		for {
			v, err := p.ident()
			if err != nil {
				return nil, err
			}
			if !strings.EqualFold(v, "tag") {
				return nil, errf(codeSyntax, "unrecognized filter variable %q", v)
			}
			if err := p.expectKw("in"); err != nil {
				return nil, err
			}
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			for {
				t, err := p.strLit()
				if err != nil {
					return nil, err
				}
				t = strings.ToUpper(t)
				if !evTags[t] {
					return nil, errf(codeSyntax, "filter value %q not recognized for filter variable \"tag\"", strings.ToUpper(t))
				}
				c.Ev.Tags = append(c.Ev.Tags, t)
				if p.acceptOp(")") {
					break
				}
				if err := p.expectOp(","); err != nil {
					return nil, err
				}
			}
			if !p.acceptKw("and") {
				break
			}
		}
	}
	if err := p.expectKw("execute"); err != nil {
		return nil, err
	}
	if !p.acceptKw("function") && !p.acceptKw("procedure") {
		return nil, p.errHere("syntax error at or near %q", p.peek().s)
	}
	names, err := p.dottedName()
	if err != nil {
		return nil, err
	}
	c.Ev.Func = names[len(names)-1]
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	return c, p.expectOp(")")
}

func (p *parser) alterEventTrigger() (Stmt, error) {
	a := &ObjDDL{Op: "alter", Class: "event trigger"}
	n, err := p.ident()
	if err != nil {
		return nil, err
	}
	a.Names = []string{n}
	switch {
	case p.acceptKw("disable"):
		a.Action = "disable"
	case p.acceptKw("enable"):
		a.Action = "enable"
		p.acceptKw("replica")
		p.acceptKw("always")
	case p.acceptKw("rename", "to"):
		a.Action = "rename"
		if a.NewName, err = p.ident(); err != nil {
			return nil, err
		}
	case p.acceptKw("owner", "to"):
		a.Action = "owner"
		if _, err = p.ident(); err != nil {
			return nil, err
		}
	default:
		return nil, p.errHere("syntax error at or near %q", p.peek().s)
	}
	return a, nil
}

// ---- execution ----

func (s *Session) eventTriggerDDL(st *ObjDDL) (*Result, error) {
	name := st.Names[0]
	cur, exists := s.E.Obj(evKind, name)
	switch st.Op {
	case "create":
		if exists {
			return nil, errf(codeDuplicateObject, "event trigger %q already exists", name)
		}
		d, ok := s.loadFunc(st.Ev.Func)
		if !ok {
			return nil, errf(codeUndefinedFunction, "function %s() does not exist", st.Ev.Func)
		}
		if d.Ret != "event_trigger" {
			return nil, errf("42P17", "function %s must return type event_trigger", st.Ev.Func)
		}
		b, _ := json.Marshal(st.Ev)
		if err := s.E.PutObj(core.Obj{Kind: evKind, Name: name, Data: string(b)}, false); err != nil {
			return nil, AsError(err)
		}
		return &Result{Tag: "CREATE EVENT TRIGGER"}, nil
	case "drop":
		if !exists {
			if st.IfExists {
				return &Result{Tag: "DROP EVENT TRIGGER", Notice: fmt.Sprintf("event trigger %q does not exist, skipping", name)}, nil
			}
			return nil, errf(codeUndefinedObject, "event trigger %q does not exist", name)
		}
		if err := s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: evKind, Name: name}, Delete: true}}); err != nil {
			return nil, AsError(err)
		}
		return &Result{Tag: "DROP EVENT TRIGGER"}, nil
	}
	if !exists {
		return nil, errf(codeUndefinedObject, "event trigger %q does not exist", name)
	}
	var d evtrigDef
	_ = json.Unmarshal([]byte(cur.Data), &d)
	switch st.Action {
	case "disable":
		d.Enabled = "D"
	case "enable":
		d.Enabled = ""
	case "owner":
		return &Result{Tag: "ALTER EVENT TRIGGER"}, nil
	case "rename":
		if _, dup := s.E.Obj(evKind, st.NewName); dup {
			return nil, errf(codeDuplicateObject, "event trigger %q already exists", st.NewName)
		}
		b, _ := json.Marshal(d)
		err := s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: evKind, Name: st.NewName, Data: string(b)}},
			{O: core.Obj{Kind: evKind, Name: name}, Delete: true}})
		if err != nil {
			return nil, AsError(err)
		}
		return &Result{Tag: "ALTER EVENT TRIGGER"}, nil
	}
	b, _ := json.Marshal(d)
	if err := s.E.PutObj(core.Obj{Kind: evKind, Name: name, Data: string(b)}, true); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "ALTER EVENT TRIGGER"}, nil
}
