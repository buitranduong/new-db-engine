package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"stask/internal/reconcile"
)

// POST /v1/admin/reconcile is an admin call: no credential 401, a read-only
// token 403, a full token runs the comparison and answers the report; bad
// parameters and unknown tables are 400.
func TestReconcileEndpoint(t *testing.T) {
	srv, _ := authServer(t, SecurityConfig{Tokens: "full", ROTokens: "reader"})
	hc := srv.Client()
	u := srv.URL + "/v1/admin/reconcile"
	for _, c := range []struct {
		auth string
		want int
	}{{"", 401}, {"Bearer reader", 403}} {
		if r := do(t, hc, "POST", u, c.auth, ""); r.StatusCode != c.want {
			t.Errorf("auth=%q: %d, want %d", c.auth, r.StatusCode, c.want)
		}
	}
	do(t, hc, "POST", srv.URL+"/v1/tables", "Bearer full", `{"name":"t","columns":[{"name":"id","type":"int"},{"name":"v","type":"string"}],"key":"id","columnar":true}`)
	do(t, hc, "POST", srv.URL+"/v1/write", "Bearer full", `{"ops":[{"table":"t","row":{"id":1,"v":"a"}},{"table":"t","row":{"id":2,"v":"b"}}]}`)
	req, _ := http.NewRequest("POST", u+"?sample=1", nil)
	req.Header.Set("Authorization", "Bearer full")
	r, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("full token: %d %s", r.StatusCode, b)
	}
	var rep reconcile.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Tables) != 1 || rep.Tables[0].Table != "t" || len(rep.Tables[0].Structures) == 0 {
		t.Fatalf("report: %s", b)
	}
	for _, q := range []string{"?sample=x", "?table=nope", "?slot=nope"} {
		req, _ := http.NewRequest("POST", u+q, nil)
		req.Header.Set("Authorization", "Bearer full")
		r, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 400 || !strings.Contains(string(body), "error") {
			t.Errorf("%s: %d %s", q, r.StatusCode, body)
		}
	}
}
