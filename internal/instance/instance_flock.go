//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package instance

import (
	"os"
	"path/filepath"
	"syscall"
)

// acquire takes an exclusive flock on a file in the temp folder; the kernel
// drops it when the process exits.
func acquire(n string) (func(), bool) {
	f, err := os.OpenFile(filepath.Join(os.TempDir(), n+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, true
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return func() {}, err != syscall.EWOULDBLOCK
	}
	return func() { f.Close() }, true
}
