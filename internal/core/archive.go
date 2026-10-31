package core

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"stask/internal/seccrypt"
)

// Continuous archiving and point-in-time recovery, as PostgreSQL does
// with archive_command and recovery_target_time
// (https://www.postgresql.org/docs/current/continuous-archiving.html):
//
//   - with Options.ArchiveDir, every sealed log segment is copied there
//     (file written under a temporary name, fsynced, renamed). GC removes
//     no segment before it is archived, so the archive holds every record
//     written since archiving started;
//   - a base backup (repl.BaseBackup: a checkpoint and the log after it)
//     plus the archive restores the database to any point after the base
//     backup's checkpoint: Options.RestoreArchive copies the archived
//     segments the backup lacks into its log, and recovery replays up to
//     Options.RecoveryTargetLSN or RecoveryTargetTS, then discards the
//     rest of the log and the engine continues from there.
//
// The log is self-describing (each record carries its LSN and commit
// time), so no separate history or manifest is kept.

func archiveName(dir string, seg uint32) string {
	return filepath.Join(dir, fmt.Sprintf("%08d.wal", seg))
}

// archivedSegs lists the segments in an archive directory.
func archivedSegs(dir string) ([]uint32, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, e := range ents {
		n, ok := strings.CutSuffix(e.Name(), ".wal")
		if !ok {
			continue
		}
		if s, err := strconv.ParseUint(n, 10, 32); err == nil {
			out = append(out, uint32(s))
		}
	}
	return out, nil
}

// archiver copies each sealed segment to the archive, oldest first.
func (e *Engine) archiver() {
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
		if e.opts.ArchiveLimit != nil {
			w = min(w, e.opts.ArchiveLimit())
		}
		for _, seg := range e.log.Segments() {
			if seg >= w || seg <= uint32(e.archived.Load()) && e.archived.Load() >= 0 {
				continue
			}
			t0 := time.Now()
			n, err := e.archiveSegment(seg)
			if err != nil {
				// retried every second; GC keeps the segment meanwhile
				e.Stats.ArchiveFailures.Add(1)
				e.logf("archive segment %d: %v", seg, err)
				break
			}
			e.archived.Store(int64(seg))
			e.Stats.ArchivedSegs.Add(1)
			e.logf("archived segment %d: %d bytes in %v", seg, n, time.Since(t0).Round(time.Millisecond))
		}
	}
}

// archiveSegment copies sealed segment seg (raw or compressed) into the
// archive. An existing archived file is left alone if it has the same
// content: a restored database must archive to a new directory, as its
// segments after the recovery target differ from the archived ones.
func (e *Engine) archiveSegment(seg uint32) (int64, error) {
	dst := archiveName(e.opts.ArchiveDir, seg)
	var data []byte
	buf := make([]byte, 1<<20)
	for off := int64(0); off < e.log.SegMax(); {
		n, err := e.log.ReadAt(seg, off, buf[:min(int64(len(buf)), e.log.SegMax()-off)])
		data = append(data, buf[:n]...)
		off += int64(n)
		if errors.Is(err, io.EOF) || n == 0 {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	// trailing zeros (the unused end of the segment) are not archived
	end := len(data)
	for end > 0 && data[end-1] == 0 {
		end--
	}
	data = data[:end]
	if old, err := os.ReadFile(dst); err == nil {
		if key := e.opts.ArchiveKey; len(key) > 0 {
			if pt, derr := seccrypt.Open(key, old); derr == nil {
				old = pt
			}
		}
		if string(old) == string(data) {
			return 0, nil
		}
		return 0, fmt.Errorf("%s exists with other content: a restored database archives to a new -archive-dir", dst)
	}
	if key := e.opts.ArchiveKey; len(key) > 0 {
		// authenticated encryption of the segment (docs/23): a segment is
		// the customer data, and an archive is copied to places the data
		// directory never is
		sealed, serr := seccrypt.Seal(key, data)
		if serr != nil {
			return 0, serr
		}
		data = sealed
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err == nil {
		err = syncDirPath(e.opts.ArchiveDir)
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return int64(len(data)), nil
}

// initArchive sets how far the archive goes (the newest segment in it).
func (e *Engine) initArchive() error {
	e.archived.Store(-1)
	if e.opts.ArchiveDir == "" {
		return nil
	}
	if err := os.MkdirAll(e.opts.ArchiveDir, 0o755); err != nil {
		return err
	}
	segs, err := archivedSegs(e.opts.ArchiveDir)
	if err != nil {
		return err
	}
	// segments of this log older than the archive's newest were archived
	// or removed already; the first segment of an empty archive is the
	// oldest one still in the log
	for _, s := range segs {
		e.archived.Store(max(e.archived.Load(), int64(s)))
	}
	return nil
}

// archiveHolds reports whether GC must keep segment seg for the archiver.
func (e *Engine) archiveHolds(seg uint32) bool {
	return e.opts.ArchiveDir != "" && int64(seg) > e.archived.Load()
}

// restoreFromArchive copies into the log directory the archived segments
// newer than the log's newest (a base backup's), sized to segMax.
func restoreFromArchive(walDir, archive string, segMax int64, key []byte, allowPlain bool, logf func(string, ...any)) error {
	segs, err := archivedSegs(archive)
	if err != nil {
		return err
	}
	have := map[uint32]bool{}
	newest := int64(-1)
	if ents, err := os.ReadDir(walDir); err == nil {
		for _, e := range ents {
			n := strings.TrimSuffix(strings.TrimSuffix(e.Name(), "z"), ".wal")
			if s, err := strconv.ParseUint(n, 10, 32); err == nil {
				have[uint32(s)] = true
				newest = max(newest, int64(s))
			}
		}
	}
	if err := os.MkdirAll(walDir, 0o755); err != nil {
		return err
	}
	n := 0
	for _, s := range segs {
		// the newest local segment may hold less than its archived copy
		// (a base backup taken while it was written)
		if int64(s) < newest || have[s] && int64(s) != newest {
			continue
		}
		data, err := os.ReadFile(archiveName(archive, s))
		if err != nil {
			return err
		}
		switch enc := seccrypt.IsEncrypted(data); {
		case enc && len(key) == 0:
			return fmt.Errorf("archive segment %d is encrypted: give the archive key (-archive-key-file or $%s)", s, seccrypt.EnvKey)
		case enc:
			if data, err = seccrypt.Open(key, data); err != nil {
				return fmt.Errorf("archive segment %d: %w", s, err)
			}
		case len(key) > 0 && !allowPlain:
			return fmt.Errorf("archive segment %d is not encrypted but a key is configured: refusing a plaintext segment (-archive-allow-plain for an archive that predates encryption)", s)
		}
		dst := filepath.Join(walDir, fmt.Sprintf("%08d.wal", s))
		os.Remove(dst + "z")
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Truncate(segMax)
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		n++
	}
	logf("restore: copied %d segment(s) from archive %s", n, archive)
	return syncDirPath(walDir)
}
