package pgwire

import (
	"context"
	"net"
	"sync"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"

	"stask/internal/core"
)

func BenchmarkWirePoint(b *testing.B) {
	e, err := core.Open(core.Options{Dir: b.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
	if err != nil {
		b.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	s := &Server{E: e, Logf: func(string, ...any) {}}
	s.prepareAuth()
	go s.Serve(ln)
	defer func() { s.Close(); e.Close() }()
	ctx := context.Background()
	url := "postgres://app@" + ln.Addr().String() + "/stask?sslmode=disable"
	c0, err := pgx.Connect(ctx, url)
	if err != nil {
		b.Fatal(err)
	}
	c0.Exec(ctx, `CREATE TABLE bt (id BIGINT PRIMARY KEY, v TEXT)`)
	c0.Exec(ctx, `INSERT INTO bt VALUES (1,'a')`)
	var mu sync.Mutex
	var r0 syscall.Rusage
	syscall.Getrusage(0, &r0)
	b.ResetTimer()
	defer func() {
		var r1 syscall.Rusage
		syscall.Getrusage(0, &r1)
		cpu := (r1.Utime.Nano() + r1.Stime.Nano()) - (r0.Utime.Nano() + r0.Stime.Nano())
		b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
	}()
	b.RunParallel(func(pb *testing.PB) {
		mu.Lock()
		c, err := pgx.Connect(ctx, url)
		mu.Unlock()
		if err != nil {
			b.Error(err)
			return
		}
		defer c.Close(ctx)
		var v string
		for pb.Next() {
			if err := c.QueryRow(ctx, `SELECT v FROM bt WHERE id = $1`, 1).Scan(&v); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
