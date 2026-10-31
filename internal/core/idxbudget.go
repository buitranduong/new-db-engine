package core

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"stask/internal/pindex"
)

// Index memory budget (docs/18 section 14, D29).
//
// staskd -index-mem-budget N gives the RAM that index structures may hold:
// primary indexes, RAM secondary indexes, the buffer pools of disk indexes
// and the trigram views (their estimate, see proj.Substr.Report). The budget
// is accounted per structure and reported (IndexMemoryReport, /v1/status
// "index_memory", /metrics, log lines). It decides nothing on its own, by
// D29: there is no auto-spill. What it does:
//
//   - refuses what would add RAM beyond it: moving an index to memory
//     (ALTER .. SET (index_storage = 'memory')) and a key arena compaction
//     that needs a second copy of the index;
//   - warns when the total reaches 80% of the budget, or when the growth of
//     the last minutes would reach it within an hour (a forecast), and says
//     when it is over, in the log and in the report, naming the largest
//     structures and the ALTER statements that would move them to disk (to be
//     run by a person);
//   - leaves creating an index alone: a RAM index created over the budget is
//     allowed and reported (refusing DDL would make existing applications fail).
//
// Sizes are estimates from node and entry counts, not heap measurements
// (cmd/memmap measures the heap); the primary index is exact for its nodes.

// ErrIndexBudget is returned when an operation would put the index memory
// over -index-mem-budget.
var ErrIndexBudget = errors.New("index memory budget exceeded")

// IndexMem is the memory of one index structure.
type IndexMem struct {
	Kind      string `json:"kind"` // primary | secondary | trigram
	Table     string `json:"table"`
	Name      string `json:"name"`
	Storage   string `json:"storage"` // memory | disk
	RAMBytes  int64  `json:"ram_bytes"`
	DiskBytes int64  `json:"disk_bytes"`
	Entries   int64  `json:"entries"`
}

// BudgetReport is the state of the index memory.
type BudgetReport struct {
	BudgetBytes     int64      `json:"budget_bytes"` // 0: no budget set
	RAMBytes        int64      `json:"ram_bytes"`
	DiskBytes       int64      `json:"disk_bytes"`
	Fraction        float64    `json:"fraction,omitempty"`
	Status          string     `json:"status"` // off | ok | warn | over
	GrowthPerSec    float64    `json:"growth_bytes_per_s"`
	ForecastSeconds float64    `json:"forecast_seconds_to_budget"` // -1: not growing
	Structures      []IndexMem `json:"structures"`
	Advice          []string   `json:"advice,omitempty"`
}

type memSource struct {
	name string
	fn   func() []IndexMem
}

// AddIndexMemSource registers a source of index memory rows outside the core
// (the trigram view).
func (e *Engine) AddIndexMemSource(name string, fn func() []IndexMem) {
	e.budget.mu.Lock()
	e.budget.sources = append(e.budget.sources, memSource{name, fn})
	e.budget.mu.Unlock()
}

type budgetState struct {
	mu      sync.Mutex
	sources []memSource
	samples []budgetSample
	last    string
	lastLog time.Time
}

type budgetSample struct {
	t   time.Time
	ram int64
}

func treeMem(tr *tree) IndexStats {
	t := &TableState{}
	t.idx.Store(tr)
	return t.IndexStats()
}

// structures lists the index structures with their memory.
func (e *Engine) structures() []IndexMem {
	var out []IndexMem
	for _, t := range e.UserTables() {
		st := t.IndexStats()
		out = append(out, IndexMem{Kind: "primary", Table: t.Schema().Name, Name: t.Schema().Name + "_pkey", Storage: "memory",
			RAMBytes: st.Bytes, Entries: int64(st.Keys)})
		for _, si := range t.secList() {
			m := IndexMem{Kind: "secondary", Table: t.Schema().Name, Name: si.name, Storage: si.storageOf()}
			if rd, ok, _ := si.openRead(); ok {
				m.Entries = int64(rd.len())
				if rd.d != nil {
					ps := rd.d.tree.Stats()
					m.DiskBytes = int64(ps.FilePages) * pindex.PageSize
					m.RAMBytes = min(e.sidxCache(), m.DiskBytes)
				} else {
					m.RAMBytes = treeMem(rd.tr).Bytes
				}
				rd.close()
			}
			out = append(out, m)
		}
	}
	e.budget.mu.Lock()
	src := append([]memSource(nil), e.budget.sources...)
	e.budget.mu.Unlock()
	for _, s := range src {
		out = append(out, s.fn()...)
	}
	return out
}

