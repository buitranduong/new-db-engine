package sql

import (
	"testing"

	"stask/internal/core"
)

func openBench(b *testing.B, dir string) *core.Engine {
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true})
	if err != nil {
		b.Fatal(err)
	}
	return e
}

// Write path of a table without triggers (docs/12 P7.1: must not slow down).
func BenchmarkWriteNoTrigger(b *testing.B) { benchWrite(b, false) }

// The same with a trigger on each of the three events whose body writes a
// row to another table (3 statements, 3 trigger bodies per iteration).
func BenchmarkWriteTrigger(b *testing.B) { benchWrite(b, true) }

func benchWrite(b *testing.B, triggers bool) {
	dir := b.TempDir()
	e := openBench(b, dir)
	defer e.Close()
	s := NewSession(e)
	mustB(b, s, `CREATE TABLE w (id BIGINT PRIMARY KEY, v BIGINT)`)
	if triggers {
		mustB(b, s, `CREATE TABLE lg (id BIGINT PRIMARY KEY, v BIGINT)`)
		mustB(b, s, `CREATE TRIGGER ti AFTER INSERT ON w BEGIN INSERT INTO lg VALUES (NEW.id * 4, 1); END`)
		mustB(b, s, `CREATE TRIGGER tu AFTER UPDATE ON w BEGIN INSERT INTO lg VALUES (NEW.id * 4 + 1, NEW.v); END`)
		mustB(b, s, `CREATE TRIGGER td AFTER DELETE ON w BEGIN INSERT INTO lg VALUES (OLD.id * 4 + 2, OLD.v); END`)
	}
	mustB(b, s, `BEGIN`)
	ins, _ := ParseOne(`INSERT INTO w VALUES ($1, 0)`)
	upd, _ := ParseOne(`UPDATE w SET v = v + 1 WHERE id = $1`)
	del, _ := ParseOne(`DELETE FROM w WHERE id = $1`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := IntD(int64(i))
		if _, err := s.Exec(ins, []Datum{id}); err != nil {
			b.Fatal(err)
		}
		if _, err := s.Exec(upd, []Datum{id}); err != nil {
			b.Fatal(err)
		}
		if _, err := s.Exec(del, []Datum{id}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	mustB(b, s, `ROLLBACK`)
}

func mustB(b *testing.B, s *Session, q string) {
	st, err := ParseOne(q)
	if err == nil {
		_, err = s.Exec(st, nil)
	}
	if err != nil {
		b.Fatal(err)
	}
}
