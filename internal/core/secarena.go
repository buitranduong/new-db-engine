package core

import (
	"errors"
)

// Reclaiming the key arena of a RAM secondary index (the counterpart of
// arenacompact.go for the primary index; docs/14 appendix importbin).
//
// Entries longer than the inline key size keep their key bytes in the index's
// arena, which only grows: when the sweeper (or a conversion) removes an
// entry, its key bytes stay behind. The compaction rebuilds the index into a
// fresh arena without stopping writers, with the machinery of the online
// storage conversion (secdisk.go):
//
//  1. under si.mu take the current tree (immutable, O(1)) and start recording
//     every later mutation of the index in si.mig;
//  2. build a new tree from it, copying the long keys into a new arena (no
//     lock held);
//  3. replay the recorded mutations onto the new tree in rounds, and under
//     si.mu apply the last few and publish the tree (installMemory, inside the
//     seqlock that readers of openRead use).
//
// Readers that hold the old tree keep it and its arena until they finish. A
// disk index has no arena (its keys are pages of the file; pindex reuses freed
// pages), so only RAM indexes are compacted. It runs when at least half of
// the arena's key bytes are dead and the dead bytes pass
// Options.ArenaCompactMinDead, and not when the memory budget cannot hold a
// second copy of the index (idxbudget.go). Peak RAM = one more copy of the
// index. It is serialised with storage conversions through si.conv.

// CompactSecArena rebuilds the RAM index of table into a fresh key arena and
// returns the arena bytes freed.
func (e *Engine) CompactSecArena(table, index string) (int64, error) {
	t, err := e.Table(table)
	if err != nil {
		return 0, err
	}
	si := t.secNamed(index)
	if si == nil {
		return 0, errors.New("index " + index + " does not exist")
	}
	return e.compactSec(si)
}

func (e *Engine) compactSec(si *secIndex) (int64, error) {
	if e.opts.Replica {
		return 0, errArenaBusy
	}
	si.conv.Lock()
	defer si.conv.Unlock()
	si.mu.Lock()
	if si.retired.Load() || si.dk.Load() != nil || si.mig != nil {
		si.mu.Unlock()
		return 0, errArenaBusy
	}
	src := si.tr.Load()
	before := src.ar.Bytes()
	si.mig = e.newMig(si)
	si.mu.Unlock()
	abort := func() {
		si.mu.Lock()
		si.discardMig()
		si.mu.Unlock()
	}
	ar := &keyArena{}
	b := newTreeBuilder(ar)
	n := 0
	aborted := false
	src.ascend("", func(en *entry) bool {
		if n++; n&0x3fff == 0 && e.convAborted(si) {
			aborted = true
			return false
		}
		ne := *en
		if ne.klen == longKey {
			ne.ikey = ar.ikeyOf(src.keyOf(en))
		}
		b.add(ne)
		return true
	})
	if aborted {
		abort()
		return 0, errConvAborted
	}
	nt := b.finish()
	e.sidxStep("secarena-built")
	if err := e.installMemory(si, nt, ar, func() {}); err != nil {
		return 0, err
	}
	freed := before - ar.Bytes()
	e.SecArenaCompactions.Add(1)
	e.SecArenaReclaimed.Add(max(freed, 0))
	return freed, nil
}

// compactSecArenas is one pass of the background compactor over the RAM
// secondary indexes.
func (e *Engine) compactSecArenas(min int64) {
	for _, t := range e.UserTables() {
		for _, si := range t.secList() {
			if si.dk.Load() != nil || si.retired.Load() {
				continue
			}
			tr := si.tr.Load()
			dead := tr.ar.Dead.Load()
			if dead < min || dead*2 < tr.ar.Live.Load() {
				continue
			}
			if e.budgetBlocks(int64(tr.Len()) * 45) { // the copy has to fit
				continue
			}
			if freed, err := e.compactSec(si); err != nil {
				if !errors.Is(err, errArenaBusy) && !errors.Is(err, errConvAborted) && !errors.Is(err, errIndexGone) {
					e.logf("key arena of index %s: %v", si.name, err)
				}
			} else {
				e.logf("key arena of index %s compacted: %d bytes freed", si.name, freed)
			}
		}
	}
}
