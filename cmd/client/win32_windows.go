//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// Minimal Win32 helpers retained for UAC prompts, single-instance focus,
// opening local folders/reports, and native log export dialog.
const (
	mbOK              = 0x00000000
	mbYesNo           = 0x00000004
	mbIconError       = 0x00000010
	mbIconQuestion    = 0x00000020
	mbIconInformation = 0x00000040

	ofnOverwritePrompt = 0x00000002
	ofnHideReadOnly    = 0x00000004
	ofnPathMustExist   = 0x00000800

	swShowNormal = 1
)

type openFileNameW struct {
	LStructSize       uint32
	HwndOwner         syscall.Handle
	HInstance         syscall.Handle
	LpstrFilter       *uint16
	LpstrCustomFilter *uint16
	NMaxCustFilter    uint32
	NFilterIndex      uint32
	LpstrFile         *uint16
	NMaxFile          uint32
	LpstrFileTitle    *uint16
	NMaxFileTitle     uint32
	LpstrInitialDir   *uint16
	LpstrTitle        *uint16
	Flags             uint32
	NFileOffset       uint16
	NFileExtension    uint16
	LpstrDefExt       *uint16
	LCustData         uintptr
	LpfnHook          uintptr
	LpTemplateName    *uint16
	PvReserved        unsafe.Pointer
	DwReserved        uint32
	FlagsEx           uint32
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	comdlg32 = syscall.NewLazyDLL("comdlg32.dll")
)

var (
	procShowWindow          = user32.NewProc("ShowWindow")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procShellExecuteW       = shell32.NewProc("ShellExecuteW")
	procGetSaveFileNameW    = comdlg32.NewProc("GetSaveFileNameW")
)

func openFolder(path string) error {
	if testing.Testing() {
		return nil
	}
	cleaned := filepath.Clean(path)
	if info, err := os.Stat(cleaned); err == nil && info.IsDir() {
		cmd := exec.Command("explorer.exe", cleaned)
		if err := cmd.Start(); err == nil {
			go func() { _ = cmd.Wait() }()
			return nil
		}
	}
	operation, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(cleaned)
	if err != nil {
		return err
	}
	result, _, callErr := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(operation)), uintptr(unsafe.Pointer(target)), 0, 0, uintptr(swShowNormal))
	if result > 32 {
		return nil
	}
	if err := exec.Command("explorer.exe", cleaned).Start(); err == nil {
		return nil
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return fmt.Errorf("ShellExecuteW returned %d", result)
}

func openURL(rawURL string) error {
	if testing.Testing() {
		return nil
	}
	trimmed := strings.TrimSpace(rawURL)
	if !strings.HasPrefix(trimmed, "https://") &&
		!strings.HasPrefix(trimmed, "http://") &&
		!strings.HasPrefix(trimmed, "steam://") {
		return fmt.Errorf("不支持的链接协议: %s", rawURL)
	}
	operation, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(trimmed)
	if err != nil {
		return err
	}
	result, _, callErr := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(operation)), uintptr(unsafe.Pointer(target)), 0, 0, uintptr(swShowNormal))
	if result > 32 {
		return nil
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return fmt.Errorf("ShellExecuteW returned %d", result)
}

func findCiv6LaunchExe(libraryRoots []string) string {
	relCandidates := []string{
		filepath.Join("steamapps", "common", "Sid Meier's Civilization VI", "LaunchPad", "LaunchPad.exe"),
		filepath.Join("steamapps", "common", "Sid Meier's Civilization VI", "Base", "Binaries", "Win64Steam", "CivilizationVI_DX12.exe"),
		filepath.Join("steamapps", "common", "Sid Meier's Civilization VI", "Base", "Binaries", "Win64Steam", "CivilizationVI.exe"),
	}
	for _, root := range libraryRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		cleanedRoot := filepath.Clean(root)
		for _, rel := range relCandidates {
			p := filepath.Join(cleanedRoot, rel)
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				return p
			}
		}
	}
	return ""
}

func launchCiv6Executable(libraryRoots []string) error {
	if testing.Testing() {
		return nil
	}
	exePath := findCiv6LaunchExe(libraryRoots)
	if exePath == "" {
		return fmt.Errorf("未找到 Steam 协议处理器或本地 CivilizationVI.exe，请先打开 Steam")
	}
	cmd := exec.Command(exePath)
	cmd.Dir = filepath.Dir(exePath)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func utf16WithNULs(s string) []uint16 {
	out := make([]uint16, 0, len(s)+1)
	for _, r := range s {
		if r < 0x10000 {
			out = append(out, uint16(r))
		} else {
			r -= 0x10000
			out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		}
	}
	return append(out, 0)
}

func saveFileDialog(owner syscall.Handle, defaultName string) (string, bool) {
	var fileBuf [2048]uint16
	utf, err := syscall.UTF16FromString(defaultName)
	if err != nil || len(utf) >= len(fileBuf) {
		utf = []uint16{'o', 'u', 't', '.', 'l', 'o', 'g', 0}
	}
	copy(fileBuf[:], utf)
	filter := utf16WithNULs("日志文件 (*.log)\x00*.log\x00所有文件 (*.*)\x00*.*\x00")
	title, _ := syscall.UTF16FromString("导出运行日志")
	defExt, _ := syscall.UTF16FromString("log")
	ofn := openFileNameW{
		LStructSize:  uint32(unsafe.Sizeof(openFileNameW{})),
		HwndOwner:    owner,
		LpstrFilter:  &filter[0],
		NFilterIndex: 1,
		LpstrFile:    &fileBuf[0],
		NMaxFile:     uint32(len(fileBuf)),
		LpstrTitle:   &title[0],
		Flags:        ofnOverwritePrompt | ofnPathMustExist | ofnHideReadOnly,
		LpstrDefExt:  &defExt[0],
	}
	r, _, _ := procGetSaveFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return "", false
	}
	return syscall.UTF16ToString(fileBuf[:]), true
}
