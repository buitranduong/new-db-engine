package core

import (
	"fmt"
	"sync/atomic"
	"testing"

	"stask/internal/schema"
)

// BenchmarkIndexedWrite measures the cost of maintaining secondary indexes:
// transactions of 50 inserts or updates (which change every indexed value)
// from parallel writers, with 0, 1, 3 and 5 indexes. A transaction of one
// row would measure fdatasync, not the indexes; ns/row is the cost per row.
func BenchmarkIndexedWrite(b *testing.B) {
	for _, mode := range []string{"insert", "update"} {
		for _, n := range []int{0, 1, 3, 5} {
			b.Run(fmt.Sprintf("%s/idx=%d", mode, n), func(b *testing.B) {
				e, err := Open(Options{Dir: b.TempDir(), SegmentSize: 64 << 20, NoCheckpoint: true})
				if err != nil {
					b.Fatal(err)
				}
				defer e.Close()
				cols := []schema.Column{{Name: "id", Type: "string"}}
				for _, c := range []string{"a", "b", "c", "d", "e"} {
					cols = append(cols, schema.Column{Name: c, Type: "int"})
				}
				if _, err := e.CreateTable(schema.Table{Name: "w", Key: "id", Columns: cols}); err != nil {
					b.Fatal(err)
				}
				mk := func(id string, v int64) schema.Row {
					return schema.Row{{S: id}, {I: v * 7 % 100003}, {I: v * 13 % 9973}, {I: v % 101}, {I: v * 31}, {I: v}}
				}
				const rows = 20000
				if mode == "update" {
					for i := 0; i < rows; i += 1000 {
						var ops []WriteOp
						for j := i; j < i+1000; j++ {
							ops = append(ops, WriteOp{Table: "w", Row: mk(fmt.Sprintf("k%08d", j), int64(j))})
						}
						if _, err := e.Write(ops); err != nil {
							b.Fatal(err)
						}
					}
				}
				for i, c := range []string{"a", "b", "c", "d", "e"}[:n] {
					_ = i
					if err := e.CreateIndex("w", schema.IndexDef{Name: "w_" + c, Columns: []string{c}}); err != nil {
						b.Fatal(err)
					}
				}
				const batch = 50
				var ctr atomic.Int64
				b.SetParallelism(2)
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						base := ctr.Add(batch) - batch
						ops := make([]WriteOp, batch)
						for j := range ops {
							i := base + int64(j)
							if mode == "insert" {
								ops[j] = WriteOp{Table: "w", Row: mk(fmt.Sprintf("n%09d", i), i)}
							} else {
								ops[j] = WriteOp{Table: "w", Row: mk(fmt.Sprintf("k%08d", i%rows), i+1000003)}
							}
						}
						if _, err := e.Write(ops); err != nil {
							b.Error(err)
							return
						}
					}
				})
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/batch, "ns/row")
			})
		}
	}
}
