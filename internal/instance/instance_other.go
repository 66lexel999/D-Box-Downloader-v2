//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package instance

// acquire is a no-op where no lock primitive is wired up: every launch runs.
func acquire(string) (func(), bool) { return func() {}, true }
