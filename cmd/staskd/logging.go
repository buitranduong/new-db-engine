package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Structured logging. The server logs with the standard log package; this
// installs a writer behind it that gives every line a level, filters by
// -log-level and writes either "time LEVEL message" or one JSON object per
// line (-log-format json). The level of a line is the prefix "debug:",
// "warn:" or "error:" when it has one; otherwise it is inferred: lines that
// say failed, error, refused, corrupt or panic are warn, the rest info
// (docs/21 section 6).

type levelWriter struct {
	mu     sync.Mutex
	out    io.Writer
	min    int
	asJSON bool
}

var levelNames = []string{"debug", "info", "warn", "error"}

func levelOf(msg string) (int, string) {
	for i, n := range levelNames {
		if strings.HasPrefix(msg, n+":") {
			return i, strings.TrimSpace(msg[len(n)+1:])
		}
	}
	l := strings.ToLower(msg)
	for _, k := range []string{"failed", "error", "refused", "corrupt", "panic", "fatal"} {
		if strings.Contains(l, k) {
			return 2, msg
		}
	}
	return 1, msg
}

func (w *levelWriter) Write(b []byte) (int, error) {
	msg := strings.TrimRight(string(b), "\n")
	lv, msg := levelOf(msg)
	if lv < w.min {
		return len(b), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if w.asJSON {
		j, _ := json.Marshal(map[string]string{"time": now.Format(time.RFC3339Nano), "level": levelNames[lv], "msg": msg})
		w.out.Write(append(j, '\n'))
	} else {
		fmt.Fprintf(w.out, "%s %-5s %s\n", now.Format("2006-01-02T15:04:05.000Z07:00"), strings.ToUpper(levelNames[lv]), msg)
	}
	return len(b), nil
}

// setupLogging installs the writer; it returns an error for an unknown level
// or format.
func setupLogging(level, format string) error {
	min := -1
	for i, n := range levelNames {
		if n == strings.ToLower(level) {
			min = i
		}
	}
	if min < 0 {
		return fmt.Errorf("-log-level %q: want debug, info, warn or error", level)
	}
	if format != "text" && format != "json" {
		return fmt.Errorf("-log-format %q: want text or json", format)
	}
	log.SetFlags(0)
	log.SetOutput(&levelWriter{out: os.Stderr, min: min, asJSON: format == "json"})
	return nil
}
