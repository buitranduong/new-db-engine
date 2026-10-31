package sql

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
)

// TestBuiltinFunctionsAgainstPG16 runs the built-in function probes of suite group 17 in process and
// compares their text results with the ones postgres:16 gave (testdata/fn_pg16.golden: one JSON
// pair [expression, result] per line, produced by `SELECT (expression)::text` on postgres:16 in
// Docker). Expressions where STASK still differs are listed in testdata/fn_pg16.known (the open
// items of docs/25 section 9, one JSON string per line); a listed expression that starts to pass
// fails the test so the list shrinks. STASK_FN_UPDATE=1 rewrites the list from the current results.
func TestBuiltinFunctionsAgainstPG16(t *testing.T) {
	f, err := os.Open("testdata/fn_pg16.golden")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	known := map[string]bool{}
	if kf, err := os.Open("testdata/fn_pg16.known"); err == nil {
		ks := bufio.NewScanner(kf)
		for ks.Scan() {
			var e string
			if json.Unmarshal(ks.Bytes(), &e) == nil {
				known[e] = true
			}
		}
		kf.Close()
	}
	var nowBad []string
	s := NewSession(newEngine(t))
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n, bad, fixed := 0, 0, 0
	for sc.Scan() {
		var pair [2]string
		if err := json.Unmarshal(sc.Bytes(), &pair); err != nil {
			t.Fatal(err)
		}
		n++
		expr, want := pair[0], pair[1]
		res, err := tryRun(s, "SELECT ("+expr+")::text")
		got := ""
		if err == nil && len(res.Rows) == 1 && len(res.Rows[0]) == 1 && !res.Rows[0][0].IsNull() {
			got = res.Rows[0][0].String()
		}
		ok := err == nil && sameFnResult(got, want)
		if !ok {
			nowBad = append(nowBad, expr)
		}
		if known[expr] {
			if ok {
				fixed++
				t.Errorf("SELECT %s now matches PostgreSQL: remove it from testdata/fn_pg16.known", expr)
			}
			continue
		}
		if !ok {
			bad++
			if bad <= 25 {
				t.Errorf("SELECT %s\n   got  %q (err %v)\n   want %q", expr, got, err, want)
			}
		}
	}
	if os.Getenv("STASK_FN_UPDATE") != "" {
		out, _ := os.Create("testdata/fn_pg16.known")
		for _, e := range nowBad {
			b, _ := json.Marshal(e)
			out.Write(append(b, '\n'))
		}
		out.Close()
		return
	}
	if n < 800 {
		t.Errorf("golden file has only %d expressions", n)
	}
	if bad > 0 {
		t.Errorf("%d of %d expressions differ from PostgreSQL 16", bad, n)
	}
	t.Logf("%d expressions, %d known differences now passing", n, fixed)
}

func sameFnResult(got, want string) bool {
	if got == want {
		return true
	}
	a, e1 := strconv.ParseFloat(got, 64)
	b, e2 := strconv.ParseFloat(want, 64)
	if e1 == nil && e2 == nil {
		return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b))
	}
	return false
}
