package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

// The CDC catalog: publications (which tables and operations a pgoutput
// consumer receives, as PostgreSQL's CREATE PUBLICATION) and each table's
// replica identity (what an UPDATE or DELETE carries of the old row, as
// ALTER TABLE ... REPLICA IDENTITY).
//
// In PostgreSQL the replica identity decides what the WAL records at write
// time. Here a slot keeps the snapshot it decodes from, so the old version
// of a row is still in the log at decode time: the identity is read only
// while decoding and costs nothing on the write path.
//
// Like slots, the catalog is leader-local, in <dir>/cdc.json, written
// atomically.

// Publication is a named set of tables and operations.
type Publication struct {
	Name      string   `json:"name"`
	AllTables bool     `json:"all_tables,omitempty"`
	Tables    []string `json:"tables,omitempty"` // when not AllTables, sorted
	Insert    bool     `json:"insert"`
	Update    bool     `json:"update"`
	Delete    bool     `json:"delete"`
	// NoTruncate: TRUNCATE is not published (publish without 'truncate');
	// negative so that catalogs written before it publish truncate, as
	// PostgreSQL does by default
	NoTruncate bool `json:"no_truncate,omitempty"`
}

// Has reports whether the publication covers table.
func (p *Publication) Has(table string) bool {
	return p.AllTables || slices.Contains(p.Tables, table)
}

type cdcCatalog struct {
	Publications []Publication `json:"publications,omitempty"`
	// IdentityFull: tables (by name) with REPLICA IDENTITY FULL
	IdentityFull []string `json:"identity_full,omitempty"`
}

var (
	ErrPublicationExists = errors.New("publication already exists")
	ErrNoPublication     = errors.New("publication does not exist")
)

func (e *Engine) cdcCatalogPath() string { return filepath.Join(e.opts.Dir, "cdc.json") }

func (e *Engine) loadCDCCatalog() error {
	// the log's copy (written with every change, replicated) wins over
	// the file, which a promoted replica does not have
	if v, ok := e.cdcRows()["catalog"]; ok {
		var c cdcCatalog
		if err := json.Unmarshal([]byte(v), &c); err != nil {
			return fmt.Errorf("cdc catalog row: %w", err)
		}
		e.cdcCat.Store(&c)
		return nil
	}
	b, err := os.ReadFile(e.cdcCatalogPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var c cdcCatalog
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("cdc.json: %w", err)
	}
	e.cdcCat.Store(&c)
	return nil
}

// updateCDCCatalog applies fn to a copy of the catalog and saves it.
func (e *Engine) updateCDCCatalog(fn func(c *cdcCatalog) error) error {
	if e.ro.Load() {
		return ErrReadOnly
	}
	e.slotMu.Lock()
	defer e.slotMu.Unlock()
	var c cdcCatalog
	if old := e.cdcCat.Load(); old != nil {
		c.Publications = slices.Clone(old.Publications)
		c.IdentityFull = slices.Clone(old.IdentityFull)
	}
	if err := fn(&c); err != nil {
		return err
	}
	b, _ := json.Marshal(&c)
	if err := e.cdcMirror("catalog", b); err != nil { // through the log first: replicas get it
		return err
	}
	if err := writeFileSync(e.cdcCatalogPath(), b); err != nil {
		return err
	}
	e.cdcCat.Store(&c)
	return nil
}

// writeFileSync replaces path atomically and durably.
func writeFileSync(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDirPath(filepath.Dir(path))
}

// CreatePublication adds a publication; its tables must exist.
func (e *Engine) CreatePublication(p Publication) error {
	if !slotNameRe.MatchString(p.Name) {
		return fmt.Errorf("invalid publication name %q (lower case letters, digits and _, at most 63)", p.Name)
	}
	for _, t := range p.Tables {
		if _, err := e.Table(t); err != nil {
			return err
		}
	}
	p.Tables = slices.Clone(p.Tables)
	sort.Strings(p.Tables)
	p.Tables = slices.Compact(p.Tables)
	return e.updateCDCCatalog(func(c *cdcCatalog) error {
		for _, q := range c.Publications {
			if q.Name == p.Name {
				return ErrPublicationExists
			}
		}
		c.Publications = append(c.Publications, p)
		sort.Slice(c.Publications, func(i, j int) bool { return c.Publications[i].Name < c.Publications[j].Name })
		return nil
	})
}

