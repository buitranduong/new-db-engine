package core

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"os"

	"stask/internal/schema"
	"stask/internal/wal"
)

// CheckpointHeader is the decoded header of a checkpoint file.
type CheckpointHeader struct {
	Version     int
	StartLSN    uint64
	StartPos    wal.Pos
	LastTS      int64
	NextTableID uint32
	Tables      []schema.Table
}

// CheckpointEntry is one index entry of a checkpoint: the head version of
// a key.
type CheckpointEntry struct {
	Table   uint32
	Key     string
	LSN     uint64
	Loc     wal.Loc
	ValOff  uint32
	ValLen  uint32
	Deleted bool
}

// LatestCheckpoint returns the path of the newest checkpoint in a data
// directory, or "" when there is none.
func LatestCheckpoint(dir string) string {
	p, _ := latestCheckpoint(dir + "/ckpt")
	return p
}

// ReadCheckpoint decodes a checkpoint file sequentially (no engine): the
// header, then fn for every entry in file order. It is the reference
// decoder for tools and for other implementations of the format.
func ReadCheckpoint(path string, fn func(CheckpointEntry) error) (*CheckpointHeader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, errors.New("short checkpoint")
	}
	hl := int(binary.LittleEndian.Uint32(data))
	if 4+hl > len(data) {
		return nil, errors.New("bad checkpoint header")
	}
	var h ckptHeader
	if err := json.Unmarshal(data[4:4+hl], &h); err != nil {
		return nil, err
	}
	hdr := &CheckpointHeader{Version: h.Version, StartLSN: h.StartLSN, StartPos: wal.Pos{Seg: h.StartSeg, Off: h.StartOff},
		LastTS: h.LastTS, NextTableID: h.NextTableID, Tables: h.Tables}
	p := 4 + hl
	for {
		if p+4 > len(data) {
			return nil, errors.New("checkpoint truncated")
		}
		n := int(binary.LittleEndian.Uint32(data[p:]))
		if n == 0 {
			return hdr, nil
		}
		if p+8+n > len(data) {
			return nil, errors.New("checkpoint truncated")
		}
		pl := data[p+8 : p+8+n]
		if crc32.Checksum(pl, crcTab) != binary.LittleEndian.Uint32(data[p+4:]) {
			return nil, errors.New("checkpoint chunk crc mismatch")
		}
		table, ents, longs, err := decodeChunk(pl, h.Version)
		if err != nil {
			return nil, err
		}
		nl := 0
		for i := range ents {
			en := &ents[i]
			var key string
			if en.klen == longKey {
				key = longs[nl]
				nl++
			} else {
				key = string(en.k[:en.klen])
			}
			ce := CheckpointEntry{Table: table, Key: key, LSN: en.lsn,
				Loc:    wal.Loc{Ok: true, Seg: en.seg, Off: en.off, Idx: en.idx},
				ValOff: en.valOff, ValLen: en.valLen, Deleted: en.flags&entDeleted != 0}
			if err := fn(ce); err != nil {
				return nil, err
			}
		}
		p += 8 + n
	}
}
