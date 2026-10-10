//go:build windows

package gui

// System-tray (notification area) icon, IDM-style: closing the main window hides
// it to the tray instead of quitting. Native Shell_NotifyIcon — no extra deps,
// matching the rest of this package's raw Win32 usage. A single tray instance per
// process (package-level state); the icon is the snail embedded in the .exe.

import (
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	shell32             = syscall.NewLazyDLL("shell32.dll")
	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	procExtractIcon     = shell32.NewProc("ExtractIconW")

	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	procAppendMenu       = user32.NewProc("AppendMenuW")
	procCheckMenuItem    = user32.NewProc("CheckMenuItem")
	procTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	procPostMessage      = user32.NewProc("PostMessageW")
	procRegisterWinMsg   = user32.NewProc("RegisterWindowMessageW")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procGetMessage       = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")

	procLoadIcon        = user32.NewProc("LoadIconW")
	procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
)

const idiApplication = 32512 // default app icon, if the exe has none to extract

const (
	wmTrayCallback  = 0x0400 + 1 // WM_APP+1
	wmCommandMsg    = 0x0111     // WM_COMMAND
	wmLButtonUp     = 0x0202
	wmLButtonDblclk = 0x0203
	wmRButtonUp     = 0x0205

	nimAdd     = 0x0
	nimModify  = 0x1
	nimDelete  = 0x2
	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04

	menuOpenID    = 1
	menuExitID    = 2
	menuStartupID = 3

	mfString       = 0x0000
	mfSeparator    = 0x0800
	mfChecked      = 0x0008 // with MF_BYCOMMAND (0)
	tpmRightButton = 0x0002
	wmNull         = 0x0000
)

type notifyIconData struct {
	cbSize            uint32
	hWnd              uintptr
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             uintptr
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uVersionOrTimeout uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          [16]byte
	hBalloonIcon      uintptr
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type trayMsg struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       struct{ X, Y int32 }
	lPrivate uint32
}

var (
	trayOnOpen func()
	trayOnExit func()
	trayNID    notifyIconData
	trayMenu   uintptr
	trayActive atomic.Bool // the icon is in the notification area right now
	trayGone   atomic.Bool // RemoveTray ran: never re-add (the app is exiting)

	// "Start with Windows" check item; nil = not offered.
	trayStartupGet func() bool
	trayStartupSet func(on bool) error

	// Explorer broadcasts this when the taskbar is (re)created — after it
	// restarts, or when it comes up after an app that started at sign-in.
	wmTaskbarCreated uintptr
)

// SetTrayStartupToggle adds a "Start with Windows" check item to the tray menu:
// get reports the current state (read each time the menu opens), set changes
// it. Call before RunTray.
func SetTrayStartupToggle(get func() bool, set func(on bool) error) {
	trayStartupGet, trayStartupSet = get, set
}

// TrayActive reports whether the tray icon is showing, i.e. whether a hidden
// window can be brought back from it.
func TrayActive() bool { return trayActive.Load() }

// addTrayIcon puts the icon in the notification area; false if the taskbar
// isn't there to take it. TaskbarCreated also arrives while the icon still
// exists (Windows sends it on display-scale changes too), where adding it
// again fails — refreshing it in place then succeeds.
func addTrayIcon() bool {
	if trayGone.Load() {
		return false
	}
	r, _, _ := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&trayNID)))
	if r == 0 {
		r, _, _ = procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&trayNID)))
	}
	trayActive.Store(r != 0)
	return r != 0
}

var trayWndProc = syscall.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
	if wmTaskbarCreated != 0 && msg == wmTaskbarCreated {
		addTrayIcon() // the old taskbar took our icon with it
		return 0
	}
	switch msg {
	case wmTrayCallback:
		switch lParam {
		case wmLButtonUp, wmLButtonDblclk:
			if trayOnOpen != nil {
				trayOnOpen()
			}
		case wmRButtonUp:
			showTrayMenu(hwnd)
		}
		return 0
	case wmCommandMsg:
		switch wParam & 0xffff {
		case menuOpenID:
			if trayOnOpen != nil {
				trayOnOpen()
			}
		case menuExitID:
			if trayOnExit != nil {
				trayOnExit()
			}
		case menuStartupID:
			if trayStartupGet != nil && trayStartupSet != nil {
				trayStartupSet(!trayStartupGet())
			}
		}
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProc.Call(hwnd, msg, wParam, lParam)
	return ret
})

