//go:build !windows

package wailsui

import (
	"errors"
	"log/slog"
	"net/http"
)

// Run is unavailable off Windows (Wails/WebView2 is Windows-only here).
func Run(_ http.Handler, _ *slog.Logger, _ func(), _ bool) error {
	return errors.New("wails UI is only supported on windows")
}

// Activate is a no-op off Windows.
func Activate() {}

// Quit is a no-op off Windows.
func Quit() {}

// Quitting is always false off Windows.
func Quitting() bool { return false }
