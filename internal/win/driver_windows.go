//go:build windows

package win

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// The WinDivert binaries are the official 2.2.2-A x64 build. They are embedded
// so that the client ships as a single exe and extracted next to each other at
// runtime (WinDivert.dll locates WinDivert64.sys in its own directory).
//
//go:embed assets/WinDivert64.dll
var windivertDLL []byte

//go:embed assets/WinDivert64.sys
var windivertSYS []byte

//go:embed assets/WinDivert-LICENSE.txt
var windivertLicense []byte

const (
	windivertDLLName     = "WinDivert64.dll"
	windivertSYSName     = "WinDivert64.sys"
	windivertLicenseName = "WinDivert-LICENSE.txt"
	windivertVersionDir  = "windivert-2.2.2"
)

var loadedDriverDLL *syscall.DLL

// EnsureDriver extracts the embedded driver files and returns the directory.
// Existing files are left untouched when their content already matches.
func EnsureDriver() (string, error) {
	dir := DriverDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// Fall back to the per-user location when ProgramData is not writable.
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			dir = filepath.Join(local, "Civ6Relay", windivertVersionDir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", fmt.Errorf("创建驱动目录失败: %w", err)
			}
		} else {
			return "", fmt.Errorf("创建驱动目录失败: %w", err)
		}
	}
	for _, item := range []struct {
		name string
		data []byte
	}{
		{windivertDLLName, windivertDLL},
		{windivertSYSName, windivertSYS},
		{windivertLicenseName, windivertLicense},
	} {
		if err := writeIfChanged(filepath.Join(dir, item.name), item.data); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func writeIfChanged(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换 %s 失败: %w", filepath.Base(path), err)
	}
	return nil
}

// DriverHashes returns the expected hashes for diagnostics.
func DriverHashes() (dll, sys [32]byte) {
	return sha256.Sum256(windivertDLL), sha256.Sum256(windivertSYS)
}

// LoadDriver loads WinDivert64.dll from dir.
func LoadDriver(dir string) (*syscall.DLL, error) {
	if loadedDriverDLL != nil {
		return loadedDriverDLL, nil
	}
	path := filepath.Join(dir, windivertDLLName)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("驱动文件不存在: %w", err)
	}
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return nil, fmt.Errorf("加载 WinDivert 失败: %w", err)
	}
	loadedDriverDLL = dll
	return dll, nil
}

// DriverLicense returns the bundled WinDivert license text.
func DriverLicense() string {
	return string(windivertLicense)
}

var errDriverNotLoaded = errors.New("WinDivert 驱动未加载")

// DriverDir returns the preferred directory for the extracted driver files.
func DriverDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = os.Getenv("LOCALAPPDATA")
	}
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "Civ6Relay", windivertVersionDir)
}

// FileStatus describes one extracted driver file.
type FileStatus struct {
	Path     string
	Exists   bool
	Size     int64
	SHA256   string
	Match    bool
	Verified bool
}

// Describe returns a detailed one-line description including the hash.
func (f FileStatus) Describe() string {
	if !f.Exists {
		return "缺失"
	}
	if f.SHA256 == "" {
		return fmt.Sprintf("存在, %d 字节（无法读取/计算哈希）", f.Size)
	}
	if !f.Verified {
		return fmt.Sprintf("存在, %d 字节, SHA256=%s", f.Size, f.SHA256)
	}
	match := "与官方一致"
	if !f.Match {
		match = "与官方不一致"
	}
	return fmt.Sprintf("存在, %d 字节, SHA256=%s, %s", f.Size, f.SHA256, match)
}

// DescribeShort omits the hash, for UI message boxes.
func (f FileStatus) DescribeShort() string {
	if !f.Exists {
		return "缺失"
	}
	if !f.Verified {
		return fmt.Sprintf("存在, %d 字节", f.Size)
	}
	if f.Match {
		return fmt.Sprintf("存在, %d 字节, 与官方一致", f.Size)
	}
	return fmt.Sprintf("存在, %d 字节, 与官方不一致", f.Size)
}

// DriverStatus is a snapshot of the extracted driver files.
type DriverStatus struct {
	Dir      string
	DLL      FileStatus
	SYS      FileStatus
	License  FileStatus
	Complete bool
}

// InspectDriver checks the extracted files against the embedded hashes.
func InspectDriver(dir string) DriverStatus {
	if dir == "" {
		dir = DriverDir()
	}
	expectedDLL, expectedSYS := DriverHashes()
	status := DriverStatus{
		Dir:     dir,
		DLL:     inspectFile(filepath.Join(dir, windivertDLLName), &expectedDLL),
		SYS:     inspectFile(filepath.Join(dir, windivertSYSName), &expectedSYS),
		License: inspectFile(filepath.Join(dir, windivertLicenseName), nil),
	}
	status.Complete = status.DLL.Exists && status.DLL.Match && status.SYS.Exists && status.SYS.Match
	return status
}

func inspectFile(path string, expected *[32]byte) FileStatus {
	status := FileStatus{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		return status
	}
	status.Exists = true
	status.Size = info.Size()
	data, err := os.ReadFile(path)
	if err != nil {
		return status
	}
	sum := sha256.Sum256(data)
	status.SHA256 = hex.EncodeToString(sum[:])
	if expected != nil {
		status.Verified = true
		status.Match = sum == *expected
	}
	return status
}
