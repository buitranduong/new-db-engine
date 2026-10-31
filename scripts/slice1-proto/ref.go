package main

import (
	"crypto/md5"
	"encoding/hex"
	"sort"
)

// reference executor: brute force over a map (the oracle for small random tests)
type Model struct {
	rows map[string]Row
	dim  Dim
}

func NewModel(rows []Row, d Dim) *Model {
	m := &Model{rows: map[string]Row{}, dim: d}
	for _, r := range rows {
		m.rows[string(r.Key[:])] = r
	}
	return m
}
func (m *Model) Apply(tx Txn) {
	for _, op := range tx.Ops {
		switch op.Kind {
		case OpPut:
			m.rows[string(op.Row.Key[:])] = op.Row
		case OpDel:
			delete(m.rows, string(op.Row.Key[:]))
		case OpDim:
			m.dim = *op.Dim
		}
	}
}
func (m *Model) Clone() *Model {
	c := &Model{rows: make(map[string]Row, len(m.rows)), dim: m.dim}
	for k, v := range m.rows {
		c.rows[k] = v
	}
	return c
}
func (m *Model) R2(q R2Q) []Out {
	type cand struct {
		r Row
		h string
	}
	var g [3][]cand
	for _, r := range m.rows {
		if r.Cat != q.Cat || r.Price < q.Lo || r.Price > q.Hi || !m.dim.car[r.Tel].Active {
			continue
		}
		s := md5.Sum(append([]byte(string(r.Key[:])), '4', '2'))
		g[r.Tel] = append(g[r.Tel], cand{r, hex.EncodeToString(s[:])})
	}
	less := func(a, b cand) bool {
		if a.r.Rate != b.r.Rate {
			return a.r.Rate > b.r.Rate
		}
		if a.h != b.h {
			return a.h < b.h
		}
		return string(a.r.Key[:]) < string(b.r.Key[:])
	}
	var all []cand
	var tel []uint8
	for t := 0; t < 3; t++ {
		sort.Slice(g[t], func(i, j int) bool { return less(g[t][i], g[t][j]) })
		lo, hi := (q.Page-1)*q.Quota[t], min(len(g[t]), q.Page*q.Quota[t])
		for i := lo; i < hi; i++ {
			all = append(all, g[t][i])
			tel = append(tel, uint8(t))
		}
	}
	idx := make([]int, len(all))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return less(all[idx[i]], all[idx[j]]) })
	var out []Out
	for _, i := range idx {
		c := all[i]
		out = append(out, Out{Key: string(c.r.Key[:]), Tel: telNames[tel[i]], Name: m.dim.car[tel[i]].Name, Price: c.r.Price, Rate: c.r.Rate})
	}
	return out
}
