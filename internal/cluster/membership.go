package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"stask/internal/raft"
)

// Membership change (see the raft package comment for the protocol): the
// leader appends a configuration entry; every node uses the new voter set
// from the moment it has the entry. This file keeps the addresses of the
// nodes (flags, configuration entries, snapshots) and runs a connection to
// each voter.

type member struct {
	Raft string `json:"raft"`
	HTTP string `json:"http,omitempty"`
}

func (c *Cluster) membersPath() string { return filepath.Join(c.raftDir(), "members.json") }

// loadMembers reads the addresses saved by earlier runs (flags win over them).
func (c *Cluster) loadMembers() {
	b, err := os.ReadFile(c.membersPath())
	if err != nil {
		return
	}
	var m map[uint64]member
	if json.Unmarshal(b, &m) == nil {
		for id, v := range m {
			if v.Raft != "" {
				c.members[id] = v
			}
		}
	}
}

func (c *Cluster) memberList() map[uint64]member {
	c.mmu.RLock()
	defer c.mmu.RUnlock()
	out := make(map[uint64]member, len(c.members))
	for id, m := range c.members {
		out[id] = m
	}
	return out
}

func (c *Cluster) saveMembers() {
	b, _ := json.Marshal(c.memberList())
	if err := writeFileSync(c.membersPath(), b); err != nil {
		c.cfg.Logf("cluster: saving the member addresses: %v", err)
	}
}

// members0 is the address record of node id (zero if unknown).
func (c *Cluster) members0(id uint64) member {
	c.mmu.RLock()
	defer c.mmu.RUnlock()
	return c.members[id]
}

// ensurePeer starts the connection to node id, once its address is known.
func (c *Cluster) ensurePeer(id uint64) error {
	c.mmu.Lock()
	defer c.mmu.Unlock()
	if id == c.cfg.ID || !c.started || c.closed.Load() || c.peers[id] != nil {
		return nil
	}
	addr := c.members[id].Raft
	if addr == "" {
		return fmt.Errorf("cluster: no address for node %d", id)
	}
	sec, err := c.cfg.Settings.Replica(addr)
	if err != nil {
		return err
	}
	p := &peerConn{c: c, id: id, addr: addr, sec: sec, out: make(chan raft.Msg, 4096), quit: make(chan struct{})}
	c.peers[id] = p
	c.wg.Add(1)
	go p.run()
	return nil
}

func (c *Cluster) stopPeer(id uint64) {
	c.mmu.Lock()
	p := c.peers[id]
	delete(c.peers, id)
	c.mmu.Unlock()
	if p != nil {
		close(p.quit)
	}
}

// onConf runs inside the Raft node, when a configuration entry enters its
// log (also while the node starts, from the saved log): learn the address
// and, for an added node, connect to it so that the leader can replicate.
func (c *Cluster) onConf(e raft.Entry) {
	cc, err := raft.DecodeConf(e.Data)
	if err != nil || cc.Type != raft.ConfAdd {
		return
	}
	var m member
	if json.Unmarshal(cc.Context, &m) != nil || m.Raft == "" {
		return
	}
	c.mmu.Lock()
	c.members[cc.ID] = m
	c.mmu.Unlock()
	c.saveMembers()
	if cc.ID != c.cfg.ID {
		if err := c.ensurePeer(cc.ID); err != nil {
			c.cfg.Logf("cluster: %v", err)
		}
	}
}

// applyConf runs when a configuration entry is committed.
func (c *Cluster) applyConf(e raft.Entry) error {
	cc, err := raft.DecodeConf(e.Data)
	if err != nil {
		return err
	}
	switch cc.Type {
	case raft.ConfAdd:
		c.cfg.Logf("cluster: node %d is a voter (entry %d)", cc.ID, e.Index)
	case raft.ConfRemove:
		c.cfg.Logf("cluster: node %d is no longer a voter (entry %d)", cc.ID, e.Index)
		if cc.ID == c.cfg.ID {
			c.cfg.Logf("cluster: this node was removed from the cluster: stop it and delete its data directory before it is reused")
		} else {
			c.stopPeer(cc.ID)
		}
	}
	return nil
}

// ChangeMember adds a voter ("add", with the new node's addresses; the node
// must be running with Config.Join) or removes one ("remove"). It must be
// called on the leader and returns once the change is committed. One change
// at a time: while the previous one is not committed it fails and can be
// retried.
func (c *Cluster) ChangeMember(op string, id uint64, raftAddr, httpAddr string) error {
	info := c.g.Info()
	if info.Role != raft.Leader {
		return fmt.Errorf("not the leader (node %d is)", info.Leader)
	}
	var cc raft.ConfChange
	switch op {
	case "add":
		if raftAddr == "" {
			return errors.New("add needs the new node's raft address")
		}
		// A voter nobody can reach counts toward the majority and can stop
		// the cluster from committing: refuse an address that does not answer.
		conn, err := net.DialTimeout("tcp", raftAddr, 2*time.Second)
		if err != nil {
			return fmt.Errorf("node %d is not reachable at %s (start it with the join option first): %w", id, raftAddr, err)
		}
		conn.Close()
		ctx, _ := json.Marshal(member{Raft: raftAddr, HTTP: httpAddr})
		cc = raft.ConfChange{Type: raft.ConfAdd, ID: id, Context: ctx}
	case "remove":
		cc = raft.ConfChange{Type: raft.ConfRemove, ID: id}
	default:
		return fmt.Errorf("unknown operation %q (add or remove)", op)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.g.ProposeConf(ctx, cc); err != nil {
		return err
	}
	c.cfg.Logf("cluster: membership change %s node %d committed", op, id)
	return nil
}

// ArchiveLimit is the first log segment that may still hold entries the
// cluster has not committed: the continuous archiver (core.Options) copies
// only segments below it, so an archive never holds a batch that a deposed
// leader wrote and the cluster discarded.
func (c *Cluster) ArchiveLimit() uint32 { return c.cur.Load().Pos.Seg }
