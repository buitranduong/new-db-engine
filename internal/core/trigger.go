package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"stask/internal/schema"
)

// SQL row triggers (CREATE TRIGGER). A trigger is one row of a hidden
// system table, stored exactly as a view is (sqlview.go): durable,
// checkpointed, replayed, shipped to replicas (which never run it: they
// apply the rows the trigger wrote on the leader, not the statements) and
// hidden from clients and CDC.
//
// Running a trigger is the SQL layer's job (internal/sql/trigger.go). The
// engine only keeps the definitions, and a cache of them by table so that
// a write to a table without triggers costs one atomic load and one lookup
// in an empty map.

const triggerTableName = "__stask_triggers"

var (
	ErrNoTrigger     = errors.New("trigger does not exist")
	ErrTriggerExists = errors.New("trigger already exists")
)

// Trigger is the stored definition of a row trigger.
type Trigger struct {
	Name   string
	Table  string
	Timing string   // "before" or "after"
	Events []string // "insert", "update", "delete"
	When   string   // SQL text of the WHEN condition, "" if none
	Body   string   // SQL text of the statements between BEGIN and END

	// Function triggers (CREATE TRIGGER .. EXECUTE FUNCTION f(args), PL/pgSQL
	// trigger functions). Func is "" for a trigger with an SQL body. These
	// fields, UpdateOf and Level are kept in the body column behind a
	// metadata prefix (trigMeta), so that the hidden table keeps its
	// columns and old definitions read as before.
	Func     string
	Args     []string
	Level    string   // "row" (also when empty) or "statement"
	UpdateOf []string // UPDATE OF col, ...: fire only if the statement assigns one of them

	// Prog is the body compiled by the SQL layer. It lives as long as this
	// cached definition: a replaced or dropped trigger gets a new one.
	Prog atomic.Value
}

// Fires tells whether the trigger runs for the event.
func (t *Trigger) Fires(timing, event string) bool {
	if t.Timing != timing {
		return false
	}
	for _, e := range t.Events {
		if e == event {
			return true
		}
	}
	return false
}

// Statement tells whether the trigger fires once per statement.
func (t *Trigger) Statement() bool { return t.Level == "statement" }

// TriggerEvents renders the events for messages: "INSERT OR UPDATE".
func (t *Trigger) TriggerEvents() string {
	return strings.ToUpper(strings.Join(t.Events, " OR "))
}

func triggerKey(table, name string) string { return table + "\x1f" + name }

func triggerSchema() schema.Table {
	return schema.Table{Name: triggerTableName, Key: "key", System: true, Columns: []schema.Column{
		{Name: "key", Type: "string"}, {Name: "tbl", Type: "text"}, {Name: "name", Type: "text"},
		{Name: "timing", Type: "text"}, {Name: "events", Type: "text"},
		{Name: "cond", Type: "text"}, {Name: "body", Type: "text"}}}
}

// trigMeta is the prefix of a body that carries the extra fields of Trigger:
// "\x00meta:" + JSON + "\x00" + body.
const trigMeta = "\x00meta:"

type trigMetaJSON struct {
	Func     string   `json:"f,omitempty"`
	Args     []string `json:"a,omitempty"`
	Level    string   `json:"l,omitempty"`
	UpdateOf []string `json:"o,omitempty"`
}

func triggerRow(t *Trigger) schema.Row {
	ev, _ := json.Marshal(t.Events)
	body := t.Body
	if t.Func != "" || t.Level == "statement" || len(t.UpdateOf) > 0 {
		m, _ := json.Marshal(trigMetaJSON{t.Func, t.Args, t.Level, t.UpdateOf})
		body = trigMeta + string(m) + "\x00" + body
	}
	return schema.Row{{S: triggerKey(t.Table, t.Name)}, {S: t.Table}, {S: t.Name}, {S: t.Timing},
		{S: string(ev)}, {S: t.When}, {S: body}}
}

