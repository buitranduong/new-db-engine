package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/schema"
	"stask/internal/wal"
)

func setup(t *testing.T) (*planner.Platform, *schema.Table) {
	t.Helper()
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 8 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	p := planner.Start(e)
	if _, err := e.CreateTable(schema.Table{Name: "acct", Key: "id", Columnar: true,
		Substr: []schema.SubstrIndex{{Name: "acct_note", Col: "note"}},
		Columns: []schema.Column{{Name: "id", Type: "int"}, {Name: "region", Type: "string"}, {Name: "bal", Type: "int"},
			{Name: "rate", Type: "float"}, {Name: "note", Type: "text"}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.CreateIndex("acct", schema.IndexDef{Name: "acct_region", Columns: []string{"region"}}); err != nil {
		t.Fatal(err)
	}
	var last uint64
	for i := 0; i < 3000; i += 100 {
		var ops []core.WriteOp
		for j := i; j < i+100; j++ {
			ops = append(ops, core.WriteOp{Table: "acct", Row: schema.Row{{I: int64(j)}, {S: fmt.Sprintf("r%d", j%5)}, {I: int64(j * 3)}, {F: float64(j) / 7}, {S: fmt.Sprintf("note number %d", j)}}})
		}
		if last, err = e.Write(ops); err != nil {
			t.Fatal(err)
		}
	}
	waitViews(t, p, last)
	ts, _ := e.Table("acct")
	return p, ts.Schema()
}

func waitViews(t *testing.T, p *planner.Platform, lsn uint64) {
	t.Helper()
	for _, f := range []*core.Follower{p.ColF, p.SubF, p.SF, p.TSF} {
		if !f.WaitApplied(lsn, 20*time.Second) {
			t.Fatalf("%s did not reach lsn %d", f.Name(), lsn)
		}
	}
}

func run(t *testing.T, p *planner.Platform, o Options) *Report {
	t.Helper()
	rep, err := Run(context.Background(), Env{P: p}, o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func find(t *testing.T, rep *Report, name string) StructReport {
	t.Helper()
	for _, s := range rep.Tables[0].Structures {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no structure %q in %+v", name, rep.Tables[0].Structures)
	return StructReport{}
}

func TestInSync(t *testing.T) {
	p, _ := setup(t)
	for _, sample := range []int{0, 1, 7} {
		rep := run(t, p, Options{Sample: sample})
		if rep.Verdict != StInSync {
			t.Fatalf("sample %d: verdict %s: %+v", sample, rep.Verdict, rep.Tables)
		}
		col := find(t, rep, "columnar")
		if sample == 1 && (col.CoreRows != 3000 || col.ViewRows != 3000 || col.KeysChecked != 3000 || col.CoreSum != col.ViewSum) {
			t.Fatalf("columnar: %+v", col)
		}
		if sample > 0 {
			ix := find(t, rep, "index:acct_region")
			if ix.Status != StInSync || ix.KeysChecked == 0 {
				t.Fatalf("index: %+v", ix)
			}
		}
	}
}

// inject applies a fabricated record to a view only, as a lost or corrupted
// update would leave it.
func inject(t *testing.T, p *planner.Platform, c core.Consumer, sch *schema.Table, lsn uint64, ops ...wal.Op) {
	t.Helper()
	c.Apply(p.E, &wal.Record{Kind: wal.KindTx, LSN: lsn, TS: time.Now().UnixNano(), Ops: ops})
	c.Flush(lsn)
}

func put(sch *schema.Table, r schema.Row) wal.Op {
	return wal.Op{Kind: wal.OpPut, Table: sch.ID, Key: []byte(sch.KeyOf(r)), Val: sch.Encode(nil, r)}
}

func TestDivergenceFound(t *testing.T) {
	p, sch := setup(t)
	lsn := p.E.DurableLSN()
	changed := schema.Row{{I: 5}, {S: "r0"}, {I: 777}, {F: 5.0 / 7}, {S: "note number 5"}}
	extra := schema.Row{{I: 99999}, {S: "r1"}, {I: 1}, {F: 1}, {S: "ghost"}}
	inject(t, p, p.Col, sch, lsn, put(sch, changed), put(sch, extra), wal.Op{Kind: wal.OpDel, Table: sch.ID, Key: []byte("7")})
	rep := run(t, p, Options{Sample: 1})
	col := find(t, rep, "columnar")
	if col.Status != StDiverged || col.Diff == nil {
		t.Fatalf("columnar: %+v", col)
	}
	d := col.Diff
	if d.Count != 3 || d.Changed != 1 || d.ExtraInView != 1 || d.MissingInView != 1 || d.FirstKey != "5" || d.LastKey != "99999" {
		t.Fatalf("diff: %+v", d)
	}
	if rep.Verdict != StDiverged {
		t.Fatalf("verdict %s", rep.Verdict)
	}
	// the other structures are untouched
	if s := find(t, rep, "trigram(note)"); s.Status != StInSync {
		t.Fatalf("trigram: %+v", s)
	}
	// counts alone notice the extra and the missing row only if they differ in number: here they cancel out
	if rep0 := run(t, p, Options{Sample: 0}); find(t, rep0, "columnar").Status != StInSync {
		t.Fatalf("counts-only check should not see an equal count: %+v", find(t, rep0, "columnar"))
	}
	// a sample of 1/64 of the keys sees a difference only if a differing key is in the sample, never a false alarm
	rep64 := run(t, p, Options{Sample: 64})
	if s := find(t, rep64, "columnar"); s.Status != StInSync && s.Status != StDiverged {
		t.Fatalf("sampled: %+v", s)
	}

	// the trigram view
	inject(t, p, p.Sub, sch, lsn, put(sch, schema.Row{{I: 8}, {S: "r3"}, {I: 24}, {F: 8.0 / 7}, {S: "tampered note"}}))
	rep = run(t, p, Options{Sample: 1})
	if s := find(t, rep, "trigram(note)"); s.Status != StDiverged || s.Diff == nil || s.Diff.Count != 1 || s.Diff.FirstKey != "8" || s.Diff.Changed != 1 {
		t.Fatalf("trigram: %+v", s)
	}
}

func TestBehindIsNotDivergence(t *testing.T) {
	p, sch := setup(t)
	release := p.Sub.Hold()
	var last uint64
	for i := 0; i < 20; i++ {
		var err error
		last, err = p.E.Write([]core.WriteOp{{Table: "acct", Row: schema.Row{{I: int64(5000 + i)}, {S: "r1"}, {I: 1}, {F: 1}, {S: "later"}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !p.ColF.WaitApplied(last, 10*time.Second) {
		t.Fatal("columnar did not catch up")
	}
	rep := run(t, p, Options{Sample: 1})
	tri := find(t, rep, "trigram(note)")
	if tri.Status != StBehind || tri.BehindRecords == 0 {
		t.Fatalf("held view should be BEHIND: %+v", tri)
	}
	if tri.CompareLSN != tri.AppliedLSN || tri.Diff != nil {
		t.Fatalf("a view that is behind but consistent must compare equal at its own LSN: %+v", tri)
	}
	if find(t, rep, "columnar").Status != StInSync || rep.Verdict != StBehind {
		t.Fatalf("verdict %s: %+v", rep.Verdict, rep.Tables)
	}
	release()
	waitViews(t, p, last)
	if rep := run(t, p, Options{Sample: 1}); rep.Verdict != StInSync {
		t.Fatalf("after release: %s %+v", rep.Verdict, rep.Tables)
	}
	_ = sch
}

func TestSlot(t *testing.T) {
	p, _ := setup(t)
	if _, err := p.E.CreateSlot("s1", "test_decoding"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := p.E.Write([]core.WriteOp{{Table: "acct", Row: schema.Row{{I: int64(8000 + i)}, {S: "r1"}, {I: 1}, {F: 1}, {S: "x"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	rep := run(t, p, Options{Slot: "s1", Tables: []string{"acct"}})
	if rep.Slot == nil || rep.Slot.Status != StBehind || len(rep.Slot.Tables) != 1 || rep.Slot.Tables[0].PendingOp != 5 || rep.Slot.Tables[0].PendingTx != 5 {
		t.Fatalf("slot: %+v", rep.Slot)
	}
	_, dpos, _ := p.E.DurablePoint()
	if _, err := p.E.ConfirmSlot("s1", dpos); err != nil {
		t.Fatal(err)
	}
	rep = run(t, p, Options{Slot: "s1"})
	if rep.Slot.Status != StInSync || rep.Slot.Tables[0].PendingOp != 0 {
		t.Fatalf("slot after confirm: %+v", rep.Slot)
	}
	if _, err := Run(context.Background(), Env{P: p}, Options{Slot: "nope"}); err == nil {
		t.Fatal("unknown slot accepted")
	}
}

func TestUnknownTable(t *testing.T) {
	p, _ := setup(t)
	if _, err := Run(context.Background(), Env{P: p}, Options{Tables: []string{"nope"}}); err == nil {
		t.Fatal("unknown table accepted")
	}
}
