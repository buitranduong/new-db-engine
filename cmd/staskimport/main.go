// staskimport loads the daily .xlsx or .csv file of SIM updates into a table
// as an idempotent keyed upsert, in short bounded transactions, with a
// resumable checkpoint, a per-row error report, and history rows (old to new)
// for changed rows only. Operation: docs/20_import_runbook.md.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type options struct {
	url, specPath, ckptPath, errPath string
	batch                            int
	dry, restart, asJSON             bool
	pause, progress                  time.Duration
	crashAfter                       int // test hook: SIGKILL self after N committed batches
	crashBeforeCkpt                  bool
}

func main() {
	var o options
	flag.StringVar(&o.url, "url", os.Getenv("STASK_URL"), "connection URL (default $STASK_URL)")
	flag.StringVar(&o.specPath, "spec", "", "column spec (JSON)")
	flag.IntVar(&o.batch, "batch", 1000, "data rows per transaction (smaller = shorter locks)")
	flag.BoolVar(&o.dry, "dry-run", false, "validate and count inserted/updated/unchanged; write nothing")
	flag.StringVar(&o.ckptPath, "checkpoint", "", "checkpoint file (default <file>.import.json)")
	flag.StringVar(&o.errPath, "errors", "", "error report CSV (default <file>.errors.csv)")
	flag.BoolVar(&o.restart, "restart", false, "ignore an existing checkpoint and start from row 1 (safe: the import is idempotent)")
	flag.DurationVar(&o.pause, "pause", 0, "sleep between batches (throttle)")
	flag.DurationVar(&o.progress, "progress", 10*time.Second, "progress line interval (0 = off)")
	flag.BoolVar(&o.asJSON, "json", false, "print the summary as JSON")
	flag.IntVar(&o.crashAfter, "test-crash-after", 0, "test only: SIGKILL after N committed batches")
	flag.BoolVar(&o.crashBeforeCkpt, "test-crash-before-ckpt", false, "test only: the kill comes before the checkpoint is written")
	flag.Parse()
	if flag.NArg() != 1 || o.specPath == "" || o.url == "" {
		fmt.Fprintln(os.Stderr, "usage: staskimport -url URL -spec spec.json [-dry-run] [-batch N] FILE.(xlsx|csv)")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	sum, err := run(ctx, o, flag.Arg(0), os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "staskimport:", err)
		os.Exit(1)
	}
	if sum.Counts.Invalid+sum.Counts.Failed > 0 {
		os.Exit(3) // finished, but some rows are in the error report
	}
}

// Summary is printed at the end.
type Summary struct {
	File       string  `json:"file"`
	Table      string  `json:"table"`
	DryRun     bool    `json:"dry_run"`
	Resumed    int64   `json:"resumed_after_row"`
	Complete   bool    `json:"complete"`
	Counts     Counts  `json:"counts"`
	Seconds    float64 `json:"seconds"`
	ErrorsFile string  `json:"errors_file,omitempty"`
	Checkpoint string  `json:"checkpoint,omitempty"`
}

