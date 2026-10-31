package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/sql"
)

// A consumer that opens the stream of an invalidated slot is told with
// 410 Gone before any stream starts, not with an error line inside a 200
// (found by the G2 drill D7, docs/33).
func TestCDCStreamOfInvalidSlotIs410(t *testing.T) {
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, SlotMaxLagBytes: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := httptest.NewServer(New(&planner.Platform{E: e}, nil).Handler())
	defer srv.Close()
	sess := sql.NewSession(e)
	exec := func(q string) {
		t.Helper()
		st, err := sql.ParseOne(q)
		if err == nil {
			_, err = sess.Exec(st, nil)
		}
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE t (id BIGINT PRIMARY KEY, v TEXT)`)
	if r, err := http.Post(srv.URL+"/v1/cdc", "application/json", strings.NewReader(`{"name":"s"}`)); err != nil || r.StatusCode != http.StatusCreated {
		t.Fatalf("create: %v %v", err, r)
	}
	pad := strings.Repeat("x", 500)
	for i := 0; i < 20; i++ {
		exec(`INSERT INTO t VALUES (` + string(rune('0'+i/10)) + string(rune('0'+i%10)) + `, '` + pad + `')`)
	}
	if r, err := http.Post(srv.URL+"/v1/admin/gc", "application/json", nil); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}
	invalid := false
	for _, si := range e.Slots() {
		invalid = invalid || si.Invalid != ""
	}
	if !invalid {
		t.Fatal("the slot was not invalidated")
	}
	r, err := http.Get(srv.URL + "/v1/cdc/s/stream")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusGone {
		t.Fatalf("stream of an invalidated slot answered %d, want 410", r.StatusCode)
	}
}
