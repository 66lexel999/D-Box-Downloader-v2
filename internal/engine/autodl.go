package engine

import (
	"fmt"
	"strings"
	"time"
)

// ---- auto download -----------------------------------------------------------
//
// Auto download skips the New Download window: while it is on, every download
// handed to D BOX (browser extension, main window) starts at once — or waits in
// the queue when "max at once" downloads already run. It is turned on from the
// New Download window for good, for some minutes, or for a number of files, and
// persists in settings.json so "forever" survives a restart.

// Auto download modes.
const (
	AutoForever = "forever"
	AutoMinutes = "minutes"
	AutoFiles   = "files"
)

// Limits for the minutes / files counts (longer than a day is what "forever" is for).
const (
	autoMaxMinutes = 24 * 60
	autoMaxFiles   = 999
)

// autoNow is the clock auto download runs on (tests move it).
var autoNow = time.Now

// autoDownload is the persisted auto download state. Mode "" is off; a
// minutes run is over once Until passes, a files run once Left reaches 0.
type autoDownload struct {
	Mode  string    `json:"mode"`
	Until time.Time `json:"until,omitempty"` // AutoMinutes: when it ends
	Left  int       `json:"left,omitempty"`  // AutoFiles: downloads still to auto-start
}

func (a autoDownload) active(now time.Time) bool {
	switch a.Mode {
	case AutoForever:
		return true
	case AutoMinutes:
		return now.Before(a.Until)
	case AutoFiles:
		return a.Left > 0
	}
	return false
}

// AutoDownloadView is the auto download state as the UI shows it.
type AutoDownloadView struct {
	Active bool   `json:"active"`
	Mode   string `json:"mode,omitempty"`  // "" when off
	Until  int64  `json:"until,omitempty"` // minutes: end time, epoch ms
	Left   int    `json:"left,omitempty"`  // files: downloads left
}

func (a autoDownload) view(now time.Time) AutoDownloadView {
	if !a.active(now) {
		return AutoDownloadView{}
	}
	v := AutoDownloadView{Active: true, Mode: a.Mode}
	switch a.Mode {
	case AutoMinutes:
		v.Until = a.Until.UnixMilli()
	case AutoFiles:
		v.Left = a.Left
	}
	return v
}

// AutoDownloadState reports whether auto download is on, and for how long.
func (e *Engine) AutoDownloadState() AutoDownloadView {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	return e.auto.view(autoNow())
}

// SetAutoDownload turns auto download on — mode AutoForever, AutoMinutes (n
// minutes from now) or AutoFiles (the next n downloads) — or off ("off" / "").
func (e *Engine) SetAutoDownload(mode string, n int) (AutoDownloadView, error) {
	now := autoNow()
	var a autoDownload
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "off":
	case AutoForever:
		a.Mode = AutoForever
	case AutoMinutes:
		if n < 1 || n > autoMaxMinutes {
			return AutoDownloadView{}, fmt.Errorf("%w: minutes must be 1 to %d", ErrConflict, autoMaxMinutes)
		}
		a.Mode, a.Until = AutoMinutes, now.Add(time.Duration(n)*time.Minute)
	case AutoFiles:
		if n < 1 || n > autoMaxFiles {
			return AutoDownloadView{}, fmt.Errorf("%w: files must be 1 to %d", ErrConflict, autoMaxFiles)
		}
		a.Mode, a.Left = AutoFiles, n
	default:
		return AutoDownloadView{}, fmt.Errorf("%w: unknown auto download mode %q", ErrConflict, mode)
	}
	e.settingsMu.Lock()
	e.auto = a
	e.saveSettingsLocked()
	e.settingsMu.Unlock()
	e.log.Info("auto download", "mode", a.Mode, "n", n)
	return a.view(now), nil
}

// TakeAutoDownload reports whether a download handed to D BOX now should skip
// the New Download window, and counts it against a files run. A caller that
// then fails to add the download gives it back with ReturnAutoDownload.
func (e *Engine) TakeAutoDownload() bool {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	if !e.auto.active(autoNow()) {
		return false
	}
	if e.auto.Mode == AutoFiles {
		e.auto.Left--
		e.saveSettingsLocked()
	}
	return true
}

// ReturnAutoDownload gives back a download TakeAutoDownload counted that was
// never added, so a files run still auto-starts as many as asked.
func (e *Engine) ReturnAutoDownload() {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	if e.auto.Mode == AutoFiles {
		e.auto.Left++
		e.saveSettingsLocked()
	}
}
