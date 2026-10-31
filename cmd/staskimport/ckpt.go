package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Fingerprint identifies the input a checkpoint belongs to.
type Fingerprint struct {
	Size int64  `json:"size"`
	Head string `json:"head_sha256"` // of the first MiB
	Spec string `json:"spec"`
}

func (f Fingerprint) describe(now Fingerprint) string {
	switch {
	case f.Spec != now.Spec:
		return "the spec changed"
	case f.Size != now.Size:
		return fmt.Sprintf("the file size changed from %d to %d", f.Size, now.Size)
	}
	return "the file content changed"
}

func fingerprint(path, spec string) (Fingerprint, error) {
	f, err := os.Open(path)
	if err != nil {
		return Fingerprint{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Fingerprint{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, 1<<20)); err != nil {
		return Fingerprint{}, err
	}
	return Fingerprint{st.Size(), hex.EncodeToString(h.Sum(nil)[:12]), spec}, nil
}

// Checkpoint is the progress of one file: every data row up to RowsDone is
// committed (or in the error report). Rewritten after each committed batch.
type Checkpoint struct {
	Fingerprint Fingerprint `json:"fingerprint"`
	RowsDone    int64       `json:"rows_done"`
	Counts      Counts      `json:"counts"`
	Complete    bool        `json:"complete"`
	Started     time.Time   `json:"started"`
	Updated     time.Time   `json:"updated"`
}

func loadCheckpoint(path string) (*Checkpoint, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("checkpoint %s is unreadable (%v): delete it or use -restart", path, err)
	}
	return &c, nil
}

// save writes the checkpoint atomically: temp file, fsync, rename, fsync of
// the directory.
func (c *Checkpoint) save(path string) error {
	c.Updated = time.Now().UTC()
	b, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
