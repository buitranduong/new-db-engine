package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// wl1Redis runs the WL-1 mix against Redis (-redis host:port): the same
// keys, values, operation mix and recorder as over HTTP, one connection per
// client, so Redis is the yardstick for point reads and writes (docs/11 §5).
// A row is stored as its val string; inserts use SET NX.
func wl1Redis(addr string) {
	pool := make([]string, 256)
	prng := rand.New(rand.NewSource(42))
	for i := range pool {
		pool[i] = randString(prng, *valSize)
	}
	key := func(i int) string { return fmt.Sprintf("k%09d", i) }
	res := map[string]any{"mode": "redis"}
	if !*skipLoad {
		t0 := time.Now()
		var wg sync.WaitGroup
		var next atomic.Int64
		for l := 0; l < 8; l++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := dialRedis(addr)
				defer c.c.Close()
				for {
					lo := int(next.Add(200)) - 200
					if lo >= *nKeys {
						return
					}
					hi := min(lo+200, *nKeys)
					for i := lo; i < hi; i++ {
						c.send("SET", key(i), pool[i%len(pool)])
					}
					for i := lo; i < hi; i++ {
						if err := c.reply(); err != nil {
							log.Fatalf("load: %v", err)
						}
					}
				}
			}()
		}
		wg.Wait()
		d := time.Since(t0)
		res["load_s"] = d.Seconds()
		res["load_rows_per_s"] = float64(*nKeys) / d.Seconds()
	}
	conns := make([]*redisConn, *conc)
	for i := range conns {
		conns[i] = dialRedis(addr)
	}
	var insSeq atomic.Int64
	insSeq.Store(time.Now().UnixNano() / 1000)
	recs, el := run(*conc, *dur, *rate, func(w int, rng *rand.Rand, rec *recorder) (string, bool) {
		c := conns[w]
		p := rng.Intn(100)
		switch {
		case p < 70:
			c.send("GET", key(rng.Intn(*nKeys)))
			return "read", c.reply() == nil && !c.null
		case p < 90:
			c.send("SET", key(rng.Intn(*nKeys)), pool[rng.Intn(len(pool))])
			return "update", c.reply() == nil
		default:
			c.send("SET", fmt.Sprintf("i%02d-%012d", w, insSeq.Add(1)), pool[rng.Intn(len(pool))], "NX")
			return "insert", c.reply() == nil && !c.null
		}
	})
	res["ops"] = recs.summary(el)
	res["timeline_max_ms"] = timeline(recs)
	report("WL-1 point operations", res)
}

// redisConn is a minimal RESP2 client: commands are buffered by send and
// flushed by reply, which reads one reply and discards its payload.
type redisConn struct {
	c    net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	null bool // the last reply was a null bulk string
}

func dialRedis(addr string) *redisConn {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	return &redisConn{c: c, r: bufio.NewReaderSize(c, 64<<10), w: bufio.NewWriterSize(c, 64<<10)}
}

func (c *redisConn) send(args ...string) {
	c.w.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		c.w.WriteString("$" + strconv.Itoa(len(a)) + "\r\n")
		c.w.WriteString(a)
		c.w.WriteString("\r\n")
	}
}

func (c *redisConn) reply() error {
	if c.w.Buffered() > 0 {
		if err := c.w.Flush(); err != nil {
			return err
		}
	}
	c.null = false
	line, err := c.r.ReadSlice('\n')
	if err != nil {
		return err
	}
	if len(line) < 3 {
		return errors.New("short reply")
	}
	switch line[0] {
	case '+', ':':
		return nil
	case '-':
		return errors.New(string(line[1 : len(line)-2]))
	case '$':
		n, err := strconv.Atoi(string(line[1 : len(line)-2]))
		if err != nil {
			return err
		}
		if n < 0 {
			c.null = true
			return nil
		}
		_, err = c.r.Discard(n + 2)
		return err
	}
	return fmt.Errorf("unexpected reply %q", line)
}
