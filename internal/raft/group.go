package raft

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Transport delivers a message to the node Msg.To, best effort. It must not
// block the caller for long and must not call back into the Group.
type Transport interface {
	Send(Msg)
}

var (
	// ErrLeadershipLost: the entry did not commit under the leader that
	// accepted it; it may or may not be committed later by another
	// leader (it is, only if that leader has it). The caller must treat
	// the outcome as unknown, as for a failed fsync.
	ErrLeadershipLost = errors.New("raft: leadership lost before the entry committed")
	ErrStopped        = errors.New("raft: group stopped")
)

// Group runs a Node in a real process: a ticker, a mutex around the node,
// and Propose, which waits for the commit.
type Group struct {
	mu      sync.Mutex
	n       *Node
	tick    *time.Ticker
	stop    chan struct{}
	done    chan struct{}
	waiters map[uint64]chan Entry // index -> committed entry
	apply   func(Entry)
}

// NewGroup starts a Group. cfg.Send and cfg.Apply are taken over: sends go
// to tr, and entries go to apply after the waiters have been told.
func NewGroup(cfg Config, tr Transport, apply func(Entry), tick time.Duration) (*Group, error) {
	g := &Group{waiters: map[uint64]chan Entry{}, stop: make(chan struct{}), done: make(chan struct{}), apply: apply}
	cfg.Send = tr.Send
	cfg.Apply = g.onApply
	n, err := NewNode(cfg)
	if err != nil {
		return nil, err
	}
	g.n = n
	g.tick = time.NewTicker(tick)
	go func() {
		defer close(g.done)
		for {
			select {
			case <-g.tick.C:
				g.mu.Lock()
				g.n.Tick()
				g.mu.Unlock()
			case <-g.stop:
				return
			}
		}
	}()
	return g, nil
}

// onApply runs under g.mu (called by the node).
func (g *Group) onApply(e Entry) {
	if ch, ok := g.waiters[e.Index]; ok {
		ch <- e
		delete(g.waiters, e.Index)
	}
	if g.apply != nil {
		g.apply(e)
	}
}

// Step delivers a message from the transport.
func (g *Group) Step(m Msg) {
	g.mu.Lock()
	g.n.Step(m)
	g.mu.Unlock()
}

// Propose appends data and waits until it is committed and applied. It
// returns ErrNotLeader at once on a node that is not the leader.
func (g *Group) Propose(ctx context.Context, data []byte) (uint64, error) {
	g.mu.Lock()
	idx, term, err := g.n.Propose(data)
	if err != nil {
		g.mu.Unlock()
		return 0, err
	}
	if g.n.Applied() >= idx { // a single node commits inside Propose
		e, _ := g.n.Entry(idx)
		g.mu.Unlock()
		if e.Term != term {
			return 0, ErrLeadershipLost
		}
		return idx, nil
	}
	ch := make(chan Entry, 1)
	g.waiters[idx] = ch
	g.mu.Unlock()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case e := <-ch:
			if e.Term != term {
				return 0, ErrLeadershipLost
			}
			return idx, nil
		case <-t.C:
			g.mu.Lock()
			role, curTerm, _, _ := g.n.State()
			lost := role != Leader || curTerm != term
			if lost {
				if g.waiters[idx] == ch {
					delete(g.waiters, idx)
				}
			}
			g.mu.Unlock()
			if lost {
				select { // it may have committed just now
				case e := <-ch:
					if e.Term == term {
						return idx, nil
					}
				default:
				}
				return 0, ErrLeadershipLost
			}
		case <-ctx.Done():
			g.mu.Lock()
			delete(g.waiters, idx)
			g.mu.Unlock()
			return 0, ctx.Err()
		case <-g.stop:
			return 0, ErrStopped
		}
	}
}

// Status returns the node's role, term and leader, and whether its lease
// is valid.
func (g *Group) Status() (role Role, term, leader uint64, lease bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	role, term, leader, _ = g.n.State()
	return role, term, leader, g.n.LeaseValid()
}

func (g *Group) Stop() {
	select {
	case <-g.stop:
		return
	default:
	}
	close(g.stop)
	<-g.done
	g.tick.Stop()
}
