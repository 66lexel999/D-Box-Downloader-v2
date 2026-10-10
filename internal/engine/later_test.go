package engine

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestAddLaterHoldsTask verifies a Download Later add is held (paused, flagged
// later) and never enters the run queue until started.
func TestAddLaterHoldsTask(t *testing.T) {
	e := newSchedTestEngine(t)

	v, err := e.Add("https://example.com/a.bin", "a.bin", 4, "", time.Time{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusPaused || !v.Later {
		t.Fatalf("status=%q later=%v, want paused+later", v.Status, v.Later)
	}
	if len(e.queue) != 0 {
		t.Fatalf("a Download Later task must not be queued; queue=%v", e.queue)
	}

	// Skip toggling is reflected in the view.
	if err := e.SetSkip(v.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(v.ID); !got.Skip {
		t.Fatal("SetSkip(true) did not set the skip flag")
	}
	if err := e.SetSkip(v.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(v.ID); got.Skip {
		t.Fatal("SetSkip(false) did not clear the skip flag")
	}

	// "Start" from the queue = Resume → enters the run queue, still flagged later.
	if err := e.Resume(v.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(v.ID)
	if got.Status != StatusQueued {
		t.Fatalf("after start status=%q, want queued", got.Status)
	}
	if !got.Later {
		t.Fatal("the later flag should persist while the queued item downloads")
	}
	if !contains(e.queue, v.ID) {
		t.Fatal("started later-item should be in the run queue")
	}
}

// TestSetMaxConcurrent verifies the simultaneous-download limit is clamped,
// persisted, and reloaded.
func TestSetMaxConcurrent(t *testing.T) {
	e := newSchedTestEngine(t)

	if err := e.SetMaxConcurrent(5); err != nil {
		t.Fatal(err)
	}
	if got := e.maxConcurrent(); got != 5 {
		t.Fatalf("after set, maxConcurrent = %d, want 5", got)
	}
	// Clamp out-of-range.
	e.SetMaxConcurrent(0)
	if got := e.maxConcurrent(); got != 1 {
		t.Fatalf("clamp low: got %d, want 1", got)
	}
	e.SetMaxConcurrent(999)
	if got := e.maxConcurrent(); got != 16 {
		t.Fatalf("clamp high: got %d, want 16", got)
	}

	// Persists across a reload over the same data dir.
	e.SetMaxConcurrent(4)
	e2 := newSchedTestEngine(t)
	e2.cfg.DataDir = e.cfg.DataDir
	e2.loadSettings()
	if got := e2.maxConcurrent(); got != 4 {
		t.Fatalf("reloaded maxConcurrent = %d, want 4", got)
	}
}

// TestStartWithWindowsDoesNotPinFlags: recording the startup choice on a fresh
// install must not freeze this launch's -dir / -concurrent into settings.json.
func TestStartWithWindowsDoesNotPinFlags(t *testing.T) {
	e := newSchedTestEngine(t)
	e.SetStartWithWindows(true)
	b, err := os.ReadFile(e.settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved["downloadDir"]; ok {
		t.Errorf("downloadDir pinned by the startup choice: %s", b)
	}
	if _, ok := saved["maxConcurrent"]; ok {
		t.Errorf("maxConcurrent pinned by the startup choice: %s", b)
	}
	if saved["startWithWindows"] != true {
		t.Errorf("startup choice not saved: %s", b)
	}

	// A later launch with other flags uses them.
	e2 := newSchedTestEngine(t)
	e2.cfg.DataDir = e.cfg.DataDir
	flagDir := t.TempDir()
	e2.cfg.DownloadDir, e2.cfg.MaxConcurrent = flagDir, 7
	e2.loadSettings()
	if e2.cfg.DownloadDir != flagDir || e2.cfg.MaxConcurrent != 7 {
		t.Fatalf("flags overridden: dir=%s concurrent=%d", e2.cfg.DownloadDir, e2.cfg.MaxConcurrent)
	}

	// Choosing them in Options still persists them.
	if err := e2.SetMaxConcurrent(4); err != nil {
		t.Fatal(err)
	}
	e3 := newSchedTestEngine(t)
	e3.cfg.DataDir = e.cfg.DataDir
	e3.loadSettings()
	if e3.cfg.MaxConcurrent != 4 {
		t.Fatalf("Options choice lost: concurrent=%d", e3.cfg.MaxConcurrent)
	}
	if got := e3.StartWithWindows(); got == nil || !*got {
		t.Fatalf("startup choice lost: %v", got)
	}
}

// TestStartWithWindowsSetting verifies the "start with Windows" choice is
// unset by default, persisted, reloaded, and that re-saving the same choice
// doesn't rewrite settings.json.
func TestStartWithWindowsSetting(t *testing.T) {
	e := newSchedTestEngine(t)
	if got := e.StartWithWindows(); got != nil {
		t.Fatalf("fresh install: got %v, want nil (never chosen)", *got)
	}
	e.SetStartWithWindows(false)
	if got := e.StartWithWindows(); got == nil || *got {
		t.Fatalf("after SetStartWithWindows(false): %v", got)
	}

	reload := func() *Engine {
		e2 := newSchedTestEngine(t)
		e2.cfg.DataDir = e.cfg.DataDir
		e2.loadSettings()
		return e2
	}
	if got := reload().StartWithWindows(); got == nil || *got {
		t.Fatalf("reloaded choice = %v, want off", got)
	}

	// Same choice again: no write (main calls this at every launch).
	path := e.settingsPath()
	os.Remove(path)
	e.SetStartWithWindows(false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an unchanged choice rewrote settings.json")
	}
	e.SetStartWithWindows(true)
	if got := reload().StartWithWindows(); got == nil || !*got {
		t.Fatalf("reloaded choice = %v, want on", got)
	}
}