// DropPublication removes a publication.
func (e *Engine) DropPublication(name string) error {
	return e.updateCDCCatalog(func(c *cdcCatalog) error {
		for i, q := range c.Publications {
			if q.Name == name {
				c.Publications = slices.Delete(c.Publications, i, i+1)
				return nil
			}
		}
		return ErrNoPublication
	})
}

// Publications lists the publications by name.
func (e *Engine) Publications() []Publication {
	if c := e.cdcCat.Load(); c != nil {
		return slices.Clone(c.Publications)
	}
	return nil
}

// SetReplicaIdentityFull sets whether updates and deletes of table carry
// the whole old row (FULL) or only its key (DEFAULT).
func (e *Engine) SetReplicaIdentityFull(table string, full bool) error {
	if _, err := e.Table(table); err != nil {
		return err
	}
	return e.updateCDCCatalog(func(c *cdcCatalog) error {
		c.IdentityFull = slices.DeleteFunc(c.IdentityFull, func(s string) bool { return s == table })
		if full {
			c.IdentityFull = append(c.IdentityFull, table)
			sort.Strings(c.IdentityFull)
		}
		return nil
	})
}

// ReplicaIdentityFull reports whether table has REPLICA IDENTITY FULL.
//
// A table without a visible one-column PRIMARY KEY (none, or composite)
// always behaves as FULL: its updates and deletes carry the whole old row,
// since the internal key column is hidden from consumers (docs/05).
func (e *Engine) ReplicaIdentityFull(table string) bool {
	if t, err := e.Table(table); err == nil {
		if sch := t.Schema(); !sch.HasPK() || sch.Composite() {
			return true
		}
	}
	c := e.cdcCat.Load()
	return c != nil && slices.Contains(c.IdentityFull, table)
}

// forgetCDCTable removes a dropped table from the publications that list
// it and from the REPLICA IDENTITY FULL tables. A replica has no catalog.
func (e *Engine) forgetCDCTable(table string) error {
	c := e.cdcCat.Load()
	if c == nil || e.ro.Load() {
		return nil
	}
	used := slices.Contains(c.IdentityFull, table)
	for _, p := range c.Publications {
		used = used || slices.Contains(p.Tables, table)
	}
	if !used {
		return nil
	}
	return e.updateCDCCatalog(func(c *cdcCatalog) error {
		c.IdentityFull = slices.DeleteFunc(c.IdentityFull, func(s string) bool { return s == table })
		for i := range c.Publications {
			c.Publications[i].Tables = slices.DeleteFunc(slices.Clone(c.Publications[i].Tables), func(s string) bool { return s == table })
		}
		return nil
	})
}

// renameCDCTable follows ALTER TABLE RENAME TO in publications and
// replica identity settings (kept by name).
func (e *Engine) renameCDCTable(old, name string) error {
	c := e.cdcCat.Load()
	if c == nil || e.ro.Load() {
		return nil
	}
	used := slices.Contains(c.IdentityFull, old)
	for _, p := range c.Publications {
		used = used || slices.Contains(p.Tables, old)
	}
	if !used {
		return nil
	}
	rename := func(ns []string) []string {
		ns = slices.Clone(ns)
		for i, n := range ns {
			if n == old {
				ns[i] = name
			}
		}
		return ns
	}
	return e.updateCDCCatalog(func(c *cdcCatalog) error {
		c.IdentityFull = rename(c.IdentityFull)
		for i := range c.Publications {
			c.Publications[i].Tables = rename(c.Publications[i].Tables)
		}
		return nil
	})
}
