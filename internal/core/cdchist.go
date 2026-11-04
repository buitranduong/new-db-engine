package core

import (
	"encoding/json"
	"slices"
	"sort"

	"stask/internal/schema"
	"stask/internal/wal"
)

// The CDC catalog as of a log position (docs/26 G3, docs/28 A0).
//
// A slot decodes a change with the catalog the log had when the change was
// written, not the one of decode time, as PostgreSQL's logical decoding
// does with its historic catalog snapshot
// (https://www.postgresql.org/docs/16/logicaldecoding-explanation.html):
// a consumer that lags across ALTER TABLE ... RENAME or ALTER PUBLICATION
// gets each change under the table name and the publications of its time,
// so changes written while a table was published are delivered even if the
// table left the publication since.
//
// Tables are tracked by relation OID (kept by RENAME and TRUNCATE), so a
// publication keeps its tables when they are renamed, and the catalog row
// written after a RENAME (publications list tables by name, cdccat.go)
// maps to the same tables. The decoder follows the log: DDL records
// (create, alter, drop) and writes of the "catalog" row of the CDC system
// table. A slot stores the catalog of its position (SlotInfo.Catalog), so
// decoding resumes from it; a slot saved before this has none and starts
// from the catalog of the time it is next decoded.

// HistCatalog is the CDC catalog as of a slot's position.
type HistCatalog struct {
	// Names: relation OID -> table name, for the live user tables
	Names map[uint32]string `json:"names"`
	Pubs  []HistPub         `json:"pubs,omitempty"`
	// IdentityFull: OIDs of the tables with REPLICA IDENTITY FULL, sorted
	IdentityFull []uint32 `json:"identity_full,omitempty"`

	view []Publication // Pubs with table names, built on demand
}

// HistPub is a publication whose tables are relation OIDs.
type HistPub struct {
	Publication
	OIDs []uint32 `json:"oids,omitempty"`
}

// histNow is the current catalog. The caller holds e.slotMu, which
// serializes the changes of the publications (updateCDCCatalog).
func (e *Engine) histNow() *HistCatalog {
	h := &HistCatalog{Names: map[uint32]string{}}
	for _, t := range e.UserTables() {
		s := t.Schema()
		h.Names[s.OID()] = s.Name
	}
	h.setCatalog(e.cdcCat.Load())
	return h
}

func (h *HistCatalog) clone() *HistCatalog {
	c := &HistCatalog{Names: make(map[uint32]string, len(h.Names)), Pubs: slices.Clone(h.Pubs),
		IdentityFull: slices.Clone(h.IdentityFull)}
	for k, v := range h.Names {
		c.Names[k] = v
	}
	return c
}

// setCatalog replaces the publications and identities with those of c,
// its table names resolved with the names of h.
func (h *HistCatalog) setCatalog(c *cdcCatalog) {
	h.Pubs, h.IdentityFull, h.view = nil, nil, nil
	if c == nil {
		return
	}
	byName := make(map[string]uint32, len(h.Names))
	for oid, n := range h.Names {
		byName[n] = oid
	}
	oids := func(names []string) []uint32 {
		var out []uint32
		for _, n := range names {
			if oid, ok := byName[n]; ok {
				out = append(out, oid)
			}
		}
		slices.Sort(out)
		return out
	}
	for _, p := range c.Publications {
		hp := HistPub{Publication: p, OIDs: oids(p.Tables)}
		hp.Tables = nil
		h.Pubs = append(h.Pubs, hp)
	}
	h.IdentityFull = oids(c.IdentityFull)
}

// applyDDL follows a DDL record: a table created, altered or renamed gets
// its name; a dropped one leaves (all its changes precede the drop). A
// TRUNCATE is the record of the new table (same OID, Replaces set): the
// old one leaves with no record of its own, so nothing is removed.
func (h *HistCatalog) applyDDL(t *schema.Table) {
	if t.System {
		return
	}
	h.view = nil
	if t.DroppedLSN != 0 {
		delete(h.Names, t.OID())
		return
	}
	h.Names[t.OID()] = t.Name
}

// histApply follows one record of the log; it reports whether the
// catalog changed.
func (e *Engine) histApply(h *HistCatalog, r *wal.Record) bool {
	changed := false
	switch r.Kind {
	case wal.KindDDL:
		var t schema.Table
		if json.Unmarshal(r.DDL, &t) == nil && !t.System {
			h.applyDDL(&t)
			changed = true
		}
	case wal.KindTx, wal.KindTxPart:
		for _, op := range r.Ops {
			if op.Kind == wal.OpDel || string(op.Key) != "catalog" {
				continue
			}
			t := e.TableByID(op.Table)
			if t == nil || t.Schema().Name != cdcTableName {
				continue
			}
			row, err := t.Schema().DecodeAt(op.Val, r.LSN)
			if err != nil || len(row) < 2 {
				continue
			}
			var c cdcCatalog
			if json.Unmarshal([]byte(row[1].S), &c) == nil {
				h.setCatalog(&c)
				changed = true
			}
		}
	}
	return changed
}

// publications are the publications with the table names of h.
func (h *HistCatalog) publications() []Publication {
	if h.view != nil {
		return h.view
	}
	view := make([]Publication, 0, len(h.Pubs))
	for _, hp := range h.Pubs {
		p := hp.Publication
		p.Tables = nil
		for _, oid := range hp.OIDs {
			if n, ok := h.Names[oid]; ok {
				p.Tables = append(p.Tables, n)
			}
		}
		sort.Strings(p.Tables)
		view = append(view, p)
	}
	h.view = view
	return view
}

// identityFull reports REPLICA IDENTITY FULL of a table (one without a
// visible one-column key always is, as ReplicaIdentityFull says).
func (h *HistCatalog) identityFull(s *schema.Table) bool {
	if !s.HasPK() || s.Composite() {
		return true
	}
	_, ok := slices.BinarySearch(h.IdentityFull, s.OID())
	return ok
}

// named returns s under its name in h: s itself when the name is the same,
// else a copy, cached per table so that a consumer comparing tables by
// pointer (pgoutput's Relation messages) sees each change of name once.
func (h *HistCatalog) named(s *schema.Table, cache map[*schema.Table]*schema.Table) *schema.Table {
	n, ok := h.Names[s.OID()]
	if !ok || n == s.Name {
		return s
	}
	if c := cache[s]; c != nil && c.Name == n {
		return c
	}
	c := *s
	c.Name = n
	cache[s] = &c
	return &c
}
