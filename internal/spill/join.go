package spill

import (
	"hash/maphash"
)

const (
	// Fanout is the number of partitions one level of repartitioning makes.
	Fanout = 32
	// MaxDepth is how many times a partition is split again before a
	// partition over the limit fails with ErrSkew.
	MaxDepth = 4
	// entryOverhead approximates the map and slice cost of a table entry
	// beyond the row and key themselves.
	entryOverhead = 160
)

// Entry is a build-side row of a join with its match flag, which the
// caller sets when a probe row joined with it (outer joins list the
// entries left unmatched).
type Entry[T any] struct {
	Row     T
	Matched bool
}

// Join is a Grace hash join. The build side is loaded into a hash table
// while it fits in MemLimit; if it does not, both sides are split by hash
// of the key into Fanout partition files and each pair is joined on its
// own, split again if the build part still does not fit. Build rows
// that have a NULL key (ok=false) never match; they are still passed to
// OnBuildDone, as outer joins need them.
type Join[T any] struct {
	M        *Manager
	C        Codec[T]
	BuildKey func(T) (key string, ok bool, err error)
	ProbeKey func(T) (key string, ok bool, err error)
	MemLimit int64

	seeds [MaxDepth + 2]maphash.Seed
	enc   []byte
}

// Run joins build with probe. For each probe row onProbe gets the build
// entries with an equal key (none for a NULL key). After a build part is
// done, onBuildDone gets all its entries. The order of calls across parts
// is unspecified.
func (j *Join[T]) Run(build, probe Source[T], onProbe func(T, []*Entry[T]) error, onBuildDone func([]*Entry[T]) error) error {
	for i := range j.seeds {
		j.seeds[i] = maphash.MakeSeed()
	}
	return j.run(build, probe, 0, onProbe, onBuildDone)
}

func (j *Join[T]) part(depth int, key string) int {
	return int(maphash.String(j.seeds[depth], key) % Fanout)
}

func (j *Join[T]) run(build, probe Source[T], depth int, onProbe func(T, []*Entry[T]) error, onBuildDone func([]*Entry[T]) error) error {
	table := map[string][]*Entry[T]{}
	var entries []*Entry[T] // build order
	var used int64
	var bparts *parts
	defer func() {
		if bparts != nil {
			bparts.abort()
		}
	}()
	toDisk := func(v T, key string, ok bool) error {
		p := 0
		if ok {
			p = j.part(depth, key)
		}
		j.enc = j.C.Enc(j.enc[:0], v)
		return bparts.add(p, j.enc)
	}
	err := build(func(v T) error {
		key, ok, err := j.BuildKey(v)
		if err != nil {
			return err
		}
		if bparts != nil {
			return toDisk(v, key, ok)
		}
		e := &Entry[T]{Row: v}
		entries = append(entries, e)
		if ok {
			table[key] = append(table[key], e)
		}
		used += j.C.Size(v) + int64(len(key)) + entryOverhead
		if used <= j.MemLimit {
			return nil
		}
		if depth >= MaxDepth {
			if used > 4*j.MemLimit {
				return ErrSkew
			}
			return nil
		}
		// out of memory: move what is loaded into partitions
		if depth == 0 {
			stats.spills.Add(1)
		}
		if bparts, err = j.M.newParts(Fanout); err != nil {
			return err
		}
		for k, es := range table {
			for _, e := range es {
				if err := toDisk(e.Row, k, true); err != nil {
					return err
				}
			}
		}
		for _, e := range entries {
			if _, ok, _ := j.BuildKey(e.Row); !ok {
				if err := toDisk(e.Row, "", false); err != nil {
					return err
				}
			}
		}
		table, entries, used = nil, nil, 0
		return nil
	})
	if err != nil {
		return err
	}
	if bparts == nil {
		err := probe(func(v T) error {
			key, ok, err := j.ProbeKey(v)
			if err != nil {
				return err
			}
			if !ok {
				return onProbe(v, nil)
			}
			return onProbe(v, table[key])
		})
		if err != nil {
			return err
		}
		return onBuildDone(entries)
	}
	bruns, err := bparts.finish()
	bparts = nil
	if err != nil {
		return err
	}
	defer func() { closeRuns(bruns) }()
	pparts, err := j.M.newParts(Fanout)
	if err != nil {
		return err
	}
	defer func() {
		if pparts != nil {
			pparts.abort()
		}
	}()
	err = probe(func(v T) error {
		key, ok, err := j.ProbeKey(v)
		if err != nil {
			return err
		}
		if !ok {
			return onProbe(v, nil)
		}
		j.enc = j.C.Enc(j.enc[:0], v)
		return pparts.add(j.part(depth, key), j.enc)
	})
	if err != nil {
		return err
	}
	pruns, err := pparts.finish()
	pparts = nil
	if err != nil {
		return err
	}
	defer func() { closeRuns(pruns) }()
	for i := range bruns {
		err := j.run(source(bruns[i], j.C), source(pruns[i], j.C), depth+1, onProbe, onBuildDone)
		if err != nil {
			return err
		}
		bruns[i].Close() // free the disk as parts finish
		pruns[i].Close()
	}
	return nil
}
