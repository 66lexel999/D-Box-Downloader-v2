//go:build windows

package gui

// D BOX's own icon on every window it opens. go-webview2 means to give its
// windows the exe's icon, but with WindowOptions.IconId == 0 it calls LoadImageW
// with its arguments shifted (the icon WIDTH lands in the image-type slot), so
// the call fails and every popup — New Download, Download status, Download
// complete — was created with no icon at all: a generic or wrong icon in the
// title bar, Alt-Tab and the taskbar. We set the icon explicitly instead, from
// the same source the tray uses (the first icon resource of DBox.exe).

import (
	"os"
	"sync"
	"syscall"
	"unsafe"
)

var (
	procExtractIconEx   = shell32.NewProc("ExtractIconExW")
	procSendMessage     = user32.NewProc("SendMessageW")
	procSetClassLongPtr = user32.NewProc("SetClassLongPtrW")

	appIconOnce              sync.Once
	appIconBig, appIconSmall uintptr // loaded once per process, never destroyed
)

const (
	wmSetIcon   = 0x0080
	iconSmall   = 0
	iconBig     = 1
	gclpHIcon   = ^uintptr(13) // GCLP_HICON   (-14)
	gclpHIconSm = ^uintptr(33) // GCLP_HICONSM (-34)
)

func appIcons() (big, small uintptr) {
	appIconOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		p, err := syscall.UTF16PtrFromString(exe)
		if err != nil {
			return
		}
		procExtractIconEx.Call(uintptr(unsafe.Pointer(p)), 0,
			uintptr(unsafe.Pointer(&appIconBig)), uintptr(unsafe.Pointer(&appIconSmall)), 1)
	})
	return appIconBig, appIconSmall
}

// applyAppIcon gives hwnd D BOX's icon for the title bar, Alt-Tab and the
// taskbar. The window-class icon is set as well, so the shell never falls back
// to a class with no icon.
func applyAppIcon(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	big, small := appIcons()
	if small == 0 {
		small = big
	}
	if big == 0 {
		big = small
	}
	if big == 0 {
		return
	}
	procSendMessage.Call(hwnd, wmSetIcon, iconSmall, small)
	procSendMessage.Call(hwnd, wmSetIcon, iconBig, big)
	procSetClassLongPtr.Call(hwnd, gclpHIcon, big)
	procSetClassLongPtr.Call(hwnd, gclpHIconSm, small)
}
