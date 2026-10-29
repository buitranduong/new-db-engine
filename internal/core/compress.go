package core

import (
	"time"

	"stask/internal/wal"
)

// compressor rewrites sealed log segments in compressed form (see
// wal/zseg.go) once Options.CompressKeepRaw newer segments exist: the
// segments recent reads and replicas mostly touch stay raw and mapped,
// older ones shrink. One segment at a time, in the background.
func (e *Engine) compressor() {
	defer e.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
		}
		if e.Failed() != nil {
			continue
		}
		w := e.log.WriteSeg()
		for _, seg := range e.log.Segments() {
			if seg+uint32(e.opts.CompressKeepRaw) >= w || e.log.IsCompressed(seg) {
				continue
			}
			select {
			case <-e.stop:
				return
			default:
			}
			t0 := time.Now()
			raw, comp, err := e.log.Compress(seg, e.opts.Compression)
			switch {
			case wal.IsBusy(err):
				continue // a reader holds the raw file: next round
			case err != nil:
				e.logf("compress segment %d: %v", seg, err)
				continue
			}
			e.Stats.CompressedSegs.Add(1)
			e.Stats.CompressRawBytes.Add(raw)
			e.Stats.CompressFileBytes.Add(comp)
			e.logf("compressed segment %d: %d -> %d bytes (%.2fx) in %v", seg, raw, comp, float64(raw)/float64(max(comp, 1)), time.Since(t0))
		}
	}
}
