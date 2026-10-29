package raft

import (
	"encoding/binary"
	"errors"
)

// EncodeMsg serializes a message for a network transport.
func EncodeMsg(m Msg) []byte {
	b := make([]byte, 0, 80+len(m.Entries)*32)
	b = append(b, byte(m.Type))
	fl := byte(0)
	if m.Reject {
		fl = 1
	}
	b = append(b, fl)
	for _, v := range []uint64{m.From, m.To, m.Term, m.PrevIndex, m.PrevTerm, m.Commit, m.Hint, m.Seq} {
		b = binary.LittleEndian.AppendUint64(b, v)
	}
	return append(b, encodeEntries(m.Entries)...)
}

// DecodeMsg is the inverse of EncodeMsg.
func DecodeMsg(b []byte) (Msg, error) {
	if len(b) < 2+8*8+4 {
		return Msg{}, errors.New("raft: short message")
	}
	m := Msg{Type: MsgType(b[0]), Reject: b[1]&1 == 1}
	if m.Type < MsgPreVote || m.Type > MsgSnap {
		return Msg{}, errors.New("raft: unknown message type")
	}
	p := []*uint64{&m.From, &m.To, &m.Term, &m.PrevIndex, &m.PrevTerm, &m.Commit, &m.Hint, &m.Seq}
	b = b[2:]
	for _, x := range p {
		*x = binary.LittleEndian.Uint64(b)
		b = b[8:]
	}
	es, err := decodeEntries(b)
	if err != nil {
		return Msg{}, err
	}
	m.Entries = es
	return m, nil
}