func run(ctx context.Context, o options, path string, out, errw io.Writer) (*Summary, error) {
	sp, specHash, err := LoadSpec(o.specPath)
	if err != nil {
		return nil, err
	}
	if o.batch < 1 || o.batch > 20000 {
		return nil, fmt.Errorf("-batch must be 1..20000")
	}
	if o.ckptPath == "" {
		o.ckptPath = path + ".import.json"
	}
	if o.errPath == "" {
		o.errPath = path + ".errors.csv"
	}
	fp, err := fingerprint(path, specHash)
	if err != nil {
		return nil, err
	}
	ck := &Checkpoint{Fingerprint: fp, Started: time.Now().UTC()}
	if !o.dry && !o.restart {
		old, err := loadCheckpoint(o.ckptPath)
		if err != nil {
			return nil, err
		}
		if old != nil {
			if old.Fingerprint != fp {
				return nil, fmt.Errorf("checkpoint %s belongs to a different file or spec (%s); use -restart to import from row 1", o.ckptPath, old.Fingerprint.describe(fp))
			}
			ck = old
		}
	}
	sum := &Summary{File: path, Table: sp.Table, DryRun: o.dry, Resumed: ck.RowsDone}
	if !o.dry {
		sum.ErrorsFile, sum.Checkpoint = o.errPath, o.ckptPath
	}
	t0 := time.Now()
	finish := func() {
		sum.Counts = ck.Counts
		sum.Complete = ck.Complete
		sum.Seconds = time.Since(t0).Seconds()
		printSummary(out, sum, o.asJSON)
	}
	if ck.Complete {
		finish()
		fmt.Fprintf(errw, "checkpoint says this file was already imported completely: nothing to do (-restart to repeat)\n")
		return sum, nil
	}
	src, err := openSource(path, sp)
	if err != nil {
		return nil, err
	}
	defer src.close()
	batches := make(chan []rawRow, 2)
	ready := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	var rerr error
	go readBatches(src, sp, o.batch, ck.RowsDone, batches, ready, &rerr, stop)
	if err := <-ready; err != nil {
		return nil, err
	}
	a := newApplier(o.url, sp, o.dry)
	defer a.close(context.Background())
	var ew *errWriter
	if !o.dry {
		if ew, err = openErrWriter(o.errPath); err != nil {
			return nil, err
		}
		defer ew.close()
	} else {
		ew = &errWriter{sample: errw}
	}
	lastProg := time.Now()
	done := 0
	for raw := range batches {
		if ctx.Err() != nil {
			break
		}
		bc, berr := processBatch(ctx, a, sp, raw, ck.Counts.Batches+1, ew)
		if berr != nil {
			finish()
			return sum, fmt.Errorf("stopped at row %d (the checkpoint is at row %d, rerun to resume): %w", raw[0].n, ck.RowsDone, berr)
		}
		ck.Counts.add(bc)
		done++
		if o.crashAfter > 0 && done == o.crashAfter && o.crashBeforeCkpt {
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		ck.RowsDone = raw[len(raw)-1].n
		if !o.dry {
			ew.flush()
			if err := ck.save(o.ckptPath); err != nil {
				return sum, fmt.Errorf("writing the checkpoint: %w", err)
			}
		}
		if o.crashAfter > 0 && done == o.crashAfter && !o.crashBeforeCkpt {
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		if o.progress > 0 && time.Since(lastProg) >= o.progress {
			lastProg = time.Now()
			fmt.Fprintf(errw, "%s row %d: inserted %d updated %d unchanged %d invalid %d failed %d\n", time.Now().Format("15:04:05"), ck.RowsDone,
				ck.Counts.Inserted, ck.Counts.Updated, ck.Counts.Unchanged, ck.Counts.Invalid, ck.Counts.Failed)
		}
		if o.pause > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(o.pause):
			}
		}
	}
	if ctx.Err() != nil {
		finish()
		return sum, fmt.Errorf("interrupted at row %d: rerun the same command to resume", ck.RowsDone)
	}
	if rerr != nil {
		finish()
		return sum, fmt.Errorf("%w (rerun to resume from row %d)", rerr, ck.RowsDone)
	}
	ck.Complete = true
	if !o.dry {
		if err := ck.save(o.ckptPath); err != nil {
			return sum, err
		}
	}
	finish()
	return sum, nil
}

func printSummary(w io.Writer, s *Summary, asJSON bool) {
	if asJSON {
		b, _ := json.MarshalIndent(s, "", "  ")
		fmt.Fprintln(w, string(b))
		return
	}
	c := s.Counts
	mode := "IMPORT"
	if s.DryRun {
		mode = "DRY RUN (nothing written)"
	}
	fmt.Fprintf(w, "%s %s -> %s\n", mode, s.File, s.Table)
	if s.Resumed > 0 {
		fmt.Fprintf(w, "  resumed after row %d\n", s.Resumed)
	}
	fmt.Fprintf(w, "  rows read   %d (in %d batches, %.1fs)\n  inserted    %d\n  updated     %d\n  unchanged   %d\n  invalid     %d\n  failed      %d\n  duplicates  %d (key repeated in a batch)\n  history     %d rows\n  retries     %d\n  complete    %v\n",
		c.Rows, c.Batches, s.Seconds, c.Inserted, c.Updated, c.Unchanged, c.Invalid, c.Failed, c.Duplicates, c.History, c.Retries, s.Complete)
	if s.ErrorsFile != "" && c.Invalid+c.Failed > 0 {
		fmt.Fprintf(w, "  error report: %s\n", s.ErrorsFile)
	}
}

