package main

import (
	"fmt"
	"time"
)

// Topo is the system under a drill: one node or a 3-node Raft cluster.
type Topo struct {
	E       *Env
	Nodes   []*Node
	Cluster bool
}

// common flags: small segments, frequent checkpoints and view saves so that a
// kill lands in all of them; a disk-resident secondary index.
func commonFlags() []string {
	return []string{"-checkpoint-min-mb", "16", "-view-save-every", "3s", "-secindex-disk"}
}

// TopoOpts tunes a Topo.
type TopoOpts struct {
	Plain  bool               // no common flags, no history store: only Extra
	NoHist bool               // single node: no history store
	Extra  []string           // more staskd flags
	Dir    func(i int) string // data directory of node i (0-based), "" for the default
}

func newTopo(e *Env, name, mode string, extra ...string) *Topo {
	return newTopoOpt(e, name, mode, TopoOpts{Extra: extra})
}

func newTopoOpt(e *Env, name, mode string, o TopoOpts) *Topo {
	t := &Topo{E: e, Cluster: mode == "cluster"}
	var flags []string
	if !o.Plain {
		flags = append(commonFlags(), o.Extra...)
	} else {
		flags = append(flags, o.Extra...)
	}
	if t.Cluster {
		t.Nodes = e.NewCluster(name, 3, flags...)
	} else {
		if !o.Plain && !o.NoHist {
			flags = append(flags, "-gc-interval", "2s", "-hist-store", "-hist-store-tables", "acct,ev")
		}
		t.Nodes = []*Node{e.NewNode(name, flags...)}
	}
	if o.Dir != nil {
		for i, n := range t.Nodes {
			if d := o.Dir(i); d != "" {
				n.SetDir(d)
			}
		}
	}
	return t
}

func (t *Topo) DSN() string {
	if t.Cluster {
		return ClusterDSN(t.Nodes)
	}
	return t.Nodes[0].URL()
}

// UpAll starts every node that is not running and waits until the system
// takes writes.
func (t *Topo) UpAll() error {
	for _, n := range t.Nodes {
		if !n.Running() {
			if err := n.Start(); err != nil {
				return err
			}
		}
	}
	return t.WaitWritable(120 * time.Second)
}

func (t *Topo) WaitWritable(d time.Duration) error {
	if !t.Cluster {
		return t.Nodes[0].WaitSQL(d)
	}
	_, err := WaitLeader(t.Nodes, d)
	return err
}

func (t *Topo) Leader() *Node {
	if !t.Cluster {
		return t.Nodes[0]
	}
	l, _ := WaitLeader(t.Nodes, 30*time.Second)
	return l
}

func (t *Topo) KillAll() {
	for _, n := range t.Nodes {
		n.Kill9()
	}
}

func fnum(m map[string]any, k string) uint64 {
	f, _ := m[k].(float64)
	return uint64(f)
}

// Settle waits until every running node has applied everything the system
// committed and its views have caught up.
func (t *Topo) Settle(d time.Duration) error {
	end := time.Now().Add(d)
	if !t.Cluster {
		return t.Nodes[0].WaitReady(d)
	}
	for {
		var commit uint64
		l := t.Leader()
		if l != nil {
			if st, err := l.ClusterStatus(); err == nil {
				commit = fnum(st, "commit")
			}
		}
		ok := l != nil && commit > 0
		for _, n := range t.Nodes {
			if !n.Running() || !ok {
				continue
			}
			st, err := n.ClusterStatus()
			if err != nil || fnum(st, "applied") < commit {
				ok = false
				continue
			}
			if err := n.WaitReady(2 * time.Second); err != nil {
				ok = false
			}
		}
		if ok {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("the cluster did not settle within %v (commit %d)", d, commit)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// CheckViews runs the full reconcile on every running node and records the
// verdict; every structure must be IN_SYNC once the node has settled.
func (t *Topo) CheckViews(r *Result, extra ...string) {
	for _, n := range t.Nodes {
		if !n.Running() {
			continue
		}
		rep, err := n.WaitInSync(120*time.Second, extra...)
		if !r.Check(err == nil, "node %d: reconcile: %v", n.ID, err) {
			continue
		}
		s := ReconcileSummary(rep)
		if r.Check(rep.Verdict == "IN_SYNC", "node %d views differ from the core: %s", n.ID, s) {
			keys, nstruct := 0, 0
			for _, tb := range rep.Tables {
				for _, st := range tb.Structures {
					keys += int(st.KeysChecked)
					nstruct++
				}
			}
			r.Logf("node %d reconcile -full: IN_SYNC (%d structures, %d key comparisons, %d ms)", n.ID, nstruct, keys, rep.DurationMs)
		}
	}
}