func triggerOfRow(r schema.Row) *Trigger {
	t := &Trigger{Table: r[1].S, Name: r[2].S, Timing: r[3].S, When: r[5].S, Body: r[6].S}
	_ = json.Unmarshal([]byte(r[4].S), &t.Events)
	if rest, ok := strings.CutPrefix(t.Body, trigMeta); ok {
		if js, body, ok := strings.Cut(rest, "\x00"); ok {
			var m trigMetaJSON
			if json.Unmarshal([]byte(js), &m) == nil {
				t.Func, t.Args, t.Level, t.UpdateOf = m.Func, m.Args, m.Level, m.UpdateOf
				t.Body = body
			}
		}
	}
	return t
}

// trigCache maps a table to its triggers in firing order (by name, as in
// PostgreSQL). It is dropped by every change made here and rebuilt by the
// next reader; gen stops a rebuild that raced with a change from
// publishing what it read before the change.
type trigCache struct {
	mu  sync.Mutex
	gen uint64
	m   atomic.Pointer[map[string][]*Trigger]
}

func (e *Engine) triggersTable() error {
	if e.tableExists(triggerTableName) {
		return nil
	}
	return e.createSystemTable(triggerSchema())
}

func (e *Engine) triggersChanged() {
	e.trig.mu.Lock()
	e.trig.gen++
	e.trig.m.Store(nil)
	e.trig.mu.Unlock()
}

// PutTrigger stores a trigger; with replace an existing one is replaced.
func (e *Engine) PutTrigger(t *Trigger, replace bool) error {
	if err := e.triggersTable(); err != nil {
		return err
	}
	_, err := e.Write([]WriteOp{{Table: triggerTableName, Row: triggerRow(t), IfAbsent: !replace}})
	e.triggersChanged()
	if errors.As(err, new(*KeyExistsError)) {
		return fmt.Errorf("%w: %s", ErrTriggerExists, t.Name)
	}
	return err
}

// DropTrigger drops one trigger of a table.
func (e *Engine) DropTrigger(table, name string) error {
	if _, ok := e.Trigger(table, name); !ok {
		return fmt.Errorf("%w: %s", ErrNoTrigger, name)
	}
	_, err := e.Write([]WriteOp{{Table: triggerTableName, Key: triggerKey(table, name)}})
	e.triggersChanged()
	return err
}

// DropTableTriggers drops all the triggers of a table (DROP TABLE).
func (e *Engine) DropTableTriggers(table string) error {
	tgs := e.Triggers(table)
	if len(tgs) == 0 {
		return nil
	}
	ops := make([]WriteOp, len(tgs))
	for i, t := range tgs {
		ops[i] = WriteOp{Table: triggerTableName, Key: triggerKey(t.Table, t.Name)}
	}
	_, err := e.Write(ops)
	e.triggersChanged()
	return err
}

func (e *Engine) loadTriggers() *map[string][]*Trigger {
	e.trig.mu.Lock()
	gen := e.trig.gen
	e.trig.mu.Unlock()
	m := map[string][]*Trigger{}
	if e.tableExists(triggerTableName) {
		e.ScanSnapshot(triggerTableName, e.DurableLSN(), func(_ string, r schema.Row, _ *Version) bool {
			t := triggerOfRow(r)
			m[t.Table] = append(m[t.Table], t)
			return true
		})
		for _, l := range m {
			sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
		}
	}
	e.trig.mu.Lock()
	if e.trig.gen == gen && !e.ro.Load() { // a replica reads the writes of its leader
		e.trig.m.Store(&m)
	}
	e.trig.mu.Unlock()
	return &m
}

// Triggers returns the triggers of a table in firing order, nil if it has
// none. This is on the path of every write: without triggers it is an
// atomic load and a lookup in an empty map.
func (e *Engine) Triggers(table string) []*Trigger {
	m := e.trig.m.Load()
	if m == nil {
		m = e.loadTriggers()
	}
	return (*m)[table]
}

// Trigger returns one trigger of a table.
func (e *Engine) Trigger(table, name string) (*Trigger, bool) {
	for _, t := range e.Triggers(table) {
		if t.Name == name {
			return t, true
		}
	}
	return nil, false
}

// AllTriggers lists every trigger by table, then name.
func (e *Engine) AllTriggers() []*Trigger {
	m := e.trig.m.Load()
	if m == nil {
		m = e.loadTriggers()
	}
	var out []*Trigger
	for _, l := range *m {
		out = append(out, l...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Name < out[j].Name
	})
	return out
}
