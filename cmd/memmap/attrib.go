package main

import (
	"runtime"
	"runtime/metrics"
	"sort"
	"strings"
)

// Heap attribution. Two independent methods are reported side by side:
//
//   - delta: live heap (runtime/metrics, after two GCs) before and after each
//     stage that adds one structure. Exact for the stage, but it cannot split
//     a stage that adds several things.
//   - pprof: runtime.MemProfile inuse_space (sampled at memprofile-rate),
//     each sample assigned to a structure by the first matching frame from
//     the allocation site outwards. Splits within a stage but is only as good
//     as the rules below and the sampling.
//
// When the two disagree the delta is the number to trust and the rule list
// is what to fix.

// structure names (report order)
const (
	sPrimary = "primary index"
	sSecond  = "secondary index"
	sTrigram = "trigram view"
	sColumn  = "columnar view"
	sSearch  = "bm25 search view"
	sHistory = "history (remap/marks)"
	sWAL     = "wal mapping"
	sRing    = "commit batch ring"
	sRuntime = "engine other"
	sBench   = "benchmark itself"
	sUnknown = "unattributed"
)

var structureOrder = []string{sPrimary, sSecond, sTrigram, sColumn, sSearch, sHistory, sWAL, sRing, sRuntime, sBench, sUnknown}

type rule struct {
	sub       string // substring of the function name
	structure string
}

// rules are matched in order against each frame, innermost frame first; the
// first frame that matches any rule decides. Order inside the list therefore
// only matters for one frame matching two rules.
var rules = []rule{
	{"stask/cmd/memmap", sBench},
	{"core.(*secIndex)", sSecond}, {"core.secKey", sSecond}, {"core.(*TableState).secList", sSecond}, {"core.(*Engine).IndexScan", sSecond}, {"core.buildSec", sSecond}, {"core.(*Engine).createIndex", sSecond},
	{"stask/internal/proj.(*Substr)", sTrigram}, {"proj.(*subTable)", sTrigram}, {"proj.subCol", sTrigram},
	{"stask/internal/proj.(*Columnar)", sColumn}, {"proj.(*colTable)", sColumn}, {"proj.(*colVec)", sColumn}, {"proj.colVec", sColumn}, {"proj.newColTable", sColumn}, {"proj.(*dict)", sColumn},
	{"stask/internal/proj.(*Search)", sSearch}, {"proj.(*searchTable)", sSearch},
	{"core.(*tree)", sPrimary}, {"core.(*keyArena)", sPrimary}, {"core.treeBuilder", sPrimary}, {"core.(*treeBuilder)", sPrimary}, {"core.(*TableState).index", sPrimary}, {"core.(*TableState).head", sPrimary}, {"core.leafNode", sPrimary}, {"core.innerNode", sPrimary}, {"stask/internal/pindex", sPrimary},
	{"core.(*Engine).noteMoved", sHistory}, {"core.remapEntry", sHistory}, {"core.(*gcState)", sHistory}, {"core.(*Engine).addMark", sHistory}, {"core.(*Engine).restoreHistoryState", sHistory}, {"core.lsnMark", sHistory}, {"stask/internal/histstore", sHistory},
	// the committer keeps the last 256 installed batches (engine.go recentBatches) to check conflicts:
	// bounded, ~256 * batch ops * ~155 B, not a per-key cost; it saturates after 256 commits
	{"core.newCommitBatch", sRing}, {"core.(*Engine).prepare", sRing}, {"core.(*Engine).flushOpen", sRing}, {"core.(*Engine).installBatch", sRing}, {"core.commitBatch", sRing},
	{"stask/internal/wal.", sWAL}, {"core.segStat", sWAL}, {"core.(*segStat)", sWAL},
	{"stask/internal/core.", sRuntime}, {"stask/internal/sql.", sRuntime}, {"stask/internal/planner.", sRuntime}, {"stask/internal/schema.", sRuntime},
}

// classify returns the structure for a stack of function names, innermost
// first.
func classify(frames []string) string {
	for _, f := range frames {
		for _, r := range rules {
			if strings.Contains(f, r.sub) {
				return r.structure
			}
		}
	}
	return sUnknown
}

// Attribution is inuse_space bytes per structure plus the largest
// unattributed allocation sites.
type Attribution struct {
	Bytes   map[string]int64
	Total   int64
	Unknown []Site
	Sites   map[string]int64 // inuse bytes by innermost stask frame (for before/after diffs)
}

type Site struct {
	Func  string
	Bytes int64
}

// profileAttribution takes the current heap profile. The caller runs
// runtime.GC() first: the profile reflects the last completed cycle.
func profileAttribution() Attribution {
	var recs []runtime.MemProfileRecord
	n, _ := runtime.MemProfile(nil, true)
	for {
		recs = make([]runtime.MemProfileRecord, n+64)
		var ok bool
		if n, ok = runtime.MemProfile(recs, true); ok {
			recs = recs[:n]
			break
		}
	}
	a := Attribution{Bytes: map[string]int64{}, Sites: map[string]int64{}}
	unk := map[string]int64{}
	for i := range recs {
		r := &recs[i]
		b := r.InUseBytes()
		if b <= 0 {
			continue
		}
		pcs := r.Stack()
		var names []string
		fr := runtime.CallersFrames(pcs)
		for {
			f, more := fr.Next()
			names = append(names, f.Function)
			if !more {
				break
			}
		}
		s := classify(names)
		for _, n := range names {
			if strings.Contains(n, "stask/") {
				a.Sites[n] += b
				break
			}
		}
		a.Bytes[s] += b
		a.Total += b
		if s == sUnknown && len(names) > 0 {
			unk[names[0]] += b
		}
	}
	for f, b := range unk {
		a.Unknown = append(a.Unknown, Site{f, b})
	}
	sort.Slice(a.Unknown, func(i, j int) bool { return a.Unknown[i].Bytes > a.Unknown[j].Bytes })
	if len(a.Unknown) > 8 {
		a.Unknown = a.Unknown[:8]
	}
	return a
}

// ---- live heap ----

var heapSamples = []metrics.Sample{
	{Name: "/memory/classes/heap/objects:bytes"},
	{Name: "/memory/classes/total:bytes"},
	{Name: "/gc/heap/live:bytes"},
}

// Mem is one reading of the process memory.
type Mem struct {
	HeapObjects int64 `json:"heap_objects"` // live + not yet swept, after 2 GCs ~ live
	TotalGo     int64 `json:"go_total"`     // all memory the Go runtime holds mapped
	RSS         int64 `json:"rss"`
}

// readMem forces two GCs and reads runtime/metrics (the first GC frees,
// the second sweeps what the first marked dead lazily).
func readMem() Mem {
	runtime.GC()
	runtime.GC()
	metrics.Read(heapSamples)
	m := Mem{HeapObjects: int64(heapSamples[0].Value.Uint64()), TotalGo: int64(heapSamples[1].Value.Uint64())}
	m.RSS = rssBytes()
	return m
}

// growth returns the sites whose in-use bytes grew most between two
// profiles: what a stage actually allocated.
func growth(before, after map[string]int64, top int) []Site {
	var out []Site
	for f, b := range after {
		if d := b - before[f]; d > 0 {
			out = append(out, Site{f, d})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	if len(out) > top {
		out = out[:top]
	}
	return out
}