// processBatch validates the rows, then applies them. A batch the database
// refuses is retried row by row so that one bad row does not hold back the rest.
func processBatch(ctx context.Context, a *applier, sp *Spec, raw []rawRow, batchNo int64, ew *errWriter) (Counts, error) {
	var bc Counts
	bc.Batches = 1
	seen := make(map[string]int, len(raw))
	var rows []vrow
	for _, r := range raw {
		bc.Rows++
		if r.bad != nil {
			bc.Invalid++
			ew.write(errRec{batchNo, r.line, "", "", "unreadable row: " + r.bad.Error()})
			continue
		}
		if blank(r.cells) {
			bc.Rows-- // an empty line is not data and not an error
			continue
		}
		v, bad := validate(sp, r)
		if bad != nil {
			bc.Invalid++
			bad.Batch = batchNo
			ew.write(*bad)
			continue
		}
		k := fmt.Sprint(v.key)
		if i, dup := seen[k]; dup {
			rows[i] = v
			bc.Duplicates++
			continue
		}
		seen[k] = len(rows)
		rows = append(rows, v)
	}
	if len(rows) == 0 {
		return bc, nil
	}
	res, err := a.apply(ctx, rows)
	bc.Retries += res.Retries
	if err == nil {
		res.Retries = 0
		bc.add(res)
		return bc, nil
	}
	var pe *pgconn.PgError
	if ctx.Err() != nil || !errors.As(err, &pe) {
		return bc, err // connection trouble: stop, the checkpoint allows resuming
	}
	// the database refused something in this batch: find the rows
	for _, r := range rows {
		one := []vrow{r}
		res, err := a.apply(ctx, one)
		bc.Retries += res.Retries
		if err != nil {
			if ctx.Err() != nil || !errors.As(err, &pe) {
				return bc, err
			}
			bc.Failed++
			ew.write(errRec{batchNo, r.line, "", fmt.Sprint(r.key), err.Error()})
			continue
		}
		res.Retries = 0
		bc.add(res)
	}
	return bc, nil
}

func blank(cells []string) bool {
	for _, c := range cells {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

func validate(sp *Spec, r rawRow) (vrow, *errRec) {
	v := vrow{n: r.n, line: r.line, vals: make([]any, len(sp.Columns))}
	for i := range sp.Columns {
		c := &sp.Columns[i]
		raw := ""
		if c.idx < len(r.cells) {
			raw = r.cells[c.idx]
		}
		x, err := c.parse(raw)
		if err != nil {
			return v, &errRec{0, r.line, c.Name, raw, err.Error()}
		}
		v.vals[i] = x
		if c.Name == sp.Key {
			v.key = x
		}
	}
	return v, nil
}

// ---- error report ----

type errWriter struct {
	f      *os.File
	w      *csv.Writer
	sample io.Writer // dry run: the first lines go to the terminal
	n      int
}

func openErrWriter(path string) (*errWriter, error) {
	st, err := os.Stat(path)
	fresh := err != nil || st.Size() == 0
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	e := &errWriter{f: f, w: csv.NewWriter(f)}
	if fresh {
		e.w.Write([]string{"batch", "row", "column", "value", "reason"})
	}
	return e, nil
}

func (e *errWriter) write(r errRec) {
	e.n++
	if e.w != nil {
		e.w.Write([]string{strconv.FormatInt(r.Batch, 10), strconv.FormatInt(r.Line, 10), r.Column, r.Value, r.Reason})
	}
	if e.sample != nil && e.n <= 20 {
		fmt.Fprintf(e.sample, "invalid row %d: column %s value %q: %s\n", r.Line, r.Column, r.Value, r.Reason)
	}
}

func (e *errWriter) flush() {
	if e.w != nil {
		e.w.Flush()
		e.f.Sync()
	}
}
func (e *errWriter) close() {
	if e.w != nil {
		e.flush()
		e.f.Close()
	}
}
