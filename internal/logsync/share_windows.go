//go:build windows

package logsync

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

type fileIdentity struct {
	valid        bool
	volSerial    uint32
	indexHigh    uint32
	indexLow     uint32
	creationHigh uint32
	creationLow  uint32
}

func (a fileIdentity) equal(b fileIdentity) bool {
	return a.valid && b.valid &&
		a.volSerial == b.volSerial &&
		a.indexHigh == b.indexHigh &&
		a.indexLow == b.indexLow &&
		a.creationHigh == b.creationHigh &&
		a.creationLow == b.creationLow
}

// openSharedRead opens path for reading with full read/write/delete sharing so
// Civ 6 and Civ6Relay can freely write, truncate, rename, or rotate log files.
func openSharedRead(path string) (*os.File, error) {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func getFileIdentity(f *os.File) fileIdentity {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return fileIdentity{}
	}
	return fileIdentity{
		valid:        true,
		volSerial:    info.VolumeSerialNumber,
		indexHigh:    info.FileIndexHigh,
		indexLow:     info.FileIndexLow,
		creationHigh: info.CreationTime.HighDateTime,
		creationLow:  info.CreationTime.LowDateTime,
	}
}
