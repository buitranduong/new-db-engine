package chsync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CH is a ClickHouse server reached over its HTTP interface (no driver
// dependency; the bodies are TSV).
type CH struct {
	URL string // e.g. http://127.0.0.1:8123
	HC  *http.Client
}

func NewCH(u string) *CH {
	return &CH{URL: strings.TrimRight(u, "/"), HC: &http.Client{Timeout: 0}}
}

// settings for every request: PostgreSQL text timestamps ("2026-11-04
// 04:35:19.123+00") are parsed as they are, and a write is acknowledged
// only once it is in a part (no asynchronous insert buffering).
var chSettings = url.Values{
	"date_time_input_format": {"best_effort"},
	"async_insert":           {"0"},
}

func (c *CH) do(ctx context.Context, query string, body io.Reader) (io.ReadCloser, error) {
	v := url.Values{}
	for k, s := range chSettings {
		v[k] = s
	}
	if body != nil {
		v.Set("query", query)
	} else {
		body = strings.NewReader(query)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/?"+v.Encode(), body)
	if err != nil {
		return nil, err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("clickhouse: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

// Exec runs one statement and discards its output.
func (c *CH) Exec(ctx context.Context, sql string) error {
	rc, err := c.do(ctx, sql, nil)
	if err != nil {
		return fmt.Errorf("%w\n  statement: %s", err, clip(sql))
	}
	io.Copy(io.Discard, rc)
	return rc.Close()
}

// Query runs one statement and returns its whole output (TSV by default).
func (c *CH) Query(ctx context.Context, sql string) (string, error) {
	rc, err := c.do(ctx, sql, nil)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

// Stream runs one statement and returns its output as a stream.
func (c *CH) Stream(ctx context.Context, sql string) (io.ReadCloser, error) {
	return c.do(ctx, sql, nil)
}

// Insert sends body (TSV rows) as the data of insert, e.g.
// "INSERT INTO t (a, b) FORMAT TabSeparated". One INSERT of at most
// max_insert_block_size rows is one part: all or nothing.
func (c *CH) Insert(ctx context.Context, insert string, body io.Reader) error {
	rc, err := c.do(ctx, insert, body)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, rc)
	return rc.Close()
}

// Wait waits until the server answers.
func (c *CH) Wait(ctx context.Context, d time.Duration) error {
	end := time.Now().Add(d)
	for {
		_, err := c.Query(ctx, "SELECT 1")
		if err == nil || time.Now().After(end) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
