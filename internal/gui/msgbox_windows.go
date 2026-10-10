//go:build windows

package gui

import (
	"syscall"
	"unsafe"
)

var procMessageBox = user32.NewProc("MessageBoxW")

const (
	mbIconInformation = 0x00000040
	mbSetForeground   = 0x00010000
	mbTopmost         = 0x00040000
)

// MessageBox shows a small native OK box and waits for it to be dismissed.
func MessageBox(title, text string) {
	t, err1 := syscall.UTF16PtrFromString(title)
	m, err2 := syscall.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	procMessageBox.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)),
		mbIconInformation|mbSetForeground|mbTopmost)
}
