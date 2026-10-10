package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"myidm/internal/engine"
)

// TestPromptAutoDownload: while auto download runs, a prompted download starts
// without the New Download window — in the folder of its category, with the
// page context — and the window comes back once the run is used up. "Save As"
// (ask) always shows the window and doesn't use up the run.
func TestPromptAutoDownload(t *testing.T) {
	s, eng, h := newTestServer(t)
	data := make([]byte, 200_000) // binary, so it isn't taken for a web page
	for i := range data {
		data[i] = byte(i * 7)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer origin.Close()
	var dialogs []url.Values
	s.SetDialogOpener(func(q url.Values) error { dialogs = append(dialogs, q); return nil })

	prompt := func(body map[string]any) map[string]any {
		var r map[string]any // fresh: decoding into a reused map keeps old keys
		call(t, h, "POST", "/api/prompt", body, &r)
		return r
	}

	r := prompt(map[string]any{"url": origin.URL + "/a.zip"})
	if r["auto"] != nil || len(dialogs) != 1 {
		t.Fatalf("auto download is off: want the window, got %v (%d windows)", r, len(dialogs))
	}

	var v engine.AutoDownloadView
	if code := call(t, h, "POST", "/api/autodownload", map[string]any{"mode": "files", "n": 2}, &v); code != http.StatusOK || !v.Active || v.Left != 2 {
		t.Fatalf("turn on: HTTP %d %+v", code, v)
	}
	r = prompt(map[string]any{"url": origin.URL + "/b.zip", "ask": true})
	if r["auto"] != nil || len(dialogs) != 2 {
		t.Fatalf("Save As (ask) should show the window: %v", r)
	}

	r = prompt(map[string]any{"url": origin.URL + "/dl?id=7", "name": "report.pdf", "referer": "https://site.test/page"})
	if r["native"] != true || r["auto"] != true || r["id"] == nil || len(dialogs) != 2 {
		t.Fatalf("1st auto download: %v (%d windows)", r, len(dialogs))
	}
	got, err := eng.Get(r["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != eng.CategoryDir("Documents") || got.Referer != "https://site.test/page" {
		t.Errorf("task dir %q referer %q, want the Documents folder and the page", got.Dir, got.Referer)
	}

	if r = prompt(map[string]any{"url": origin.URL + "/c.zip"}); r["auto"] != true {
		t.Fatalf("2nd auto download: %v", r)
	}
	var list struct {
		Auto engine.AutoDownloadView `json:"auto"`
	}
	call(t, h, "GET", "/api/tasks", nil, &list)
	if list.Auto.Active {
		t.Fatalf("the run is used up, the list still reports %+v", list.Auto)
	}
	if r = prompt(map[string]any{"url": origin.URL + "/d.zip"}); r["auto"] != nil || len(dialogs) != 3 {
		t.Fatalf("after 2 files the window should be back: %v (%d windows)", r, len(dialogs))
	}

	// Both auto downloads finish, each in its category's folder.
	want := map[string]string{"report.pdf": eng.CategoryDir("Documents"), "c.zip": eng.CategoryDir("Compressed")}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		tasks, done := eng.List(), 0
		for _, task := range tasks {
			if task.Status == engine.StatusFailed {
				t.Fatalf("%s failed: %s", task.FileName, task.Error)
			}
			if task.Status == engine.StatusCompleted {
				done++
			}
		}
		if len(tasks) != 2 {
			t.Fatalf("%d tasks, want the 2 auto downloads", len(tasks))
		}
		if done == 2 {
			for _, task := range tasks {
				if filepath.Dir(task.FilePath) != want[task.FileName] {
					t.Errorf("%s saved to %s, want %s", task.FileName, task.FilePath, want[task.FileName])
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("auto downloads didn't finish: %+v", tasks)
		}
	}
}

// TestPromptAutoDownloadFallsBackToWindow: a download auto download can't add
// shows the New Download window (which explains why) and isn't counted.
func TestPromptAutoDownloadFallsBackToWindow(t *testing.T) {
	s, eng, h := newTestServer(t)
	var dialogs int
	s.SetDialogOpener(func(url.Values) error { dialogs++; return nil })
	if _, err := eng.SetAutoDownload("files", 1); err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	call(t, h, "POST", "/api/prompt", map[string]any{"url": "ftp://files.test/a.zip"}, &r)
	if r["auto"] != nil || r["native"] != true || dialogs != 1 {
		t.Fatalf("unaddable link: %v (%d windows)", r, dialogs)
	}
	if v := eng.AutoDownloadState(); !v.Active || v.Left != 1 {
		t.Fatalf("the failed one was counted: %+v", v)
	}
}

func TestAutoDownloadEndpoints(t *testing.T) {
	_, _, h := newTestServer(t)
	var v engine.AutoDownloadView
	if code := call(t, h, "POST", "/api/autodownload", map[string]any{"mode": "minutes", "n": 30}, &v); code != http.StatusOK ||
		!v.Active || v.Mode != "minutes" || v.Until < time.Now().Add(29*time.Minute).UnixMilli() {
		t.Fatalf("minutes: HTTP %d %+v", code, v)
	}
	var settings map[string]any
	call(t, h, "GET", "/api/settings", nil, &settings)
	if a, _ := settings["autoDownload"].(map[string]any); a["active"] != true {
		t.Fatalf("settings autoDownload = %v", settings["autoDownload"])
	}
	if code := call(t, h, "POST", "/api/autodownload", map[string]any{"mode": "files", "n": 0}, nil); code != http.StatusBadRequest {
		t.Fatalf("0 files: HTTP %d", code)
	}
	call(t, h, "POST", "/api/autodownload", map[string]any{"mode": "off"}, &v)
	if v.Active {
		t.Fatalf("off: %+v", v)
	}
	call(t, h, "GET", "/api/autodownload", nil, &v)
	if v.Active {
		t.Fatalf("GET after off: %+v", v)
	}
}

func TestAutoCategory(t *testing.T) {
	for _, c := range []struct{ name, url, mime, want string }{
		{"setup.EXE", "https://x.test/get?id=1", "", "Programs"},
		{"", "https://x.test/files/archive.tar.gz?sig=abc", "", "Compressed"},
		{"download", "https://x.test/dl.php?f=2", "audio/mpeg", "Music"},
		{"", "https://cdn.test/master.m3u8", "", "Video"},
		{"", "https://x.test/v", "video/mp4; codecs=avc1", "Video"},
		{"photo.jpg", "https://x.test/a.zip", "", "Images"}, // the browser's name beats the link
		{"", "https://x.test/page.html", "text/html", "General"},
		{"", "not a url", "", "General"},
	} {
		if got := autoCategory(c.name, c.url, c.mime); got != c.want {
			t.Errorf("autoCategory(%q, %q, %q) = %q, want %q", c.name, c.url, c.mime, got, c.want)
		}
	}
}

// TestDoneWindows: the completion windows' registry — count, the active one
// (focused last, else the newest), forgetting dead windows, and Close all.
func TestDoneWindows(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	d := &doneWindows{now: func() time.Time { return now }}
	step := func() { now = now.Add(100 * time.Millisecond) }

	if n, active, _ := d.beat("a", true); n != 1 || !active {
		t.Fatalf("one window: count %d active %v", n, active)
	}
	step()
	if n, active, _ := d.beat("b", false); n != 2 || !active {
		t.Fatalf("a new window is the active one: count %d active %v", n, active)
	}
	if _, active, _ := d.beat("a", false); active {
		t.Fatal("the older window should step back")
	}
	step()
	if _, active, _ := d.beat("a", true); !active { // the user clicked the older one
		t.Fatal("the focused window should be active")
	}
	if _, active, _ := d.beat("b", false); active {
		t.Fatal("only one window is active")
	}
	step()
	d.beat("b", true) // b gains focus …
	step()
	if _, active, _ := d.beat("a", true); active { // … while a still claims it: no flip-flop
		t.Fatal("a window that kept focus took it back from the one that gained it")
	}
	if _, active, _ := d.beat("b", true); !active {
		t.Fatal("the window that gained focus last should stay active")
	}

	d.gone("b")
	if n, active, _ := d.beat("a", false); n != 1 || !active {
		t.Fatalf("after b closed: count %d active %v", n, active)
	}
	d.beat("c", false)
	now = now.Add(doneWinTTL + time.Second) // c's process died; a keeps beating
	if n, _, _ := d.beat("a", false); n != 1 {
		t.Fatalf("a dead window still counted: %d", n)
	}

	d.beat("d", false)
	if got := d.closeAll(); got != 2 {
		t.Fatalf("closeAll closed %d, want 2", got)
	}
	if _, _, closing := d.beat("a", false); !closing {
		t.Fatal("Close all should close a")
	}
	if n, active, closing := d.beat("e", false); n != 1 || !active || closing {
		t.Fatalf("a window opened after Close all stays: count %d active %v closing %v", n, active, closing)
	}
}

func TestDoneWindowEndpoints(t *testing.T) {
	_, eng, h := newTestServer(t)
	var r map[string]any
	if code := call(t, h, "POST", "/api/done-windows/bad%20key!", map[string]bool{"focused": true}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad key: HTTP %d", code)
	}
	call(t, h, "POST", "/api/done-windows/w1", map[string]bool{"focused": true}, &r)
	call(t, h, "POST", "/api/done-windows/w2", map[string]bool{"focused": true}, &r)
	if r["count"] != float64(2) || r["active"] != true || r["close"] != false {
		t.Fatalf("two windows: %v", r)
	}
	call(t, h, "DELETE", "/api/done-windows/w2", nil, nil)
	call(t, h, "POST", "/api/done-windows/w1", nil, &r)
	if r["count"] != float64(1) {
		t.Fatalf("after w2 closed: %v", r)
	}
	call(t, h, "POST", "/api/done-windows/w3", nil, &r)
	call(t, h, "POST", "/api/done-windows/close-all", nil, &r)
	if r["closed"] != float64(2) {
		t.Fatalf("close-all: %v", r)
	}
	call(t, h, "POST", "/api/done-windows/w3", nil, &r)
	if r["close"] != true {
		t.Fatalf("w3 should close: %v", r)
	}

	// "Don't show again": turns the popup off (persisted) and closes the open ones.
	call(t, h, "POST", "/api/done-windows/w4", nil, nil)
	var v map[string]any
	if code := call(t, h, "POST", "/api/settings/done-popup", map[string]bool{"on": false}, &v); code != http.StatusOK || v["showDonePopup"] != false {
		t.Fatalf("done-popup off: HTTP %d %v", code, v)
	}
	if eng.DonePopupEnabled() {
		t.Fatal("engine still shows the popup")
	}
	call(t, h, "POST", "/api/done-windows/w4", nil, &r)
	if r["close"] != true {
		t.Fatalf("Don't show again should close the open windows: %v", r)
	}
	call(t, h, "POST", "/api/settings/done-popup", map[string]bool{"on": true}, &v)
	if v["showDonePopup"] != true {
		t.Fatalf("back on in Options: %v", v)
	}
}
