package sql

import (
	"testing"

	"stask/internal/core"
)

func BenchmarkPointSelect(b *testing.B) {
	for _, mode := range []string{"plain", "tracked", "tracked+role"} {
		b.Run(mode, func(b *testing.B) {
			e := benchEngine(b)
			su := NewSession(e)
			if _, err := tryRun(su, `CREATE TABLE bt (id BIGINT PRIMARY KEY, v TEXT)`); err != nil {
				b.Fatal(err)
			}
			tryRun(su, `INSERT INTO bt VALUES (1,'a')`)
			s := NewSession(e)
			if mode != "plain" {
				s.Track("u", "app", "127.0.0.1", 1)
				defer s.Close()
			}
			if mode == "tracked+role" {
				tryRun(su, `CREATE ROLE r LOGIN`)
				tryRun(su, `GRANT SELECT ON bt TO r`)
				s.SetRole("r")
			}
			st, _ := ParseOne(`SELECT v FROM bt WHERE id = 1`)
			q := "SELECT v FROM bt WHERE id = 1"
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.SetQuery(q)
				if _, err := s.Exec(st, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchEngine(b *testing.B) *core.Engine {
	e, err := core.Open(core.Options{Dir: b.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { e.Close() })
	return e
}
