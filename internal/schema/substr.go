package schema

import "fmt"

// SubstrIndex declares a substring/trigram search index on a text column
// (CREATE INDEX name ON t USING gin (col gin_trgm_ops), or USING search /
// trigram). It is served by the substring view (internal/proj/substr.go),
// which is derived from the log like the other views; the core keeps only
// the declaration.
type SubstrIndex struct {
	Name string `json:"name"`
	Col  string `json:"col"`
}

func (t *Table) checkSubstr() error {
	for _, s := range t.Substr {
		i, ok := t.idx[s.Col]
		if !ok || (t.Columns[i].T != TText && t.Columns[i].T != TString) {
			return fmt.Errorf("trigram index %q: column %q must be text/string", s.Name, s.Col)
		}
	}
	return nil
}
