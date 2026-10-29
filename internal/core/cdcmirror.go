package core

import (
	"encoding/json"
	"strings"

	"stask/internal/schema"
)

// Failover state for CDC (P6.3, docs/10 section 7 and docs/06 section 5).
//
// Slots, publications and replica identity used to live in files of the
// leader (<dir>/slots, cdc.json), so a replica promoted after the leader
// died had none of them. They now also go through the log, as rows of a
// hidden system table (like sequences), so every replica has them byte for
// byte:
//
//	"catalog"      the publications and identities (cdcCatalog as JSON)
//	"slot:<name>"  a slot's SlotInfo as JSON; deleted with the slot
//
// The log is the same on all nodes, byte for byte (docs/06 section 2), so a
// slot's (segment, offset) means the same on a promoted replica.
//
// A slot's confirmed position is mirrored at most once a second, and not at
// all when the only log it passed is the mirror's own writes. After a
// failover a slot can therefore be up to about a second behind what the
// subscriber confirmed. The subscriber starts streaming at its own position
// (PostgreSQL's START_REPLICATION gives it, and a subscriber keeps its
// origin progress), which the walsender honors, so nothing is lost and
// nothing is applied twice; only the server-side position was stale.
//
// On a replica the files are not written: at promotion (a restart without
// -replica-of) Open rebuilds the catalog and the slots from the table. A
// file on disk wins over the table row only if it is not behind it.

const cdcTableName = "__stask_cdc"

func cdcSchema() schema.Table {
	return schema.Table{Name: cdcTableName, Key: "key", System: true, Columns: []schema.Column{
		{Name: "key", Type: "string"}, {Name: "val", Type: "string"}}}
}

func (e *Engine) cdcTable() error {
	if e.tableExists(cdcTableName) {
		return nil
	}
	_, err := e.CreateTable(cdcSchema())
	if err != nil && e.tableExists(cdcTableName) {
		return nil // created concurrently
	}
	return err
}

// cdcMirror writes (val != nil) or deletes key in the CDC table, through
// the log, and returns the LSN of the write.
func (e *Engine) cdcMirror(key string, val []byte) error {
	if err := e.cdcTable(); err != nil {
		return err
	}
	op := WriteOp{Table: cdcTableName, Key: key}
	if val != nil {
		op.Row = schema.Row{{S: key}, {S: string(val)}}
	}
	lsn, err := e.Write([]WriteOp{op})
	if err != nil {
		return err
	}
	for {
		old := e.cdcMirrorLSN.Load()
		if lsn <= old || e.cdcMirrorLSN.CompareAndSwap(old, lsn) {
			break
		}
	}
	return nil
}

// cdcRows returns the CDC table's rows by key (nil when there is none).
func (e *Engine) cdcRows() map[string]string {
	if !e.tableExists(cdcTableName) {
		return nil
	}
	rows := map[string]string{}
	e.ScanSnapshot(cdcTableName, e.durable.Load(), func(k string, r schema.Row, _ *Version) bool {
		rows[k] = r[1].S
		return true
	})
	return rows
}

// slotsFromTable adopts the slots in the CDC table that this node has no
// file for, or whose file is behind (loadSlots, before it registers them).
func (e *Engine) slotsFromTable(have map[string]SlotInfo) {
	for k, v := range e.cdcRows() {
		name, ok := strings.CutPrefix(k, "slot:")
		if !ok {
			continue
		}
		var info SlotInfo
		if json.Unmarshal([]byte(v), &info) != nil || info.Name != name {
			continue
		}
		if cur, ok := have[name]; ok && !cur.Pos().Less(info.Pos()) {
			continue
		}
		have[name] = info
	}
}
