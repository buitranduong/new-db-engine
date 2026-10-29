package planner

import (
	"fmt"
	"time"

	"stask/internal/proj"
)

// SubstrChoose decides whether a SQL pattern predicate on table.col reads
// the substring view or the core, under the caller's consistency contract:
//
//	strong   the view must reach the durable LSN (waits up to 2s) else core
//	bounded  the view may lag by at most maxLag (default 1s); if it lags
//	         more it is waited for up to maxLag, else the core answers
//	session  the view must reach minLSN (the session's read-your-writes token)
//	eventual whatever the view has
//
// The returned plan names the engine, the view's applied LSN and its lag,
// so a client never reads stale index content unknowingly.
func (p *Platform) SubstrChoose(table, col, mode string, maxLag time.Duration, minLSN uint64) (Plan, bool) {
	t, err := p.E.UserTable(table)
	if err != nil {
		return Plan{Engine: "core", Reason: err.Error()}, false
	}
	c := Consistency{Mode: mode, MaxLagMs: max(1, maxLag.Milliseconds()), MinLSN: minLSN} // 0 would mean the default
	if err := checkMode(c); err != nil {
		return Plan{Engine: "core", Reason: err.Error()}, false
	}
	has := p.Sub.Has(t.Schema().ID, col)
	use, pl := p.choose(p.SubF, has, "substr", c, "bounded")
	if !has && p.SubF.Ready() {
		pl.Reason = fmt.Sprintf("no trigram index on %s(%s)", table, col)
		for _, x := range t.Schema().Substr {
			if x.Col == col {
				pl.Reason = fmt.Sprintf("trigram index %s on %s(%s) is still being built (CREATE INDEX CONCURRENTLY or not finished): the table is scanned", x.Name, table, col)
			}
		}
	}
	return pl, use
}

// SubstrScan reads one batch of matching keys from the view (see
// proj.Substr.Scan).
func (p *Platform) SubstrScan(table string, q *proj.SubstrReq, after uint64, max int) (keys []string, next uint64, done, ok bool) {
	t, err := p.E.UserTable(table)
	if err != nil {
		return nil, 0, true, false
	}
	return p.Sub.Scan(t.Schema().ID, q, after, max)
}

// SubstrApplied is the view's applied LSN and lag, for plans and metrics.
func (p *Platform) SubstrApplied() (uint64, time.Duration) { return p.SubF.Applied(), p.SubF.Lag() }

// SubstrBuilt reports whether the view has applied lsn and holds the trigram
// index on table.col: CREATE INDEX waits for it unless CONCURRENTLY.
func (p *Platform) SubstrBuilt(table, col string, lsn uint64) bool {
	t, err := p.E.UserTable(table)
	if err != nil {
		return true // dropped meanwhile: nothing to wait for
	}
	return p.SubF.Ready() && p.SubF.Applied() >= lsn && p.Sub.Has(t.Schema().ID, col)
}
