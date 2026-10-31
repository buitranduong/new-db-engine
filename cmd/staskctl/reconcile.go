package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"stask/internal/reconcile"
)

// cmdReconcile asks each node to compare its core with its derived
// structures (POST /v1/admin/reconcile, docs/33) and prints the answer.
//
// Exit status: 0 everything in sync, 3 behind or not decided, 4 diverged
// (or suspect), 1 the tool failed.
func cmdReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	urls := fs.String("url", "http://127.0.0.1:8080", "comma-separated HTTP API URLs of the nodes (each node is checked against its own views)")
	token := fs.String("token", os.Getenv("STASK_TOKEN"), "API token with full access (admin endpoint)")
	table := fs.String("table", "", "comma-separated tables (default all)")
	sample := fs.Int("sample", 0, "content check of 1/N of the keys (0: LSNs and row counts only; 1 or -full: every key)")
	full := fs.Bool("full", false, "compare every key (same as -sample 1)")
	slot := fs.String("cdc-slot", "", "also compare this logical replication slot with the tables")
	retries := fs.Int("retries", 3, "extra passes when a structure moves under the check")
	pause := fs.Int("pause-ms", 0, "sleep between chunks of a few thousand rows, to go easy on a busy node")
	asJSON := fs.Bool("json", false, "print the JSON reports")
	timeout := fs.Duration("timeout", 30*time.Minute, "give up after this long per node")
	var tf tlsFlags
	tf.add(fs)
	fs.Parse(args)
	if *full {
		*sample = 1
	}
	hc, err := tf.client(*timeout)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("sample", strconv.Itoa(*sample))
	q.Set("retries", strconv.Itoa(*retries))
	q.Set("pause_ms", strconv.Itoa(*pause))
	if *table != "" {
		q.Set("table", *table)
	}
	if *slot != "" {
		q.Set("slot", *slot)
	}
	worst := ""
	for _, u := range strings.Split(*urls, ",") {
		u = strings.TrimSpace(u)
		req, _ := http.NewRequest("POST", strings.TrimRight(u, "/")+"/v1/admin/reconcile?"+q.Encode(), nil)
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("%s: %w", u, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("%s: %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
		}
		var rep reconcile.Report
		if err := json.Unmarshal(b, &rep); err != nil {
			return fmt.Errorf("%s: bad answer: %v", u, err)
		}
		rep.Node = u
		if *asJSON {
			out, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Println(string(out))
		} else {
			printReconcile(&rep)
		}
		worst = reconcile.Worse(worst, rep.Verdict)
	}
	switch worst {
	case reconcile.StInSync, reconcile.StUnchecked, "":
		return nil
	case reconcile.StDiverged, reconcile.StSuspect, "INVALID", "LOG_MISSING":
		os.Exit(4)
	default:
		os.Exit(3)
	}
	return errors.New("unreachable")
}

func printReconcile(r *reconcile.Report) {
	fmt.Printf("== %s  durable_lsn=%d  verdict=%s  (%d ms, sample %s)\n", r.Node, r.DurableLSN, r.Verdict, r.DurationMs, sampleName(r.Sample))
	for _, t := range r.Tables {
		fmt.Printf("table %s  core_rows=%d\n", t.Table, t.CoreRows)
		for _, s := range t.Structures {
			line := fmt.Sprintf("  %-28s %-12s applied=%d durable=%d", s.Name, s.Status, s.AppliedLSN, s.DurableLSN)
			if s.BehindRecords > 0 {
				line += fmt.Sprintf("  behind by %d records / %d ms", s.BehindRecords, s.BehindMs)
			}
			if s.ViewRows != 0 || s.CoreRows != 0 {
				line += fmt.Sprintf("  rows core=%d view=%d", s.CoreRows, s.ViewRows)
			}
			fmt.Println(line)
			if s.Checked != "" && s.Checked != "lsn" {
				fmt.Printf("      checked: %s", s.Checked)
				if s.CompareLSN != 0 {
					fmt.Printf(" at lsn %d (%d keys)", s.CompareLSN, s.KeysChecked)
				}
				fmt.Println()
			}
			if s.Detail != "" {
				fmt.Printf("      %s\n", s.Detail)
			}
			if d := s.Diff; d != nil && d.Count > 0 {
				fmt.Printf("      first differing key: %q  last: %q  (%d keys: %d missing in the structure, %d extra, %d changed)\n",
					d.FirstKey, d.LastKey, d.Count, d.MissingInView, d.ExtraInView, d.Changed)
				for _, e := range d.Examples {
					fmt.Printf("        %-16s %q\n", e.Kind, e.Key)
				}
			}
		}
	}
	if s := r.Slot; s != nil {
		fmt.Printf("slot %s (%s)  %s  confirmed_lsn=%d durable_lsn=%d  behind=%d records / %d bytes of log\n",
			s.Name, s.Plugin, s.Status, s.ConfirmedLSN, s.DurableLSN, s.BehindRecs, s.LagBytes)
		if s.Detail != "" {
			fmt.Printf("      %s\n", s.Detail)
		}
		for _, t := range s.Tables {
			fmt.Printf("  %-24s %-8s not delivered: %d transactions, %d row changes; oldest %d ms old\n", t.Table, t.Status, t.PendingTx, t.PendingOp, t.OldestMs)
		}
	}
	for _, n := range r.Notes {
		fmt.Println("note:", n)
	}
}

func sampleName(n int) string {
	switch {
	case n == 0:
		return "none (LSNs and counts)"
	case n == 1:
		return "all keys"
	}
	return fmt.Sprintf("1/%d", n)
}
