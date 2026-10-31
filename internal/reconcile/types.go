// Package reconcile compares the core (the log and its row versions, the one
// source of truth) with every structure derived from it: the columnar view,
// the trigram view, the search and time-series views, the secondary indexes
// and the history store, plus the position of a CDC slot. For each table and
// structure it reports one of
//
//	IN_SYNC       applied up to the durable LSN and equal to the core
//	BEHIND        applied < durable: by how many records and how many ms
//	              (and, when content was checked, whether what it holds is
//	              equal to the core AT ITS OWN applied LSN)
//	DIVERGED      differs from the core at the same LSN, with the first and
//	              last differing key and counts of missing/extra/changed keys
//	SUSPECT       differs in every one of several passes while the view kept
//	              moving, so no pass could prove it; the keys are listed
//	INCONCLUSIVE  could not be decided (writes too fast, schema changed)
//	NOT_READY     the view is still bootstrapping
//	UNCHECKED     lag only (no content comparison for this structure)
//
// The content check is an order-independent checksum: every row (or a
// deterministic 1/N sample of the keys) contributes a 64-bit hash of its key
// and of the columns the structure holds; the sums are kept per hash bucket
// of the key. Equal buckets prove equality of the sampled rows; a differing
// bucket is re-read to find the exact keys. All reads are bounded chunks of
// a few thousand rows, so no table lock is held for long (the columnar
// view's table lock is taken per chunk, never for a whole scan).
package reconcile

import "time"

// Options of a run.
type Options struct {
	Tables  []string      // empty: every user table
	Sample  int           // 0: LSNs and counts only (cheap); 1: every key; N: keys whose hash is a multiple of N
	Retries int           // extra passes when the structure moves under the check (default 3)
	Pause   time.Duration // sleep between chunks, to go easy on a busy node
	Chunk   int           // rows per chunk (default 2048)
	Slot    string        // also compare this CDC slot with each table
	Limit   int           // most differing keys listed per structure (default 20)
}

// Report is the result of one run on one node.
type Report struct {
	Node       string        `json:"node,omitempty"`
	At         string        `json:"at"`
	DurableLSN uint64        `json:"durable_lsn"`
	Sample     int           `json:"sample"`
	Verdict    string        `json:"verdict"` // worst status over everything
	DurationMs int64         `json:"duration_ms"`
	Tables     []TableReport `json:"tables"`
	Slot       *SlotReport   `json:"slot,omitempty"`
	Notes      []string      `json:"notes,omitempty"`
}

type TableReport struct {
	Table      string         `json:"table"`
	CoreRows   int64          `json:"core_rows"`
	Structures []StructReport `json:"structures"`
}

// StructReport is the state of one derived structure of one table.
type StructReport struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	Detail        string `json:"detail,omitempty"`
	AppliedLSN    uint64 `json:"applied_lsn"`
	DurableLSN    uint64 `json:"durable_lsn"`
	BehindRecords uint64 `json:"behind_records"`
	BehindMs      int64  `json:"behind_ms"`
	CoreRows      int64  `json:"core_rows"`
	ViewRows      int64  `json:"view_rows"`
	Checked       string `json:"checked"`               // what was compared
	KeysChecked   int64  `json:"keys_checked"`          // keys in the compared sample (core side)
	CompareLSN    uint64 `json:"compare_lsn,omitempty"` // the LSN the core was read at
	CoreSum       string `json:"core_sum,omitempty"`
	ViewSum       string `json:"view_sum,omitempty"`
	Diff          *Diff  `json:"diff,omitempty"`
}

// Diff lists how a structure differs from the core.
type Diff struct {
	FirstKey      string   `json:"first_key,omitempty"`
	LastKey       string   `json:"last_key,omitempty"`
	Count         int      `json:"count"`
	MissingInView int      `json:"missing_in_view"` // in the core, not in the structure
	ExtraInView   int      `json:"extra_in_view"`   // in the structure, not in the core
	Changed       int      `json:"changed"`         // in both with different content
	Examples      []DiffEx `json:"examples,omitempty"`
	Buckets       int      `json:"differing_buckets,omitempty"`
}

type DiffEx struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
}

// SlotReport compares a logical replication slot with the log and tables.
type SlotReport struct {
	Name         string      `json:"name"`
	Plugin       string      `json:"plugin"`
	Status       string      `json:"status"` // IN_SYNC, BEHIND, INVALID, LOG_MISSING
	Detail       string      `json:"detail,omitempty"`
	ConfirmedLSN uint64      `json:"confirmed_lsn"`
	DurableLSN   uint64      `json:"durable_lsn"`
	BehindRecs   uint64      `json:"behind_records"`
	LagBytes     int64       `json:"lag_bytes"`
	Invalid      string      `json:"invalid,omitempty"`
	Tables       []SlotTable `json:"tables,omitempty"`
	Truncated    bool        `json:"truncated,omitempty"`
}

// SlotTable: what the slot has not delivered yet for one table.
type SlotTable struct {
	Table     string `json:"table"`
	PendingTx int64  `json:"pending_tx"`
	PendingOp int64  `json:"pending_ops"`
	OldestMs  int64  `json:"oldest_pending_age_ms"`
	Status    string `json:"status"`
}

const (
	StInSync       = "IN_SYNC"
	StBehind       = "BEHIND"
	StDiverged     = "DIVERGED"
	StSuspect      = "SUSPECT"
	StInconclusive = "INCONCLUSIVE"
	StNotReady     = "NOT_READY"
	StUnchecked    = "UNCHECKED"
)

var rank = map[string]int{StInSync: 0, StUnchecked: 0, StBehind: 1, StInconclusive: 2, StNotReady: 2, StSuspect: 3, StDiverged: 4, "INVALID": 4, "LOG_MISSING": 4}

// Worse returns the more serious of two statuses.
func Worse(a, b string) string {
	if rank[b] > rank[a] {
		return b
	}
	return a
}
