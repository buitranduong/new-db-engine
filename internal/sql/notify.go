package sql

import (
	"sync"
	"sync/atomic"

	"stask/internal/core"
)

// LISTEN / NOTIFY with PostgreSQL's semantics
// (https://www.postgresql.org/docs/16/sql-notify.html):
//
//   - a notification is delivered only if, and once, its transaction
//     commits, and notifications of different transactions arrive in
//     commit order (Tx.OnCommit runs in LSN order);
//   - identical channel and payload pairs within one transaction collapse
//     into one;
//   - LISTEN and UNLISTEN inside a transaction take effect at its commit;
//   - a listening session receives notifications between transactions
//     (the wire protocol delivers them when the session is idle).
//
// Notifications are not durable: a session that is not connected misses
// them (use CDC for changes that must not be lost). Unlike PostgreSQL, a
// listener whose queue is full (MaxListenerQueue: it stopped reading) loses
// new notifications instead of making NOTIFY fail at commit; the count is
// NotificationsDropped.

// Notification is one delivered notification.
type Notification struct {
	PID     int32 // the notifying session
	Channel string
	Payload string
}

// MaxListenerQueue bounds the notifications waiting for one listener.
var MaxListenerQueue = 100_000

// NotificationsDropped counts notifications lost to full listener queues.
var NotificationsDropped atomic.Int64

// MaxNotifyPayload is PostgreSQL's limit (bytes).
const MaxNotifyPayload = 8000

// Listener receives the notifications of the channels its session listens on.
type Listener struct {
	hub    *notifyHub
	mu     sync.Mutex
	chans  map[string]bool
	queue  []Notification
	signal chan struct{} // cap 1: there is something in the queue
}

// Signal fires (is readable) when notifications are waiting.
func (l *Listener) Signal() <-chan struct{} { return l.signal }

// Take returns and removes the waiting notifications.
func (l *Listener) Take() []Notification {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.queue
	l.queue = nil
	return q
}

func (l *Listener) push(n Notification) {
	l.mu.Lock()
	if len(l.queue) >= MaxListenerQueue {
		l.mu.Unlock()
		NotificationsDropped.Add(1)
		return
	}
	l.queue = append(l.queue, n)
	l.mu.Unlock()
	select {
	case l.signal <- struct{}{}:
	default:
	}
}

type notifyHub struct {
	mu     sync.RWMutex
	byChan map[string]map[*Listener]bool
}

var hubs sync.Map // *core.Engine -> *notifyHub

func hubOf(e *core.Engine) *notifyHub {
	if h, ok := hubs.Load(e); ok {
		return h.(*notifyHub)
	}
	h, _ := hubs.LoadOrStore(e, &notifyHub{byChan: map[string]map[*Listener]bool{}})
	return h.(*notifyHub)
}

func (h *notifyHub) publish(ns []Notification) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, n := range ns {
		for l := range h.byChan[n.Channel] {
			l.push(n)
		}
	}
}

func (h *notifyHub) listen(l *Listener, ch string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.byChan[ch]
	if m == nil {
		m = map[*Listener]bool{}
		h.byChan[ch] = m
	}
	m[l] = true
	l.mu.Lock()
	l.chans[ch] = true
	l.mu.Unlock()
}

func (h *notifyHub) unlisten(l *Listener, ch string, all bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.chans {
		if all || c == ch {
			delete(h.byChan[c], l)
			if len(h.byChan[c]) == 0 {
				delete(h.byChan, c)
			}
			delete(l.chans, c)
		}
	}
}

// Listener is the session's listener, nil until it has listened.
func (s *Session) Listener() *Listener { return s.listener }

// listenOp is a pending LISTEN (all=false, ch set), UNLISTEN ch or UNLISTEN *.
type listenOp struct {
	listen, all bool
	ch          string
}

func (s *Session) applyListen(op listenOp) {
	h := hubOf(s.E)
	if op.listen {
		if s.listener == nil {
			s.listener = &Listener{hub: h, chans: map[string]bool{}, signal: make(chan struct{}, 1)}
		}
		h.listen(s.listener, op.ch)
		return
	}
	if s.listener != nil {
		h.unlisten(s.listener, op.ch, op.all)
	}
}

// notify queues a notification of the current transaction.
func (s *Session) notify(ch, payload string) error {
	if ch == "" {
		return errf(codeInvalidParameter, "channel name cannot be empty")
	}
	if len(payload) >= MaxNotifyPayload {
		return errf(codeInvalidParameter, "payload string too long")
	}
	n := Notification{PID: s.PID, Channel: ch, Payload: payload}
	for _, p := range s.notes {
		if p == n {
			return nil
		}
	}
	s.notes = append(s.notes, n)
	return nil
}

// beforeCommit arranges for the transaction's notifications to be
// published when tx commits; it returns the LISTEN/UNLISTEN to apply once
// the commit succeeded.
func (s *Session) beforeCommit(tx *core.Tx) []listenOp {
	notes, ops := s.notes, s.listenOps
	s.notes, s.listenOps = nil, nil
	if len(notes) > 0 {
		h := hubOf(s.E)
		tx.OnCommit(func(uint64) { h.publish(notes) })
	}
	return ops
}

// discardPending drops the notifications and LISTEN/UNLISTEN of a
// transaction that did not commit.
func (s *Session) discardPending() { s.notes, s.listenOps = nil, nil }

// closeListener stops listening (the session ends).
func (s *Session) closeListener() {
	if s.listener != nil {
		hubOf(s.E).unlisten(s.listener, "", true)
		s.listener = nil
	}
}
