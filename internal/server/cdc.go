package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"stask/internal/core"
	"stask/internal/sql"
)

// CDC over HTTP, for consumers without a PostgreSQL driver (the same slots
// as pg_create_logical_replication_slot and friends):
//
//	GET    /v1/cdc                    list slots
//	POST   /v1/cdc                    {"name": ..., "plugin": "wal2json" | "test_decoding"}
//	DELETE /v1/cdc/{slot}
//	GET    /v1/cdc/{slot}/stream?opt=value...   changes as they commit, one
//	       JSON object per line: {"lsn", "xid", "data", "commit"}; data is
//	       the plugin's message (wal2json: a JSON object, format-version 2
//	       with nextlsn by default; test_decoding: a string). Query
//	       parameters are plugin options. The stream does not advance the
//	       slot: a consumer confirms what it has applied with
//	POST   /v1/cdc/{slot}/ack?lsn=X   X = the lsn of a commit message
//	       (commit: true); after a reconnect the stream resumes after the
//	       last confirmed transaction, so delivery is at least once.

func (s *Server) cdcRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /v1/cdc", s.cdcList)
	m.HandleFunc("POST /v1/cdc", s.cdcCreate)
	m.HandleFunc("DELETE /v1/cdc/{slot}", s.cdcDrop)
	m.HandleFunc("GET /v1/cdc/{slot}/stream", s.cdcStream)
	m.HandleFunc("POST /v1/cdc/{slot}/ack", s.cdcAck)
}

func cdcFail(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, core.ErrNoSlot):
		code = http.StatusNotFound
	case errors.Is(err, core.ErrSlotExists), errors.Is(err, core.ErrSlotBusy):
		code = http.StatusConflict
	case errors.Is(err, core.ErrSlotInvalid):
		code = http.StatusGone
	}
	reply(w, code, map[string]any{"error": err.Error()})
}

func (s *Server) cdcList(w http.ResponseWriter, r *http.Request) {
	type slot struct {
		core.SlotInfo
		LagBytes int64 `json:"lag_bytes"`
	}
	out := []slot{}
	for _, si := range s.P.E.Slots() {
		out = append(out, slot{si, s.P.E.SlotLag(si)})
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) cdcCreate(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Plugin string }
	if err := decode(r, &req); err != nil {
		cdcFail(w, err)
		return
	}
	if req.Plugin == "" {
		req.Plugin = "wal2json"
	}
	if req.Plugin != "wal2json" && req.Plugin != "test_decoding" {
		cdcFail(w, errors.New("plugin must be wal2json or test_decoding"))
		return
	}
	info, err := s.P.E.CreateSlot(req.Name, req.Plugin)
	if err != nil {
		cdcFail(w, err)
		return
	}
	reply(w, http.StatusCreated, info)
}

func (s *Server) cdcDrop(w http.ResponseWriter, r *http.Request) {
	if err := s.P.E.DropSlot(r.PathValue("slot")); err != nil {
		cdcFail(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"dropped": r.PathValue("slot")})
}

func (s *Server) cdcAck(w http.ResponseWriter, r *http.Request) {
	pos, err := sql.ConfirmLSN(s.P.E, r.PathValue("slot"), r.URL.Query().Get("lsn"))
	if err != nil {
		cdcFail(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"confirmed_lsn": pos})
}

func (s *Server) cdcStream(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	opts := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			opts[k] = v[len(v)-1]
		}
	}
	found := false
	for _, si := range s.P.E.Slots() {
		found = found || si.Name == slot
	}
	if !found {
		cdcFail(w, core.ErrNoSlot)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	// Lines are written by hand: wal2json messages are already JSON (an
	// encoder would validate and compact each again), and flushes are
	// batched: when decoding has caught up, else at most every 10ms.
	raw := sql.IsJSONMessage(s.P.E, slot)
	bw := bufio.NewWriterSize(w, 64<<10)
	var line []byte
	lastFlush := time.Now()
	flush := func() {
		bw.Flush()
		if fl != nil {
			fl.Flush()
		}
		lastFlush = time.Now()
	}
	err := sql.StreamChanges(s.P.E, slot, opts, r.Context().Done(), flush, func(lsn string, xid uint64, data string, commit bool) error {
		line = append(line[:0], `{"lsn":"`...)
		line = append(line, lsn...)
		line = append(line, `","xid":`...)
		line = strconv.AppendUint(line, xid, 10)
		line = append(line, `,"data":`...)
		if raw {
			line = append(line, data...)
		} else {
			q, _ := json.Marshal(data)
			line = append(line, q...)
		}
		if commit {
			line = append(line, `,"commit":true`...)
		}
		line = append(line, "}\n"...)
		if _, err := bw.Write(line); err != nil {
			return err
		}
		if commit && time.Since(lastFlush) > 10*time.Millisecond {
			flush()
		}
		return nil
	})
	if err != nil && r.Context().Err() == nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error()}) // e.g. another consumer holds the slot
		bw.Write(append(b, '\n'))
	}
	flush()
}
