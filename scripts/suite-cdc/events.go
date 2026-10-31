package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
)

// ---- HTTP CDC (NDJSON, wal2json format 2) ----

type httpLine struct {
	LSN    string          `json:"lsn"`
	Xid    uint64          `json:"xid"`
	Data   json.RawMessage `json:"data"`
	Commit bool            `json:"commit"`
	Error  string          `json:"error"`
}

type w2Col struct {
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type w2Change struct {
	Action   string          `json:"action"`
	Table    string          `json:"table"`
	LSN      string          `json:"lsn"`
	NextLSN  string          `json:"nextlsn"`
	Columns  []w2Col         `json:"columns"`
	Identity []w2Col         `json:"identity"`
	Xid      json.RawMessage `json:"xid"`
}

// HTx is one transaction of the HTTP stream.
type HTx struct {
	Xid       uint64
	CommitLSN string // lsn of the commit line (the value to acknowledge)
	Changes   []w2Change
}

func (c w2Change) col(name string) string {
	for _, x := range c.Columns {
		if x.Name == name {
			return strings.Trim(string(x.Value), `"`)
		}
	}
	return ""
}

func httpDo(method, u string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func httpCreateSlot(n *Node, name, plugin string) error {
	code, b, err := httpDo("POST", "http://"+n.HTTP+"/v1/cdc", map[string]string{"name": name, "plugin": plugin})
	if err != nil {
		return err
	}
	if code != 201 {
		return fmt.Errorf("create slot: HTTP %d %s", code, b)
	}
	return nil
}

func httpAck(n *Node, slot, lsn string) error {
	code, b, err := httpDo("POST", "http://"+n.HTTP+"/v1/cdc/"+slot+"/ack?lsn="+url.QueryEscape(lsn), nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("ack: HTTP %d %s", code, b)
	}
	return nil
}

// HTTPStream reads /v1/cdc/{slot}/stream and groups lines into transactions.
type HTTPStream struct {
	Txs    chan *HTx
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
	Raw    int
}

func (h *HTTPStream) Err() error { h.mu.Lock(); defer h.mu.Unlock(); return h.err }
func (h *HTTPStream) Close()     { h.cancel() }

func openHTTPStream(n *Node, slot string, q url.Values) *HTTPStream {
	ctx, cancel := context.WithCancel(bg())
	h := &HTTPStream{Txs: make(chan *HTx, 100000), cancel: cancel}
	if q == nil {
		q = url.Values{}
	}
	if q.Get("include-transaction") == "" {
		q.Set("include-transaction", "true")
	}
	if q.Get("include-xids") == "" {
		q.Set("include-xids", "true")
	}
	u := "http://" + n.HTTP + "/v1/cdc/" + slot + "/stream?" + q.Encode()
	go func() {
		defer close(h.Txs)
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			h.mu.Lock()
			h.err = err
			h.mu.Unlock()
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			h.mu.Lock()
			h.err = fmt.Errorf("HTTP %d %s", resp.StatusCode, b)
			h.mu.Unlock()
			return
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		var cur *HTx
		for sc.Scan() {
			var l httpLine
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				h.mu.Lock()
				h.err = fmt.Errorf("bad line %q: %v", sc.Text(), err)
				h.mu.Unlock()
				return
			}
			h.Raw++
			if l.Error != "" {
				h.mu.Lock()
				h.err = fmt.Errorf("stream error: %s", l.Error)
				h.mu.Unlock()
				return
			}
			var ch w2Change
			if err := json.Unmarshal(l.Data, &ch); err != nil {
				h.mu.Lock()
				h.err = fmt.Errorf("bad data %q: %v", l.Data, err)
				h.mu.Unlock()
				return
			}
			switch ch.Action {
			case "B":
				cur = &HTx{Xid: l.Xid}
			case "C":
				if cur == nil {
					h.mu.Lock()
					h.err = fmt.Errorf("commit without begin")
					h.mu.Unlock()
					return
				}
				cur.CommitLSN = l.LSN
				h.Txs <- cur
				cur = nil
			default:
				if cur == nil {
					h.mu.Lock()
					h.err = fmt.Errorf("change outside a transaction: %s", l.Data)
					h.mu.Unlock()
					return
				}
				cur.Changes = append(cur.Changes, ch)
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			h.mu.Lock()
			h.err = err
			h.mu.Unlock()
		}
	}()
	return h
}

// ---- LISTEN / NOTIFY ----

// Listener is a session LISTENing on channels and collecting notifications.
type Listener struct {
	c    *pgx.Conn
	mu   sync.Mutex
	Got  []Notif
	stop chan struct{}
	done chan struct{}
}

type Notif struct {
	Channel string
	Payload string
	At      time.Time
}

func startListener(t *T, url string, channels ...string) *Listener {
	t.Helper()
	l := &Listener{c: mustConn(t, url), stop: make(chan struct{}), done: make(chan struct{})}
	for _, ch := range channels {
		ex(t, l.c, "LISTEN "+ch)
	}
	go func() {
		defer close(l.done)
		for {
			select {
			case <-l.stop:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(bg(), 200*time.Millisecond)
			n, err := l.c.WaitForNotification(ctx)
			cancel()
			if err == nil {
				l.mu.Lock()
				l.Got = append(l.Got, Notif{n.Channel, n.Payload, time.Now()})
				l.mu.Unlock()
			} else if l.c.IsClosed() {
				return
			}
		}
	}()
	return l
}

func (l *Listener) Stop() {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	<-l.done
}

func (l *Listener) Snapshot() []Notif {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Notif(nil), l.Got...)
}

// Exec runs a statement on the listener's own connection while it is not
// waiting (stops the wait loop first).
func (l *Listener) Pause() func() {
	close(l.stop)
	<-l.done
	return func() {
		l.stop = make(chan struct{})
		l.done = make(chan struct{})
		go func() {
			defer close(l.done)
			for {
				select {
				case <-l.stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(bg(), 200*time.Millisecond)
				n, err := l.c.WaitForNotification(ctx)
				cancel()
				if err == nil {
					l.mu.Lock()
					l.Got = append(l.Got, Notif{n.Channel, n.Payload, time.Now()})
					l.mu.Unlock()
				} else if l.c.IsClosed() {
					return
				}
			}
		}()
	}
}

// waitFor polls cond until it holds or the time is up.
func waitFor(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// drainRepl reads transactions until one with commit LSN >= upto has been applied.
func lsnOf(s string) pglogrepl.LSN {
	l, _ := pglogrepl.ParseLSN(s)
	return l
}
