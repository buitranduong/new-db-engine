package proj

import (
	"math/rand"
	"testing"
)

// intPosIndex against a map, through inserts, overwrites (a key moves to a
// new position), deletes and rehashes with many tombstones.
func TestIntPosIndex(t *testing.T) {
	var x intPosIndex
	var keys pvec[int64] // key at each position (300000 steps: several chunks)
	ref := map[int64]uint32{}
	r := rand.New(rand.NewSource(1))
	for step := 0; step < 300000; step++ {
		k := r.Int63n(20000) - 10000
		switch r.Intn(3) {
		case 0, 1: // put: delete the old position, append a new one
			if _, ok := x.del(k, &keys); ok != (ref[k] != 0) {
				t.Fatalf("step %d: del %d presence %v", step, k, ok)
			}
			delete(ref, k)
			keys.push(k)
			x.set(k, uint32(keys.len()-1), &keys)
			ref[k] = uint32(keys.len())
		case 2:
			p, ok := x.del(k, &keys)
			if want, had := ref[k]; ok != had || (ok && p != want-1) {
				t.Fatalf("step %d: del %d = %d %v, want %d %v", step, k, p, ok, want-1, had)
			}
			delete(ref, k)
		}
		if step%10007 == 0 {
			for k, want := range ref {
				if p, ok := x.get(k, &keys); !ok || p != want-1 {
					t.Fatalf("get %d = %d %v, want %d", k, p, ok, want-1)
				}
			}
			if x.live != len(ref) {
				t.Fatalf("live %d, want %d", x.live, len(ref))
			}
		}
	}
	if len(x.slots) > 1<<17 {
		t.Fatalf("table grew to %d slots for %d keys", len(x.slots), x.live)
	}
}
