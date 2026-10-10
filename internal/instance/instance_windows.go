//go:build windows

package instance

import (
	"errors"

	"golang.org/x/sys/windows"
)

// acquire uses a named mutex in this sign-in session's namespace. Only its
// existence matters: Windows deletes it when the last handle closes, so a
// crashed D BOX never leaves a stale lock behind.
func acquire(n string) (func(), bool) {
	p, err := windows.UTF16PtrFromString(`Local\` + n)
	if err != nil {
		return func() {}, true
	}
	h, err := windows.CreateMutex(nil, false, p)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(h)
		return func() {}, false
	}
	if err != nil { // can't tell — don't block the app from starting
		return func() {}, true
	}
	return func() { windows.CloseHandle(h) }, true
}
