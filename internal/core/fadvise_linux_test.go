package core

import (
	"os"
	"syscall"
)

// fadviseDontNeed drops f from the page cache (POSIX_FADV_DONTNEED).
func fadviseDontNeed(f *os.File) {
	syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, 4, 0, 0)
}
