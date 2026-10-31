package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLevelOf(t *testing.T) {
	for _, c := range []struct {
		msg   string
		level int
		out   string
	}{
		{"debug: details", 0, "details"},
		{"warn: -pg-insecure", 2, "-pg-insecure"},
		{"error: boom", 3, "boom"},
		{"checkpoint: lsn=5", 1, "checkpoint: lsn=5"},
		{"connection or apply error, will reconnect", 2, "connection or apply error, will reconnect"},
		{"info: x", 1, "x"},
	} {
		if lv, m := levelOf(c.msg); lv != c.level || m != c.out {
			t.Errorf("%q: got %d %q", c.msg, lv, m)
		}
	}
}

func TestLevelWriter(t *testing.T) {
	var b bytes.Buffer
	w := &levelWriter{out: &b, min: 2, asJSON: true}
	w.Write([]byte("an info line\n"))
	w.Write([]byte("warn: look\n"))
	if strings.Count(b.String(), "\n") != 1 {
		t.Fatalf("level filter: %q", b.String())
	}
	var m map[string]string
	if err := json.Unmarshal(b.Bytes(), &m); err != nil || m["level"] != "warn" || m["msg"] != "look" || m["time"] == "" {
		t.Fatalf("json line: %v %v", m, err)
	}
	if err := setupLogging("loud", "text"); err == nil {
		t.Error("bad level accepted")
	}
	if err := setupLogging("info", "xml"); err == nil {
		t.Error("bad format accepted")
	}
}
