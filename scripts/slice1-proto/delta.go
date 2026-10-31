package main

import (
	"bufio"
	"fmt"
	"os"
)

// deltaOps builds K deterministic ops over N base rows (see docs/31 §measure):
// updates of price, rate, category, deletes and inserts; every 20th key is
// updated twice (several versions of one key in the tail).
func deltaOps(N int64, K int) []Op {
	stride := N / int64(K)
	var ops []Op
	for i := 0; i < K; i++ {
		g := int64(i)*stride + int64(i)%stride
		r := genRow(g)
		switch m := i % 10; {
		case m <= 5:
			r.Price = int32(1000000 + (g*104729)%2500000)
			ops = append(ops, Op{Kind: OpPut, Row: r})
			if i%20 == 0 {
				r.Rate = 10
				ops = append(ops, Op{Kind: OpPut, Row: r})
			}
		case m == 6:
			r.Rate = 10
			ops = append(ops, Op{Kind: OpPut, Row: r})
		case m == 7:
			ops = append(ops, Op{Kind: OpDel, Row: r})
		case m == 8:
			r.Cat = 0
			ops = append(ops, Op{Kind: OpPut, Row: r})
		default:
			n := genRow(N + int64(i))
			n.Price = int32(1000000 + ((N+int64(i))*104729)%2500000)
			n.Rate = 10
			ops = append(ops, Op{Kind: OpPut, Row: n})
		}
	}
	return ops
}

func toTxns(ops []Op, per int, lsn *uint64) []Txn {
	var out []Txn
	for i := 0; i < len(ops); i += per {
		*lsn++
		out = append(out, Txn{LSN: *lsn, Ops: ops[i:min(len(ops), i+per)]})
	}
	return out
}

// writeDeltaCSV writes the net effect per key for the PostgreSQL oracle:
// op(u|d|i),number,category,price,tel,store_rate
func writeDeltaCSV(path string, N int64, ops []Op) error {
	net := map[string]Op{}
	var order []string
	for _, o := range ops {
		k := string(o.Row.Key[:])
		if _, ok := net[k]; !ok {
			order = append(order, k)
		}
		net[k] = o
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, k := range order {
		o := net[k]
		r := o.Row
		kind := "u"
		var g int64
		fmt.Sscanf(k[2:], "%d", &g)
		switch {
		case o.Kind == OpDel:
			kind = "d"
		case g >= N:
			kind = "i"
		}
		fmt.Fprintf(w, "%s,%s,%s,%d,%s,%d\n", kind, k, catNames[r.Cat], r.Price, telNames[r.Tel], r.Rate)
	}
	return w.Flush()
}
