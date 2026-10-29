package sql

import (
	"testing"

	"stask/internal/core"
)

func BenchmarkPointSelectParallel(b *testing.B) {
	e, err := core.Open(core.Options{Dir: b.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	su := NewSession(e)
	for _, q := range []string{`CREATE TABLE bt (id BIGINT PRIMARY KEY, v TEXT)`, `INSERT INTO bt VALUES (1,'a')`} {
		st, _ := ParseOne(q)
		if _, err := su.Exec(st, nil); err != nil {
			b.Fatal(err)
		}
	}
	st, _ := ParseOne(`SELECT v FROM bt WHERE id = 1`)
	b.RunParallel(func(pb *testing.PB) {
		s := NewSession(e)
		for pb.Next() {
			if _, err := s.Exec(st, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