// IndexMemoryReport computes the report now.
func (e *Engine) IndexMemoryReport() BudgetReport {
	r := BudgetReport{BudgetBytes: e.opts.IndexMemBudget, Structures: e.structures(), ForecastSeconds: -1}
	for _, m := range r.Structures {
		r.RAMBytes += m.RAMBytes
		r.DiskBytes += m.DiskBytes
	}
	sort.SliceStable(r.Structures, func(i, j int) bool { return r.Structures[i].RAMBytes > r.Structures[j].RAMBytes })
	e.budget.mu.Lock()
	r.GrowthPerSec = growth(e.budget.samples)
	e.budget.mu.Unlock()
	if r.BudgetBytes <= 0 {
		r.Status = "off"
		return r
	}
	r.Fraction = float64(r.RAMBytes) / float64(r.BudgetBytes)
	if r.GrowthPerSec > 0 && r.RAMBytes < r.BudgetBytes {
		r.ForecastSeconds = float64(r.BudgetBytes-r.RAMBytes) / r.GrowthPerSec
	}
	switch {
	case r.Fraction >= 1:
		r.Status = "over"
	case r.Fraction >= 0.8 || (r.ForecastSeconds >= 0 && r.ForecastSeconds <= 3600):
		r.Status = "warn"
	default:
		r.Status = "ok"
	}
	if r.Status != "ok" {
		need := r.RAMBytes - r.BudgetBytes*8/10 // bring it under 80%
		if need <= 0 {
			need = r.RAMBytes / 10
		}
		for _, m := range r.Structures {
			if need <= 0 {
				break
			}
			if m.Kind == "primary" || m.Storage != "memory" || m.RAMBytes == 0 {
				continue
			}
			r.Advice = append(r.Advice, fmt.Sprintf("ALTER INDEX %s SET (index_storage = 'disk')  -- frees about %d MiB of RAM on %s", m.Name, m.RAMBytes>>20, m.Table))
			need -= m.RAMBytes
		}
	}
	return r
}

// growth is the least-squares slope of the samples, in bytes per second.
func growth(s []budgetSample) float64 {
	if len(s) < 4 {
		return 0
	}
	t0 := s[0].t
	var sx, sy, sxx, sxy float64
	for _, p := range s {
		x := p.t.Sub(t0).Seconds()
		y := float64(p.ram)
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
	}
	n := float64(len(s))
	d := n*sxx - sx*sx
	if d == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / d
}

// budgetBlocks reports whether adding extra bytes of RAM would put the index
// memory over the budget.
func (e *Engine) budgetBlocks(extra int64) bool {
	if e.opts.IndexMemBudget <= 0 {
		return false
	}
	var ram int64
	for _, m := range e.structures() {
		ram += m.RAMBytes
	}
	return ram+extra > e.opts.IndexMemBudget
}

// checkBudgetFor returns ErrIndexBudget (wrapped with the numbers) if moving
// a structure of the given size to memory does not fit.
func (e *Engine) checkBudgetFor(what string, extra int64) error {
	if e.opts.IndexMemBudget <= 0 {
		return nil
	}
	if e.budgetBlocks(extra) {
		r := e.IndexMemoryReport()
		return fmt.Errorf("%w: %s needs about %d MiB of RAM, index memory is %d of %d MiB (-index-mem-budget); keep it on disk or raise the budget",
			ErrIndexBudget, what, extra>>20, r.RAMBytes>>20, r.BudgetBytes>>20)
	}
	return nil
}

// budgetMonitor samples the index memory and logs what a person should know.
func (e *Engine) budgetMonitor() {
	defer e.wg.Done()
	every := e.opts.BudgetSample
	if every <= 0 {
		every = 10 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
		}
		e.budgetTick()
	}
}

func (e *Engine) budgetTick() {
	var ram int64
	for _, m := range e.structures() {
		ram += m.RAMBytes
	}
	e.budget.mu.Lock()
	e.budget.samples = append(e.budget.samples, budgetSample{time.Now(), ram})
	if n := len(e.budget.samples); n > 36 {
		e.budget.samples = append([]budgetSample(nil), e.budget.samples[n-36:]...)
	}
	e.budget.mu.Unlock()
	r := e.IndexMemoryReport()
	e.budget.mu.Lock()
	changed := r.Status != e.budget.last
	repeat := time.Since(e.budget.lastLog) > 10*time.Minute
	if changed || (r.Status != "ok" && repeat) {
		e.budget.last, e.budget.lastLog = r.Status, time.Now()
		e.budget.mu.Unlock()
		msg := fmt.Sprintf("index memory %s: %d MiB of %d MiB budget (%.0f%%), disk %d MiB", r.Status, r.RAMBytes>>20, r.BudgetBytes>>20, r.Fraction*100, r.DiskBytes>>20)
		if r.ForecastSeconds >= 0 {
			msg += fmt.Sprintf("; growing %.1f KiB/s, budget reached in about %s", r.GrowthPerSec/1024, (time.Duration(r.ForecastSeconds) * time.Second).Round(time.Minute))
		}
		e.logf("%s", msg)
		for i, m := range r.Structures {
			if i >= 3 {
				break
			}
			e.logf("index memory: largest %s %s.%s %s %d MiB", m.Kind, m.Table, m.Name, m.Storage, m.RAMBytes>>20)
		}
		if r.Status != "ok" {
			for _, a := range r.Advice {
				e.logf("index memory: advice (not applied, D29 no auto-spill): %s", a)
			}
		}
		e.BudgetAlerts.Add(1)
		return
	}
	e.budget.mu.Unlock()
}
