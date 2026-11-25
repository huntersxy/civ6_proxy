//go:build windows

package win

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

const (
	windivertLayerNetwork  = 0
	windivertShutdownBoth  = 3
	windivertPacketBufSize = 65575
	windivertAddressSize   = 80
	windivertBitOutbound   = 1 << 17
)

// wdAddress mirrors WINDIVERT_ADDRESS from WinDivert 2.2.2 (80 bytes).
type wdAddress struct {
	Timestamp int64
	Bits      uint32
	Reserved2 uint32
	IfIdx     uint32
	SubIfIdx  uint32
	Reserved  [14]uint32
}

func (a *wdAddress) setLayer(layer uint32) {
	a.Bits = (a.Bits &^ 0xff) | (layer & 0xff)
}

func (a *wdAddress) setOutbound(outbound bool) {
	if outbound {
		a.Bits |= windivertBitOutbound
	} else {
		a.Bits &^= windivertBitOutbound
	}
}

type divertAPI struct {
	open     *syscall.Proc
	recv     *syscall.Proc
	send     *syscall.Proc
	shutdown *syscall.Proc
	close    *syscall.Proc
	setParam *syscall.Proc
}

func loadDivertAPI(dll *syscall.DLL) (*divertAPI, error) {
	lookup := func(name string) (*syscall.Proc, error) {
		proc, err := dll.FindProc(name)
		if err != nil {
			return nil, fmt.Errorf("WinDivert 缺少导出 %s: %w", name, err)
		}
		return proc, nil
	}
	var api divertAPI
	for _, item := range []struct {
		name string
		dst  **syscall.Proc
	}{
		{"WinDivertOpen", &api.open},
		{"WinDivertRecv", &api.recv},
		{"WinDivertSend", &api.send},
		{"WinDivertShutdown", &api.shutdown},
		{"WinDivertClose", &api.close},
		{"WinDivertSetParam", &api.setParam},
	} {
		proc, err := lookup(item.name)
		if err != nil {
			return nil, err
		}
		*item.dst = proc
	}
	return &api, nil
}

// DivertHandle is an open WinDivert NETWORK-layer handle.
type DivertHandle struct {
	api    *divertAPI
	handle uintptr
	closed bool
}

// OpenDivert opens a WinDivert handle with the given filter, extracting and
// loading the embedded driver from dir.
func OpenDivert(dir, filter string) (*DivertHandle, error) {
	dll, err := LoadDriver(dir)
	if err != nil {
		return nil, err
	}
	api, err := loadDivertAPI(dll)
	if err != nil {
		return nil, err
	}
	filterPtr, err := syscall.BytePtrFromString(filter)
	if err != nil {
		return nil, err
	}
	r1, _, callErr := api.open.Call(
		uintptr(unsafe.Pointer(filterPtr)),
		uintptr(windivertLayerNetwork),
		0,
		0,
	)
	handle := uintptr(r1)
	if handle == 0 || handle == ^uintptr(0) {
		if callErr != nil && callErr != syscall.Errno(0) {
			return nil, fmt.Errorf("WinDivertOpen 失败: %w", callErr)
		}
		return nil, errors.New("WinDivertOpen 失败")
	}
	return &DivertHandle{api: api, handle: handle}, nil
}

// Recv blocks until one packet arrives. The returned slice aliases buf.
func (h *DivertHandle) Recv(buf []byte) ([]byte, wdAddress, error) {
	var addr wdAddress
	if len(buf) == 0 {
		return nil, addr, errors.New("empty buffer")
	}
	var recvLen uint32
	r1, _, callErr := h.api.recv.Call(
		h.handle,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(uint32(len(buf))),
		uintptr(unsafe.Pointer(&recvLen)),
		uintptr(unsafe.Pointer(&addr)),
	)
	if r1 == 0 {
		return nil, addr, callError("WinDivertRecv", callErr)
	}
	if recvLen > uint32(len(buf)) {
		return nil, addr, errors.New("WinDivertRecv returned an oversized packet")
	}
	return buf[:recvLen], addr, nil
}

// Send injects one packet with the supplied address metadata.
func (h *DivertHandle) Send(packet []byte, addr *wdAddress) error {
	if len(packet) == 0 {
		return errors.New("empty packet")
	}
	var sent uint32
	r1, _, callErr := h.api.send.Call(
		h.handle,
		uintptr(unsafe.Pointer(&packet[0])),
		uintptr(uint32(len(packet))),
		uintptr(unsafe.Pointer(&sent)),
		uintptr(unsafe.Pointer(addr)),
	)
	if r1 == 0 {
		return callError("WinDivertSend", callErr)
	}
	return nil
}

// Shutdown unblocks pending Recv/Send calls.
func (h *DivertHandle) Shutdown() {
	if h == nil || h.closed {
		return
	}
	_, _, _ = h.api.shutdown.Call(h.handle, windivertShutdownBoth)
}

// Close releases the handle.
func (h *DivertHandle) Close() {
	if h == nil || h.closed {
		return
	}
	h.closed = true
	_, _, _ = h.api.close.Call(h.handle)
}

// TestDriver opens and closes a handle with a filter that matches nothing.
// It validates driver loading end to end and needs administrator rights.
func TestDriver(dir string) error {
	h, err := OpenDivert(dir, "false")
	if err != nil {
		return err
	}
	h.Close()
	return nil
}

func callError(op string, err error) error {
	if err != nil && err != syscall.Errno(0) {
		return fmt.Errorf("%s 失败: %w", op, err)
	}
	return fmt.Errorf("%s 失败", op)
}
