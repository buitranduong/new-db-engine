package pindex

import "os"

// File is the storage the index needs: positioned reads and writes, a
// durability barrier and the current size. *os.File satisfies it; tests use a
// fault-injecting in-memory implementation to model crashes.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Close() error
	Size() (int64, error)
}

type osFile struct{ *os.File }

func (f osFile) Size() (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
