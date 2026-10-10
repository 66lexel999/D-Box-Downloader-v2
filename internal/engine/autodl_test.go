package engine

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"myidm/internal/config"
)

func newSettingsEngine(dataDir string) *Engine {
	return &Engine{cfg: &config.Config{DataDir: dataDir}, overrides: map[string]string{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestAutoDownloadFiles: a files run auto-starts exactly that many downloads,
// a download given back is counted again, and the count survives a restart.
func TestAutoDownloadFiles(t *testing.T) {
	dataDir := t.TempDir()
	e := newSettingsEngine(dataDir)
	if e.TakeAutoDownload() {
		t.Fatal("auto download is on by default")
	}
	v, err := e.SetAutoDownload("files", 2)
	if err != nil || !v.Active || v.Mode != AutoFiles || v.Left != 2 {
		t.Fatalf("SetAutoDownload(files, 2) = %+v, %v", v, err)
	}
	if !e.TakeAutoDownload() {
		t.Fatal("1st download should auto-start")
	}
	e2 := newSettingsEngine(dataDir) // a restart keeps the count
	e2.loadSettings()
	if got := e2.AutoDownloadState(); !got.Active || got.Left != 1 {
		t.Fatalf("after restart = %+v, want 1 file left", got)
	}
	if !e.TakeAutoDownload() {
		t.Fatal("2nd download should auto-start")
	}
	if e.TakeAutoDownload() {
		t.Fatal("3rd download should show the window again")
	}
	if got := e.AutoDownloadState(); got.Active {
		t.Fatalf("state after the run = %+v, want off", got)
	}
	e.ReturnAutoDownload() // the 2nd couldn't be added after all
	if !e.TakeAutoDownload() {
		t.Fatal("a returned download should auto-start the next one")
	}
}

// TestAutoDownloadMinutes: a minutes run ends on time, also across a restart.
func TestAutoDownloadMinutes(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	autoNow = func() time.Time { return now }
	t.Cleanup(func() { autoNow = time.Now })

	dataDir := t.TempDir()
	e := newSettingsEngine(dataDir)
	v, err := e.SetAutoDownload("minutes", 10)
	if err != nil || !v.Active || v.Until != now.Add(10*time.Minute).UnixMilli() {
		t.Fatalf("SetAutoDownload(minutes, 10) = %+v, %v", v, err)
	}
	now = now.Add(9 * time.Minute)
	if !e.TakeAutoDownload() || !e.TakeAutoDownload() {
		t.Fatal("downloads within the 10 minutes should auto-start, however many")
	}
	e2 := newSettingsEngine(dataDir)
	e2.loadSettings()
	if !e2.AutoDownloadState().Active {
		t.Fatal("a restart within the 10 minutes should keep it on")
	}
	now = now.Add(time.Minute)
	if e.TakeAutoDownload() || e.AutoDownloadState().Active {
		t.Fatal("auto download should end after 10 minutes")
	}
	e3 := newSettingsEngine(dataDir)
	e3.loadSettings()
	if e3.AutoDownloadState().Active {
		t.Fatal("an expired run should stay off after a restart")
	}
}

func TestAutoDownloadForeverAndOff(t *testing.T) {
	dataDir := t.TempDir()
	e := newSettingsEngine(dataDir)
	if _, err := e.SetAutoDownload("forever", 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if !e.TakeAutoDownload() {
			t.Fatalf("download %d should auto-start", i+1)
		}
	}
	e2 := newSettingsEngine(dataDir)
	e2.loadSettings()
	if got := e2.AutoDownloadState(); !got.Active || got.Mode != AutoForever {
		t.Fatalf("forever after restart = %+v", got)
	}
	if _, err := e.SetAutoDownload("off", 0); err != nil {
		t.Fatal(err)
	}
	if e.TakeAutoDownload() {
		t.Fatal("off should show the window again")
	}
	e.ReturnAutoDownload() // nothing to give back to once it's off
	if e.TakeAutoDownload() {
		t.Fatal("ReturnAutoDownload must not turn a stopped run back on")
	}
}

func TestAutoDownloadRejectsBadInput(t *testing.T) {
	e := newSettingsEngine(t.TempDir())
	for _, c := range []struct {
		mode string
		n    int
	}{{"minutes", 0}, {"minutes", autoMaxMinutes + 1}, {"files", 0}, {"files", -3}, {"files", autoMaxFiles + 1}, {"hours", 2}} {
		if _, err := e.SetAutoDownload(c.mode, c.n); err == nil {
			t.Errorf("SetAutoDownload(%q, %d) accepted", c.mode, c.n)
		}
	}
	if e.AutoDownloadState().Active {
		t.Fatal("rejected input turned auto download on")
	}
}

// TestDonePopupSetting: "Don't show again" stops the completion popup and
// persists; turning it back on also ends a 2-hour pause.
func TestDonePopupSetting(t *testing.T) {
	dataDir := t.TempDir()
	e := newSettingsEngine(dataDir)
	var shown []string
	e.SetCompletionNotifier(func(id string) { shown = append(shown, id) })

	e.notifyCompleted("a")
	e.SetDonePopupEnabled(false)
	e.notifyCompleted("b")
	e2 := newSettingsEngine(dataDir)
	e2.loadSettings()
	if e2.DonePopupEnabled() {
		t.Fatal("hiding the popup should persist")
	}

	e.SuppressCompletionPopup(2 * time.Hour)
	e.SetDonePopupEnabled(true)
	e.notifyCompleted("c")
	if len(shown) != 2 || shown[0] != "a" || shown[1] != "c" {
		t.Fatalf("popups shown for %v, want [a c]", shown)
	}
}
