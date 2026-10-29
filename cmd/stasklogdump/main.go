// stasklogdump prints the persistent state of a stopped data directory in a
// canonical form, one JSON object per line: the newest checkpoint (header,
// then every index entry in file order), then every record of every log
// segment in log order, and where each segment's records end. It is the
// reference reader of the disk format (docs/09_spec.md): another
// implementation must print the same lines for the same directory.
//
// Run it on a stopped server's directory or a copy: opening the log removes
// leftovers of an interrupted segment compression.
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"log"
	"os"
	"path/filepath"
	"unicode/utf8"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/wal"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func main() {
	dir := flag.String("dir", "", "data directory (holds wal/ and ckpt/)")
	rows := flag.Bool("rows", false, "also decode each value as a row of its table")
	noLog := flag.Bool("no-log", false, "checkpoint only")
	flag.Parse()
	if *dir == "" {
		log.Fatal("-dir required")
	}
	w := bufio.NewWriterSize(os.Stdout, 1<<20)
	defer w.Flush()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	out := func(v any) {
		if err := enc.Encode(v); err != nil {
			log.Fatal(err)
		}
	}
	tables := map[uint32]*schema.Table{}
	addTable := func(t schema.Table) {
		if err := t.Init(); err == nil {
			tables[t.ID] = &t
		}
	}

	if path := core.LatestCheckpoint(*dir); path != "" {
		var entries []map[string]any
		hdr, err := core.ReadCheckpoint(path, func(ce core.CheckpointEntry) error {
			m := map[string]any{"type": "entry", "table": ce.Table, "lsn": ce.LSN, "loc": locStr(ce.Loc),
				"val_off": ce.ValOff, "val_len": ce.ValLen, "deleted": ce.Deleted}
			keyField(m, "key", []byte(ce.Key))
			entries = append(entries, m)
			return nil
		})
		if err != nil {
			log.Fatalf("%s: %v", path, err)
		}
		out(map[string]any{"type": "ckpt", "file": filepath.Base(path), "version": hdr.Version, "start_lsn": hdr.StartLSN,
			"start": posStr(hdr.StartPos), "last_ts": hdr.LastTS, "next_table_id": hdr.NextTableID, "tables": hdr.Tables})
		for _, t := range hdr.Tables {
			addTable(t)
		}
		for _, m := range entries {
			out(m)
		}
	}
	if *noLog {
		return
	}

	l, err := wal.Open(filepath.Join(*dir, "wal"), wal.MaxSegment)
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	for _, seg := range l.Segments() {
		stop, torn, err := l.Scan(seg, 0, -1, func(r *wal.Record) error {
			m := map[string]any{"type": "rec", "pos": posStr(r.Pos), "size": r.Size, "kind": kindName(r.Kind),
				"lsn": r.LSN, "ts": r.TS}
			if r.Kind == wal.KindDDL {
				var t schema.Table
				if err := json.Unmarshal(r.DDL, &t); err != nil {
					return fmt.Errorf("ddl at %s: %w", posStr(r.Pos), err)
				}
				m["ddl"] = t
				addTable(t)
			} else {
				ops := make([]map[string]any, len(r.Ops))
				for i, op := range r.Ops {
					o := map[string]any{"op": opName(op.Kind), "table": op.Table, "prev": locStr(op.Prev),
						"val_off": op.ValOff, "val_len": len(op.Val), "val_crc": crc32.Checksum(op.Val, castagnoli)}
					keyField(o, "key", op.Key)
					if r.Kind == wal.KindMove {
						o["orig_lsn"], o["orig_ts"] = op.OrigLSN, op.OrigTS
					}
					if t := tables[op.Table]; *rows && t != nil && op.Kind == wal.OpPut {
						lsn := r.LSN
						if r.Kind == wal.KindMove {
							lsn = op.OrigLSN
						}
						if row, err := t.DecodeAt(op.Val, lsn); err == nil {
							o["row"] = t.ToJSON(row)
						} else {
							o["row_error"] = err.Error()
						}
					}
					ops[i] = o
				}
				m["ops"] = ops
			}
			out(m)
			return nil
		})
		if err != nil {
			log.Fatalf("segment %d: %v", seg, err)
		}
		out(map[string]any{"type": "end", "pos": posStr(wal.Pos{Seg: seg, Off: stop}), "torn": torn})
	}
}

func posStr(p wal.Pos) string { return fmt.Sprintf("%d:%d", p.Seg, p.Off) }

func locStr(l wal.Loc) any {
	if !l.Ok {
		return nil
	}
	return fmt.Sprintf("%d:%d:%d", l.Seg, l.Off, l.Idx)
}

// keyField stores a key as a string when it is valid UTF-8, else as hex.
func keyField(m map[string]any, name string, k []byte) {
	if utf8.Valid(k) {
		m[name] = string(k)
	} else {
		m[name+"_hex"] = hex.EncodeToString(k)
	}
}

func kindName(k uint8) string {
	switch k {
	case wal.KindTx:
		return "tx"
	case wal.KindDDL:
		return "ddl"
	case wal.KindMove:
		return "move"
	case wal.KindTxPart:
		return "tx_part"
	}
	return fmt.Sprint(k)
}

func opName(k uint8) string {
	switch k {
	case wal.OpPut:
		return "put"
	case wal.OpDel:
		return "del"
	}
	return fmt.Sprint(k)
}
