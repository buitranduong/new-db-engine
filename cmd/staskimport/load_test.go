package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dump is the table and history content in a comparable form (updated_at and
// history ids differ between runs).
func (r *rig) dump() string {
	r.t.Helper()
	var b strings.Builder
	for _, q := range []string{
		"SELECT number, carrier, status, price FROM sims ORDER BY number",
		"SELECT number, source, old_carrier, new_carrier, old_status, new_status, old_price, new_price FROM sim_history ORDER BY number, new_price, old_price",
	} {
		rs, err := r.conn().Query(context.Background(), q)
		if err != nil {
			r.t.Fatal(err)
		}
		for rs.Next() {
			v, err := rs.Values()
			if err != nil {
				r.t.Fatal(err)
			}
			fmt.Fprintln(&b, v...)
		}
		rs.Close()
	}
	return b.String()
}

// Every load path gives the same table and history for the same two days
// (inserts, changes, kept values, invalid rows, repeated keys).
func TestLoadModesAgree(t *testing.T) {
	var want string
	for _, mode := range []string{loadInsert, loadBinary, loadText} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t)
			f1 := filepath.Join(r.dir, "d1.csv")
			writeCSV(t, f1, gen(3000, func(i int, x *simRow) {
				if i%97 == 0 {
					x.status = "" // NULL status
				}
			}))
			s := r.run(f1, func(o *options) { o.load = mode; o.batch = 700 })
			if s.Counts.Inserted != 3000 || s.Load != mode {
				t.Fatalf("day1 %+v load=%s", s.Counts, s.Load)
			}
			r.exec("UPDATE sims SET status = 'sold' WHERE number < '0900000100'")
			f2 := filepath.Join(r.dir, "d2.csv")
			writeCSV(t, f2, gen(3300, func(i int, x *simRow) {
				if i%5 == 0 {
					x.price += 3
				}
			}), []string{"0900000001", "mobifone", "paid", "42"}, []string{"1200000001", "viettel", "new", "5"})
			s = r.run(f2, func(o *options) { o.load = mode; o.batch = 700 })
			if s.Counts.Inserted != 300 || s.Counts.Invalid != 1 || s.Counts.Updated == 0 {
				t.Fatalf("day2 %+v", s.Counts)
			}
			got := r.dump()
			if want == "" {
				want = got
			} else if got != want {
				t.Fatalf("%s result differs from the insert path", mode)
			}
		})
	}
}

// A server that refuses binary COPY (or all COPY) moves the run down a step and
// the file still loads completely, with the right history.
func TestLoadFallback(t *testing.T) {
	for _, c := range []struct{ refuse, want string }{{loadBinary, loadText}, {"all", loadInsert}} {
		t.Run(c.refuse, func(t *testing.T) {
			r := newRig(t)
			f := filepath.Join(r.dir, "d.csv")
			writeCSV(t, f, gen(2500, nil))
			s := r.run(f, func(o *options) { o.batch = 500; o.testRefuse = c.refuse })
			if s.Load != c.want || s.Counts.Inserted != 2500 || !s.Complete {
				t.Fatalf("%+v load=%s", s.Counts, s.Load)
			}
			if n := r.count("SELECT count(*) FROM sims"); n != 2500 {
				t.Fatalf("rows %d", n)
			}
			if n := r.count("SELECT count(DISTINCT number) FROM sim_history"); n != 2500 {
				t.Fatalf("history %d", n)
			}
		})
	}
}

// A row written by someone else between the lookup and the COPY makes the COPY fail
// with a duplicate key; the batch is then redone row by row and nothing is lost.
func TestLoadCopyDuplicateFallsBackToRows(t *testing.T) {
	r := newRig(t)
	f := filepath.Join(r.dir, "d.csv")
	writeCSV(t, f, gen(200, nil))
	once := false
	hook := func() {
		if !once {
			once = true
			r.exec("INSERT INTO sims (number, carrier, status, price, updated_at) VALUES ('0900000007','viettel','paid',1,now())")
		}
	}
	var out, errb bytes.Buffer
	testHookAfterLookup = hook
	defer func() { testHookAfterLookup = nil }()
	s, err := run(context.Background(), options{url: r.url, specPath: r.spec, batch: 100}, f, &out, &errb)
	if err != nil {
		t.Fatalf("%v\n%s%s", err, out.String(), errb.String())
	}
	if s.Counts.Failed != 0 || s.Counts.Inserted != 199 || s.Counts.Updated != 1 {
		t.Fatalf("%+v", s.Counts)
	}
	if n := r.count("SELECT count(*) FROM sims"); n != 200 {
		t.Fatalf("rows %d", n)
	}
	if n := r.count("SELECT count(*) FROM sims WHERE number = '0900000007' AND status = 'new'"); n != 1 {
		t.Fatal("the row written meanwhile was not updated by the file")
	}
}

func mustSpec(t *testing.T, path string) *Spec {
	t.Helper()
	sp, _, err := LoadSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestLoadFlagValidation(t *testing.T) {
	r := newRig(t)
	f := filepath.Join(r.dir, "d.csv")
	writeCSV(t, f, gen(3, nil))
	_, err := run(context.Background(), options{url: r.url, specPath: r.spec, batch: 10, load: "bulk"}, f, os.Stdout, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "-load") {
		t.Fatalf("err=%v", err)
	}
	_ = time.Second
}

func TestTextCopyEncoding(t *testing.T) {
	got := string(textCopy([][]any{{"a\tb\\c\nd\r", nil, int64(-5), 1.5, true, time.Date(2024, 1, 2, 3, 4, 5, 600000000, time.UTC)}}))
	want := "a\\tb\\\\c\\nd\\r\t\\N\t-5\t1.5\tt\t2024-01-02 03:04:05.6+00\n"
	if got != want {
		t.Fatalf("%q != %q", got, want)
	}
}
