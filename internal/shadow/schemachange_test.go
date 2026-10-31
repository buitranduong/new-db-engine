package shadow

import (
	"reflect"
	"strings"
	"testing"
)

func kn(cols ...shapeCol) []shapeCol { return cols }

func TestPlanSchema(t *testing.T) {
	known := kn(
		shapeCol{"id", 20, "bigint", 1}, shapeCol{"name", 1043, "character varying(10)", 2},
		shapeCol{"qty", 23, "integer", 3}, shapeCol{"old", 25, "text", 4})
	st := map[string]bool{"id": true, "name": true, "qty": true, "old": true}
	nowBase := func() map[string]srcColNow {
		return map[string]srcColNow{"id": {"bigint", 1, false}, "name": {"character varying(10)", 2, false},
			"qty": {"integer", 3, false}, "old": {"text", 4, false}}
	}
	msg := func(cols ...msgCol) []msgCol { return cols }
	id, name, qty, old := msgCol{"id", 20}, msgCol{"name", 1043}, msgCol{"qty", 23}, msgCol{"old", 25}

	t.Run("no change", func(t *testing.T) {
		ops, nk, err := planSchema("t", known, msg(id, name, qty, old), st, nowBase())
		if err != nil || len(ops) != 0 || !shapeEqual(nk, known) {
			t.Fatalf("%v %v %v", ops, nk, err)
		}
	})
	t.Run("add column with a default is backfilled", func(t *testing.T) {
		now := nowBase()
		now["flag"] = srcColNow{"boolean", 5, true}
		ops, nk, err := planSchema("t", known, msg(id, name, qty, old, msgCol{"flag", 16}), st, now)
		want := []schemaOp{{Kind: "add", Col: "flag", Type: "boolean", Backfill: true}}
		if err != nil || !reflect.DeepEqual(ops, want) || len(nk) != 5 {
			t.Fatalf("%+v %v", ops, err)
		}
		if got := ops[0].sql("t"); got != `ALTER TABLE "t" ADD COLUMN "flag" boolean` {
			t.Fatal(got)
		}
	})
	t.Run("drop column", func(t *testing.T) {
		ops, _, err := planSchema("t", known, msg(id, name, qty), st, nowBase())
		if err != nil || len(ops) != 1 || ops[0].Kind != "drop" || ops[0].Col != "old" {
			t.Fatalf("%+v %v", ops, err)
		}
	})
	t.Run("rename (same attnum) is not drop+add", func(t *testing.T) {
		now := nowBase()
		delete(now, "old")
		now["renamed"] = srcColNow{"text", 4, false}
		ops, nk, err := planSchema("t", known, msg(id, name, qty, msgCol{"renamed", 25}), st, now)
		if err != nil || len(ops) != 1 || ops[0].Kind != "rename" || ops[0].Col != "old" || ops[0].NewName != "renamed" {
			t.Fatalf("%+v %v", ops, err)
		}
		if nk[3].Name != "renamed" || nk[3].Attnum != 4 {
			t.Fatalf("%+v", nk)
		}
	})
	t.Run("drop and add of the same type (new attnum) is not a rename", func(t *testing.T) {
		now := nowBase()
		delete(now, "old")
		now["fresh"] = srcColNow{"text", 5, false}
		ops, _, err := planSchema("t", known, msg(id, name, qty, msgCol{"fresh", 25}), st, now)
		if err != nil || len(ops) != 2 || ops[0].Kind != "drop" || ops[1].Kind != "add" {
			t.Fatalf("%+v %v", ops, err)
		}
	})
	t.Run("type change and length change", func(t *testing.T) {
		now := nowBase()
		now["qty"] = srcColNow{"bigint", 3, false}
		now["name"] = srcColNow{"character varying(40)", 2, false}
		ops, _, err := planSchema("t", known, msg(id, name, msgCol{"qty", 20}, old), st, now)
		if err != nil || len(ops) != 2 {
			t.Fatalf("%+v %v", ops, err)
		}
		got := ops[0].sql("t") + "; " + ops[1].sql("t")
		if !strings.Contains(got, `"name" TYPE varchar(40)`) && !strings.Contains(got, `"qty" TYPE bigint`) {
			t.Fatal(got)
		}
	})
	t.Run("repeat after a crash is idempotent", func(t *testing.T) {
		// STASK already has "flag" and no "old": nothing left to do
		st2 := map[string]bool{"id": true, "name": true, "qty": true, "flag": true}
		now := nowBase()
		delete(now, "old")
		now["flag"] = srcColNow{"boolean", 5, true}
		ops, _, err := planSchema("t", known, msg(id, name, qty, msgCol{"flag", 16}), st2, now)
		if err != nil || len(ops) != 0 {
			t.Fatalf("%+v %v", ops, err)
		}
	})
	t.Run("unsupported type is refused", func(t *testing.T) {
		if _, _, err := planSchema("t", known, msg(id, name, qty, old, msgCol{"geo", 600}), st, map[string]srcColNow{"geo": {"point", 5, false}}); err == nil {
			t.Fatal("accepted a point column")
		}
	})
}

func TestHashQuery(t *testing.T) {
	q := hashQuery("o rders", []string{"id", "a\"b"})
	for _, want := range []string{`from "o rders"`, `coalesce("id"::text,'<N>')`, `"a""b"`, "string_agg(h,',' order by h)"} {
		if !strings.Contains(q, want) {
			t.Errorf("%q lacks %q", q, want)
		}
	}
}
