//go:build windows

package main

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// Win32 constants used by the native UI.
const (
	wsOverlapped       = 0x00000000
	wsCaption          = 0x00C00000
	wsSysMenu          = 0x00080000
	wsMinimizeBox      = 0x00020000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsBorder           = 0x00800000
	wsVScroll          = 0x00200000
	wsClipChildren     = 0x02000000
	wsClipSiblings     = 0x04000000
	wsExClientEdge     = 0x00000200
	esAutoHScroll      = 0x00000080
	esAutoVScroll      = 0x00000040
	esMultiline        = 0x00000004
	esReadOnly         = 0x00000800
	bsPushButton       = 0x00000000
	lvsReport          = 0x00000001
	lvsSingleSel       = 0x00000004
	lvsShowSelAlways   = 0x00000008
	lvsExFullRowSelect = 0x00000020
	lvsExGridLines     = 0x00000001
	lvsExDoubleBuffer  = 0x00010000

	wmSetFont   = 0x0030
	wmCommand   = 0x0111
	wmTimer     = 0x0113
	wmClose     = 0x0010
	wmDestroy   = 0x0002
	wmAppInvoke = 0x8001

	emSetSel                    = 0x00B1
	emReplaceSel                = 0x00C2
	emSetReadOnly               = 0x00CF
	emScrollCaret               = 0x00B7
	lvmFirst                    = 0x1000
	lvmInsertColumnW            = lvmFirst + 97
	lvmInsertItemW              = lvmFirst + 77
	lvmSetItemTextW             = lvmFirst + 116
	lvmDeleteAllItems           = lvmFirst + 9
	lvmSetExtendedListViewStyle = lvmFirst + 54

	lvcfFmt      = 0x00000001
	lvcfWidth    = 0x00000002
	lvcfText     = 0x00000004
	lvcfSubItem  = 0x00000008
	lvcfmtLeft   = 0x00000000
	lvcfmtCenter = 0x00000002
	lvifText     = 0x00000001

	iccListViewClasses = 0x00000001
	iccStandardClasses = 0x00004000

	mbOK              = 0x00000000
	mbYesNo           = 0x00000004
	mbIconError       = 0x00000010
	mbIconQuestion    = 0x00000020
	mbIconInformation = 0x00000040

	ofnOverwritePrompt = 0x00000002
	ofnHideReadOnly    = 0x00000004
	ofnPathMustExist   = 0x00000800

	cfUnicodeText = 13
	gmemMoveable  = 0x0002

	logPixelsY     = 90
	defaultCharset = 1
	clearType      = 5
	fwNormal       = 400
	fwBold         = 700
	swShowNormal   = 1
	smCxScreen     = 0
	smCyScreen     = 1
	bnClicked      = 0
)

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     syscall.Handle
	HIcon         syscall.Handle
	HCursor       syscall.Handle
	HbrBackground syscall.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       syscall.Handle
}

type rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type msgW struct {
	HWnd     syscall.Handle
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       struct{ X, Y int32 }
	LPrivate uint32
}

type initCommonControlsEx struct {
	Size uint32
	ICC  uint32
}

type lvColumnW struct {
	Mask      uint32
	Fmt       int32
	Cx        int32
	PszText   *uint16
	CchText   int32
	ISubItem  int32
	IImage    int32
	IOrder    int32
	CxMin     int32
	CxDefault int32
	CxIdeal   int32
}

type lvItemW struct {
	Mask      uint32
	IItem     int32
	ISubItem  int32
	State     uint32
	StateMask uint32
	PszText   *uint16
	CchText   int32
	IImage    int32
	LParam    uintptr
	IIndent   int32
	IGroupID  int32
	CColumns  uint32
	PuColumns *uint32
	PiColFmt  *int32
	IGroup    int32
}

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
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	comctl32 = syscall.NewLazyDLL("comctl32.dll")
	comdlg32 = syscall.NewLazyDLL("comdlg32.dll")
)

