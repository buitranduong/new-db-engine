package spill

import "hash/maphash"

// HashAgg is a hash aggregate (or DISTINCT, with a trivial state) that
// degrades to partitioned processing: groups live in a hash table while
// their state fits in MemLimit; once it does not, rows of groups not
// already in the table go to Fanout partition files by hash of the key,
// and after the input the table is emitted and each partition is
// aggregated the same way (recursively, with another hash).
// Rows of groups in the table keep being folded in, so a skewed key costs
// no disk.
type HashAgg[T, S any] struct {
	M        *Manager
	C        Codec[T]
	Key      func(T) (string, error)
	New      func(T) (S, error) // state for the first row of a group
	Update   func(S, T) error   // fold a row (also the first)
	Size     func(S) int64      // bytes of a state, charged at creation
	Finish   func(S) error      // emit a finished group
	MemLimit int64

	seeds [MaxDepth + 2]maphash.Seed
	enc   []byte
}

// Run aggregates src.
func (a *HashAgg[T, S]) Run(src Source[T]) error {
	for i := range a.seeds {
		a.seeds[i] = maphash.MakeSeed()
	}
	return a.run(src, 0)
}

func (a *HashAgg[T, S]) run(src Source[T], depth int) error {
	table := map[string]S{}
	var used int64
	var ps *parts
	defer func() {
		if ps != nil {
			ps.abort()
		}
	}()
	err := src(func(v T) error {
		key, err := a.Key(v)
		if err != nil {
			return err
		}
		if s, ok := table[key]; ok {
			return a.Update(s, v)
		}
		if ps != nil {
			a.enc = a.C.Enc(a.enc[:0], v)
			return ps.add(int(maphash.String(a.seeds[depth], key)%Fanout), a.enc)
		}
		s, err := a.New(v)
		if err != nil {
			return err
		}
		if err := a.Update(s, v); err != nil {
			return err
		}
		table[key] = s
		used += a.Size(s) + int64(len(key)) + entryOverhead
		if used > a.MemLimit {
			if depth >= MaxDepth {
				if used > 4*a.MemLimit {
					return ErrSkew
				}
				return nil
			}
			if depth == 0 {
				stats.spills.Add(1)
			}
			ps, err = a.M.newParts(Fanout)
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, s := range table {
		if err := a.Finish(s); err != nil {
			return err
		}
	}
	table = nil
	if ps == nil {
		return nil
	}
	runs, err := ps.finish()
	ps = nil
	if err != nil {
		return err
	}
	defer func() { closeRuns(runs) }()
	for _, r := range runs {
		if err := a.run(source(r, a.C), depth+1); err != nil {
			return err
		}
		r.Close()
	}
	return nil
}
