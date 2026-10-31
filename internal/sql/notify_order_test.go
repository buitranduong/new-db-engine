package sql

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNotifyOrderSameForAllListeners: notifications of concurrent sessions
// reach every listener in one and the same order (PostgreSQL has a single
// queue). Found by the CDC suite, scenario f_notify: publishes interleaved
// differently on different listeners.
func TestNotifyOrderSameForAllListeners(t *testing.T) {
	e := newEngine(t)
	l1, l2 := NewSession(e), NewSession(e)
	l1.PID, l2.PID = 100, 101
	run(t, l1, `LISTEN ord`)
	run(t, l2, `LISTEN ord`)
	const senders, per = 8, 300
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		s := NewSession(e)
		s.PID = int32(i + 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < per; k++ {
				run(t, s, fmt.Sprintf(`NOTIFY ord, '%d:%d'`, s.PID, k))
			}
		}()
	}
	wg.Wait()
	take := func(s *Session) string {
		var out []string
		deadline := time.Now().Add(5 * time.Second)
		for len(out) < senders*per && time.Now().Before(deadline) {
			for _, n := range s.Listener().Take() {
				out = append(out, n.Payload)
			}
			time.Sleep(time.Millisecond)
		}
		return strings.Join(out, " ")
	}
	a, b := take(l1), take(l2)
	if len(strings.Fields(a)) != senders*per {
		t.Fatalf("listener 1 got %d of %d", len(strings.Fields(a)), senders*per)
	}
	if a != b {
		t.Fatal("two listeners saw the notifications of concurrent senders in different orders")
	}
}