var (
	procRegisterClassExW     = user32.NewProc("RegisterClassExW")
	procCreateWindowExW      = user32.NewProc("CreateWindowExW")
	procDefWindowProcW       = user32.NewProc("DefWindowProcW")
	procDestroyWindow        = user32.NewProc("DestroyWindow")
	procShowWindow           = user32.NewProc("ShowWindow")
	procUpdateWindow         = user32.NewProc("UpdateWindow")
	procGetMessageW          = user32.NewProc("GetMessageW")
	procTranslateMessage     = user32.NewProc("TranslateMessage")
	procDispatchMessageW     = user32.NewProc("DispatchMessageW")
	procPostQuitMessage      = user32.NewProc("PostQuitMessage")
	procPostMessageW         = user32.NewProc("PostMessageW")
	procSendMessageW         = user32.NewProc("SendMessageW")
	procSetWindowTextW       = user32.NewProc("SetWindowTextW")
	procGetWindowTextW       = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW = user32.NewProc("GetWindowTextLengthW")
	procEnableWindow         = user32.NewProc("EnableWindow")
	procSetFocus             = user32.NewProc("SetFocus")
	procSetTimer             = user32.NewProc("SetTimer")
	procKillTimer            = user32.NewProc("KillTimer")
	procMessageBoxW          = user32.NewProc("MessageBoxW")
	procFindWindowW          = user32.NewProc("FindWindowW")
	procSetForegroundWindow  = user32.NewProc("SetForegroundWindow")
	procGetSystemMetrics     = user32.NewProc("GetSystemMetrics")
	procAdjustWindowRectEx   = user32.NewProc("AdjustWindowRectEx")
	procGetDC                = user32.NewProc("GetDC")
	procReleaseDC            = user32.NewProc("ReleaseDC")
	procOpenClipboard        = user32.NewProc("OpenClipboard")
	procEmptyClipboard       = user32.NewProc("EmptyClipboard")
	procSetClipboardData     = user32.NewProc("SetClipboardData")
	procCloseClipboard       = user32.NewProc("CloseClipboard")
	procGetModuleHandleW     = kernel32.NewProc("GetModuleHandleW")
	procGlobalAlloc          = kernel32.NewProc("GlobalAlloc")
	procGlobalLock           = kernel32.NewProc("GlobalLock")
	procGlobalUnlock         = kernel32.NewProc("GlobalUnlock")
	procGlobalFree           = kernel32.NewProc("GlobalFree")
	procRtlMoveMemory        = kernel32.NewProc("RtlMoveMemory")
	procGetDeviceCaps        = gdi32.NewProc("GetDeviceCaps")
	procCreateSolidBrush     = gdi32.NewProc("CreateSolidBrush")
	procSetBkMode            = gdi32.NewProc("SetBkMode")
	procCreateFontW          = gdi32.NewProc("CreateFontW")
	procDeleteObject         = gdi32.NewProc("DeleteObject")
	procInitCommonControlsEx = comctl32.NewProc("InitCommonControlsEx")
	procGetSaveFileNameW     = comdlg32.NewProc("GetSaveFileNameW")
)

var uiDPI uint32 = 96

func initDPI() {
	dc, _, _ := procGetDC.Call(0)
	if dc == 0 {
		return
	}
	dpi, _, _ := procGetDeviceCaps.Call(dc, logPixelsY)
	procReleaseDC.Call(0, dc)
	if dpi >= 96 {
		uiDPI = uint32(dpi)
	}
}

func px(v int) int32 {
	return int32(float64(v)*float64(uiDPI)/96.0 + 0.5)
}

func createFont(pt int, bold bool, face string) syscall.Handle {
	weight := uintptr(fwNormal)
	if bold {
		weight = fwBold
	}
	height := -int32(float64(pt)*float64(uiDPI)/72.0 + 0.5)
	facePtr, _ := syscall.UTF16PtrFromString(face)
	r, _, _ := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, weight,
		0, 0, 0, defaultCharset, 0, 0, clearType, 0,
		uintptr(unsafe.Pointer(facePtr)),
	)
	return syscall.Handle(r)
}

func setFont(control, font syscall.Handle) {
	procSendMessageW.Call(uintptr(control), wmSetFont, uintptr(font), 1)
}

func createChild(parent syscall.Handle, exStyle uint32, class, text string, style uint32, x, y, w, h int32, id uint32) syscall.Handle {
	classPtr, _ := syscall.UTF16PtrFromString(class)
	var textPtr *uint16
	if text != "" {
		textPtr, _ = syscall.UTF16PtrFromString(text)
	}
	hInstance, _, _ := procGetModuleHandleW.Call(0)
	r, _, _ := procCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(classPtr)),
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(style|wsChild|wsVisible),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		uintptr(parent), uintptr(id), hInstance, 0,
	)
	return syscall.Handle(r)
}

func windowText(h syscall.Handle) string {
	n, _, _ := procGetWindowTextLengthW.Call(uintptr(h))
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func setWindowText(h syscall.Handle, text string) {
	ptr, _ := syscall.UTF16PtrFromString(text)
	procSetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(ptr)))
}

func enableWindow(h syscall.Handle, enabled bool) {
	v := uintptr(0)
	if enabled {
		v = 1
	}
	procEnableWindow.Call(uintptr(h), v)
}

func msgBox(owner syscall.Handle, title, text string, flags uint32) int {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	textPtr, _ := syscall.UTF16PtrFromString(text)
	r, _, _ := procMessageBoxW.Call(
		uintptr(owner),
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)
	return int(r)
}

func copyToClipboard(owner syscall.Handle, text string) error {
	ok, _, callErr := procOpenClipboard.Call(uintptr(owner))
	if ok == 0 {
		return fmt.Errorf("打开剪贴板失败: %v", callErr)
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	utf, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	size := uintptr(len(utf) * 2)
	h, _, allocErr := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return fmt.Errorf("GlobalAlloc 失败: %v", allocErr)
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h)
		return errors.New("GlobalLock 失败")
	}
	procRtlMoveMemory.Call(ptr, uintptr(unsafe.Pointer(&utf[0])), size)
	procGlobalUnlock.Call(h)
	if r, _, setErr := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData 失败: %v", setErr)
	}
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
