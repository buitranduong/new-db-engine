package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
)

// Pump runs a consumer loop: receive a transaction, apply it (when an
// Applier is set), then acknowledge its end LSN. That order (apply, then
// confirm) is what makes delivery at-least-once and the apply idempotent.
type Pump struct {
	t     *T
	R     *Repl
	Ap    *Applier
	mu    sync.Mutex
	All   []*TxEvt
	Seen  map[int64]bool // ev ids received
	Err   error
	stop  chan struct{}
	done  chan struct{}
	Acked pglogrepl.LSN
	// NoAck: do not acknowledge (a consumer that never confirms)
	NoAck bool
	// OnTx is called for every transaction before it is applied
	OnTx func(*TxEvt)
	// Reconnect, if set, opens a new started stream when the connection
	// breaks (failover, restart); start is where the applier left off.
	Reconnect  func(start pglogrepl.LSN) (*Repl, error)
	Reconnects int
}

func startPump(t *T, r *Repl, ap *Applier) *Pump {
	p := &Pump{t: t, R: r, Ap: ap, Seen: map[int64]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	go p.run()
	return p
}

func (p *Pump) run() {
	defer close(p.done)
	for {
		select {
		case <-p.stop:
			return
		default:
		}
		tx, err := p.R.Next(bg(), 300*time.Millisecond)
		if err == errTimeout {
			continue
		}
		if err != nil {
			select {
			case <-p.stop:
				return
			default:
			}
			if p.Reconnect != nil {
				p.R.Close()
				var start pglogrepl.LSN
				if p.Ap != nil {
					start = p.Ap.LastEnd
				}
				r, rerr := p.Reconnect(start)
				if rerr == nil {
					p.mu.Lock()
					p.R = r
					p.Reconnects++
					p.mu.Unlock()
					continue
				}
				err = fmt.Errorf("%v; reconnect: %w", err, rerr)
			}
			p.mu.Lock()
			p.Err = err
			p.mu.Unlock()
			return
		}
		if p.OnTx != nil {
			p.OnTx(tx)
		}
		p.mu.Lock()
		p.All = append(p.All, tx)
		for i := range tx.Chgs {
			if tx.Chgs[i].Rel == "ev" && tx.Chgs[i].Op == 'I' {
				var id int64
				fmt.Sscan(string(tx.Chgs[i].New["id"].Data), &id)
				p.Seen[id] = true
			}
		}
		p.mu.Unlock()
		if p.Ap != nil {
			if err := p.Ap.Apply(tx); err != nil {
				if p.Ap.Connect != nil && p.Reconnect != nil && p.Ap.Resync() == nil {
					p.R.Close() // the stream restarts at the position the applier has
					r, rerr := p.Reconnect(p.Ap.LastEnd)
					if rerr == nil {
						p.mu.Lock()
						p.R = r
						p.Reconnects++
						p.mu.Unlock()
						continue
					}
					err = fmt.Errorf("%v; reconnect: %w", err, rerr)
				}
				p.mu.Lock()
				p.Err = err
				p.mu.Unlock()
				return
			}
		}
		if !p.NoAck {
			if err := p.R.Ack(bg(), tx.EndLSN); err != nil {
				p.mu.Lock()
				p.Err = fmt.Errorf("ack: %w", err)
				p.mu.Unlock()
				return
			}
			p.mu.Lock()
			p.Acked = tx.EndLSN
			p.mu.Unlock()
		}
	}
}

// Stop ends the loop (the connection is left open) and returns what was received.
func (p *Pump) Stop() []*TxEvt {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*TxEvt(nil), p.All...)
}

func (p *Pump) Failed() error { p.mu.Lock(); defer p.mu.Unlock(); return p.Err }

// WaitEv waits until the ev row id has been received (and applied).
func (p *Pump) WaitEv(id int64, d time.Duration) bool {
	return waitFor(d, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.Seen[id] || p.Err != nil
	}) && p.Failed() == nil
}

func (p *Pump) Count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.All) }
