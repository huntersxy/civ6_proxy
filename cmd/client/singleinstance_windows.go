//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

var singleInstanceMutex syscall.Handle

// acquireSingleInstance returns false when another client already owns the
// per-session mutex.
func acquireSingleInstance() bool {
	name, _ := syscall.UTF16PtrFromString(`Local\Civ6RelayClient`)
	proc := kernel32.NewProc("CreateMutexW")
	h, _, err := proc.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return true
	}
	if err == syscall.ERROR_ALREADY_EXISTS {
		_ = syscall.CloseHandle(syscall.Handle(h))
		return false
	}
	singleInstanceMutex = syscall.Handle(h)
	return true
}

// focusExistingWindow brings the already running client to the foreground.
func focusExistingWindow() {
	title, _ := syscall.UTF16PtrFromString(uiTitle)
	for i := 0; i < 10; i++ {
		hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
		if hwnd != 0 {
			procShowWindow.Call(hwnd, swShowNormal)
			procSetForegroundWindow.Call(hwnd)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	messageBox("Civ6Relay", "客户端已经在运行，但没有找到窗口。请在任务管理器中结束 Civ6RelayClient.exe 后重试。", mbOK|mbIconInformation)
}