// showTrayMenu pops the right-click menu. SetForegroundWindow first so the menu
// dismisses correctly when the user clicks elsewhere (a Win32 tray-menu quirk).
func showTrayMenu(hwnd uintptr) {
	if trayMenu == 0 {
		return
	}
	if trayStartupGet != nil {
		var check uintptr // MF_UNCHECKED
		if trayStartupGet() {
			check = mfChecked
		}
		procCheckMenuItem.Call(trayMenu, menuStartupID, check)
	}
	procSetForegroundWindow.Call(hwnd)
	var pt struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procTrackPopupMenu.Call(trayMenu, tpmRightButton, uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	procPostMessage.Call(hwnd, wmNull, 0, 0) // the other half of the dismiss quirk fix
}

// RunTray creates the tray icon and runs its message loop (BLOCKS — call in a
// goroutine). onOpen fires on left click / "Open"; onExit on "Exit". It signals
// ready<-true once the icon is up, or ready<-false if it couldn't be shown (so
// the caller can fall back to a plain minimize). When the taskbar isn't there
// yet (an app started at sign-in can beat it) or Explorer restarts later, the
// icon is added as soon as the taskbar announces itself. The loop ends when the
// process exits.
func RunTray(tooltip string, onOpen, onExit func(), ready chan<- bool) {
	runtime.LockOSThread()
	trayOnOpen, trayOnExit = onOpen, onExit

	hInst, _, _ := procGetModuleHandle.Call(0)

	// First icon embedded in the .exe = the D BOX app icon.
	var hIcon uintptr
	if exe, err := os.Executable(); err == nil {
		if p, err := syscall.UTF16PtrFromString(exe); err == nil {
			hIcon, _, _ = procExtractIcon.Call(hInst, uintptr(unsafe.Pointer(p)), 0)
		}
	}
	if hIcon == 0 || hIcon == 1 { // ExtractIcon returns 1 when the file has no icons
		hIcon, _, _ = procLoadIcon.Call(0, idiApplication)
	}

	className, _ := syscall.UTF16PtrFromString("DBoxTrayClass")
	wc := wndClassEx{lpfnWndProc: trayWndProc, hInstance: hInst, hIcon: hIcon, hIconSm: hIcon, lpszClassName: className}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if atom, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		if ready != nil {
			ready <- false
		}
		return
	}

	winName, _ := syscall.UTF16PtrFromString("D BOX")
	hwnd, _, _ := procCreateWindowEx.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(winName)),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		if ready != nil {
			ready <- false
		}
		return
	}

	if tc, err := syscall.UTF16PtrFromString("TaskbarCreated"); err == nil {
		wmTaskbarCreated, _, _ = procRegisterWinMsg.Call(uintptr(unsafe.Pointer(tc)))
	}

	trayMenu, _, _ = procCreatePopupMenu.Call()
	appendItem := func(id uintptr, text string) {
		if p, err := syscall.UTF16PtrFromString(text); err == nil {
			procAppendMenu.Call(trayMenu, mfString, id, uintptr(unsafe.Pointer(p)))
		}
	}
	appendItem(menuOpenID, "Open D BOX")
	if trayStartupGet != nil && trayStartupSet != nil {
		procAppendMenu.Call(trayMenu, mfSeparator, 0, 0)
		appendItem(menuStartupID, "Start with Windows")
		procAppendMenu.Call(trayMenu, mfSeparator, 0, 0)
	}
	appendItem(menuExitID, "Exit")

	trayNID = notifyIconData{hWnd: hwnd, uID: 1, uFlags: nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayCallback, hIcon: hIcon}
	trayNID.cbSize = uint32(unsafe.Sizeof(trayNID))
	for i, c := range syscall.StringToUTF16(tooltip) {
		if i >= len(trayNID.szTip) {
			break
		}
		trayNID.szTip[i] = c
	}
	// A few quick retries cover a taskbar that's still starting; after that the
	// TaskbarCreated broadcast adds the icon whenever the taskbar shows up.
	up := addTrayIcon()
	for i := 0; i < 6 && !up && !trayGone.Load(); i++ {
		time.Sleep(500 * time.Millisecond)
		up = addTrayIcon()
	}
	if ready != nil {
		ready <- up
	}

	var msg trayMsg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
	RemoveTray()
}

// RemoveTray deletes the tray icon. Safe to call more than once and from any
// thread (call right before the app actually exits so no ghost icon lingers).
func RemoveTray() {
	trayGone.Store(true)
	if !trayActive.Swap(false) {
		return
	}
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&trayNID)))
}
