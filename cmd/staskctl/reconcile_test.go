package main

import (
	"testing"
)

// staskctl reconcile against a real staskd: a table with a columnar view is
// IN_SYNC (exit status 0 means cmdReconcile returns nil); an unknown table
// and an unknown slot are errors the operator sees.
func TestReconcileCommand(t *testing.T) {
	bin := buildStaskd(t)
	addr, stop := startNode(t, bin, t.TempDir())
	defer stop()
	u := "http://" + addr
	httpJSON(t, "POST", u+"/v1/tables", `{"name":"t","columns":[{"name":"id","type":"int"},{"name":"v","type":"string"}],"key":"id","columnar":true}`)
	httpJSON(t, "POST", u+"/v1/write", `{"ops":[{"table":"t","row":{"id":1,"v":"a"}},{"table":"t","row":{"id":2,"v":"b"}}]}`)
	for _, args := range [][]string{
		{"-url", u},
		{"-url", u, "-sample", "1"},
		{"-url", u, "-full", "-table", "t", "-json"},
	} {
		if err := cmdReconcile(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if err := cmdReconcile([]string{"-url", u, "-table", "nope"}); err == nil {
		t.Fatal("unknown table accepted")
	}
	if err := cmdReconcile([]string{"-url", u, "-cdc-slot", "nope"}); err == nil {
		t.Fatal("unknown slot accepted")
	}
}
