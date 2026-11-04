package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stask/internal/core"
	"stask/internal/planner"
)

// A follower that does not know the table yet (a lagging member, or one
// refilling an empty directory) answers a write with "not the leader" (503
// and the header clients and the router retry on), not 404 "no such table",
// which reads as a final failure (cluster TestClusterHistoryStore).
func TestWriteOnFollowerWithoutTable(t *testing.T) {
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := httptest.NewServer(New(&planner.Platform{E: e}, nil).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/write", "application/json",
		strings.NewReader(`{"ops":[{"op":"put","table":"t","row":{"id":"a","v":1}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(HeaderNotLeader) == "" {
		t.Fatalf("write on a follower without the table: %d, %s=%q; want 503 and the header",
			resp.StatusCode, HeaderNotLeader, resp.Header.Get(HeaderNotLeader))
	}
}
