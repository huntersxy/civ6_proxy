//go:build !windows

package logsync

import (
	"os"
	"syscall"
)

type fileIdentity struct {
	valid bool
	dev   uint64
	ino   uint64
}

func (a fileIdentity) equal(b fileIdentity) bool {
	return a.valid && b.valid && a.dev == b.dev && a.ino == b.ino
}

func openSharedRead(path string) (*os.File, error) {
	return os.Open(path)
}

func getFileIdentity(f *os.File) fileIdentity {
	info, err := f.Stat()
	if err != nil {
		return fileIdentity{}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return fileIdentity{}
	}
	return fileIdentity{
		valid: true,
		dev:   uint64(st.Dev),
		ino:   uint64(st.Ino),
	}
}
