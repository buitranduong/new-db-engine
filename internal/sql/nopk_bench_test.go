package sql

import (
	"fmt"
	"strings"
	"testing"

	"stask/internal/core"
)

// Cost of the hidden key: 1000-row INSERT statements into a table with a
// one-column PRIMARY KEY, without one (nextval per row) and with a
// composite one (key encoded per row).
func openBenchEngine(b *testing.B) (*core.Engine, error) {
	e, err := core.Open(core.Options{Dir: b.TempDir(), SegmentSize: 64 << 20, NoCheckpoint: true})
	if err == nil {
		b.Cleanup(func() { e.Close() })
	}
	return e, err
}

func BenchmarkInsert1000(b *testing.B) {
	for _, c := range []struct{ name, ddl string }{
		{"pk", `CREATE TABLE bt (id BIGINT PRIMARY KEY, g INT, v TEXT)`},
		{"nopk", `CREATE TABLE bt (id BIGINT, g INT, v TEXT)`},
		{"composite", `CREATE TABLE bt (id BIGINT, g INT, v TEXT, PRIMARY KEY (id, g))`},
	} {
		b.Run(c.name, func(b *testing.B) {
			e, err := openBenchEngine(b)
			if err != nil {
				b.Fatal(err)
			}
			s := NewSession(e)
			if _, err := tryRun(s, c.ddl); err != nil {
				b.Fatal(err)
			}
			n := 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var sb strings.Builder
				sb.WriteString("INSERT INTO bt VALUES ")
				for j := 0; j < 1000; j++ {
					if j > 0 {
						sb.WriteByte(',')
					}
					fmt.Fprintf(&sb, "(%d, %d, 'row')", n, j)
					n++
				}
				if _, err := tryRun(s, sb.String()); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*1000), "ns/row")
		})
	}
}
