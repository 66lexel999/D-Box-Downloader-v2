package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"myidm/internal/config"
	"myidm/internal/engine"
	"myidm/internal/store"
)

func newTestServer(t *testing.T) (*Server, *engine.Engine, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.DownloadDir = filepath.Join(dir, "dl")
	cfg.Categories = config.DefaultCategories(cfg.DownloadDir)
	cfg.GUI = false
	st, err := store.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(cfg, st, log)
	ctx, cancel := context.WithCancel(context.Background())
	if err := eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Shutdown(3 * time.Second); cancel() })
	s := New(eng, log)
	return s, eng, s.Handler()
}

func call(t *testing.T, h http.Handler, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: bad JSON %q", method, path, rec.Body.String())
		}
	}
	return rec.Code
}

// TestExtensionFlowWithPageContext: the extension posts a captured link with
// its page and cookies; the dialog inspects it and starts it through a context
// token; the origin, which insists on that page's Referer and cookie, serves
// the file. Cookies never appear in the dialog URL.
func TestExtensionFlowWithPageContext(t *testing.T) {
	data := bytes.Repeat([]byte("D-BOX!"), 300_000)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://videos.test/watch/42" || !strings.Contains(r.Header.Get("Cookie"), "sid=s3cr3t") {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer origin.Close()

	s, eng, h := newTestServer(t)
	s.SetWebMode(true) // single-window UI: prompts are relayed to the page
	fileURL := origin.URL + "/v/stream"

	var pr map[string]any
	call(t, h, "POST", "/api/prompt", map[string]any{
		"url": fileURL, "referer": "https://videos.test/watch/42", "cookies": "sid=s3cr3t", "title": "Holiday Clip",
	}, &pr)
	var prompts struct{ Prompts []string }
	call(t, h, "GET", "/api/prompts", nil, &prompts)
	if len(prompts.Prompts) != 1 {
		t.Fatalf("prompts = %v", prompts.Prompts)
	}
	q, _ := url.ParseQuery(prompts.Prompts[0])
	if strings.Contains(prompts.Prompts[0], "s3cr3t") {
		t.Fatal("cookie leaked into the dialog URL")
	}
	tok := q.Get("ctx")
	if tok == "" || q.Get("title") != "Holiday Clip" {
		t.Fatalf("dialog query = %v", q)
	}

	var ins inspectResponse
	call(t, h, "GET", "/api/inspect?"+url.Values{"url": {fileURL}, "ctx": {tok}, "title": {"Holiday Clip"}}.Encode(), nil, &ins)
	if ins.Error != "" || ins.Kind != "file" || ins.Size != int64(len(data)) || ins.FileName != "Holiday Clip.mp4" || !ins.Resumable {
		t.Fatalf("inspect = %+v", ins)
	}

	var v engine.TaskView
	if code := call(t, h, "POST", "/api/tasks", map[string]any{"url": fileURL, "fileName": ins.FileName, "ctx": tok}, &v); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		cur, _ := eng.Get(v.ID)
		if cur.Status == engine.StatusCompleted {
			got, _ := os.ReadFile(cur.FilePath)
			if !bytes.Equal(got, data) {
				t.Fatal("content mismatch")
			}
			if filepath.Base(cur.FilePath) != "Holiday Clip.mp4" {
				t.Errorf("name = %s", filepath.Base(cur.FilePath))
			}
			return
		}
		if cur.Status == engine.StatusFailed || time.Now().After(deadline) {
			t.Fatalf("status %s: %s", cur.Status, cur.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestIconCacheFollowsFile: a different file at the same path (a new
// "setup.exe" after the old one was deleted) must get its own icon, not the
// previous file's cached one.
func TestIconCacheFollowsFile(t *testing.T) {
	s, _, h := newTestServer(t)
	calls := 0
	s.SetIconResolver(func(p string) ([]byte, bool, error) {
		calls++
		b, _ := os.ReadFile(p)
		return []byte(fmt.Sprintf("icon-of-%s", b)), false, nil
	}, func(string) bool { return false })

	p := filepath.Join(t.TempDir(), "setup.exe")
	os.WriteFile(p, []byte("vscode"), 0o644)
	get := func() string {
		req := httptest.NewRequest("GET", "/api/icon?path="+url.QueryEscape(p)+"&v=1", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if got := get(); got != "icon-of-vscode" {
		t.Fatalf("first icon = %q", got)
	}
	get()
	if calls != 1 {
		t.Fatalf("unchanged file re-extracted (%d calls)", calls)
	}
	os.Remove(p)
	os.WriteFile(p, []byte("discord!"), 0o644) // same path, different program
	os.Chtimes(p, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	if got := get(); got != "icon-of-discord!" {
		t.Fatalf("stale icon after the file changed: %q", got)
	}
}

// TestContextAliases: "referrer" (the DOM spelling) and "userAgent" are
// accepted from the extension, alongside "referer"/"cookies".
func TestContextAliases(t *testing.T) {
	s, _, _ := newTestServer(t)
	ref, h := s.resolveContext(browserContext{Referrer: "https://page.test/v/1", UserAgent: "Brave/1", Cookies: "cf_clearance=x"})
	if ref != "https://page.test/v/1" || h["User-Agent"] != "Brave/1" || h["Cookie"] != "cf_clearance=x" {
		t.Fatalf("referer=%q headers=%v", ref, h)
	}
}

// TestStartupSetting: the Settings switch for "Start D BOX when Windows
// starts" is hidden when the platform can't do it, reads the live state, and
// turning it off/on goes through the wired setter.
func TestStartupSetting(t *testing.T) {
	s, _, h := newTestServer(t)

	var v map[string]any
	call(t, h, "GET", "/api/settings", nil, &v)
	if v["startWithWindowsSupported"] != false || v["startWithWindows"] != nil {
		t.Fatalf("not wired: %v", v)
	}
	if code := call(t, h, "POST", "/api/settings/startup", map[string]bool{"on": true}, nil); code != http.StatusNotImplemented {
		t.Fatalf("set without a backend: HTTP %d", code)
	}

	on := true
	var sets []bool
	s.SetStartup(func() (bool, error) { return on, nil }, func(b bool) error {
		sets = append(sets, b)
		on = b
		return nil
	})
	call(t, h, "GET", "/api/settings", nil, &v)
	if v["startWithWindowsSupported"] != true || v["startWithWindows"] != true {
		t.Fatalf("wired: %v", v)
	}
	if code := call(t, h, "POST", "/api/settings/startup", map[string]bool{"on": false}, &v); code != http.StatusOK || v["startWithWindows"] != false {
		t.Fatalf("turn off: HTTP %d %v", code, v)
	}
	if len(sets) != 1 || sets[0] {
		t.Fatalf("setter calls = %v", sets)
	}

	s.SetStartup(func() (bool, error) { return false, nil }, func(bool) error { return fmt.Errorf("access denied") })
	var e map[string]string
	if code := call(t, h, "POST", "/api/settings/startup", map[string]bool{"on": true}, &e); code != http.StatusInternalServerError || !strings.Contains(e["error"], "access denied") {
		t.Fatalf("failure: HTTP %d %v", code, e)
	}
}

// TestShowBringsUpTheWindow: a second launch asks the running copy to show its
// window through POST /api/show.
func TestShowBringsUpTheWindow(t *testing.T) {
	s, _, h := newTestServer(t)
	var r map[string]bool
	if code := call(t, h, "POST", "/api/show", nil, &r); code != http.StatusOK || r["shown"] {
		t.Fatalf("no window wired: HTTP %d %v", code, r)
	}
	shown, closing := 0, false
	s.SetShowFunc(func() bool {
		if closing {
			return false
		}
		shown++
		return true
	})
	if code := call(t, h, "POST", "/api/show", nil, &r); code != http.StatusOK || !r["shown"] || shown != 1 {
		t.Fatalf("HTTP %d %v, activations %d", code, r, shown)
	}
	closing = true // File → Exit in progress: the new launch must wait, not hand over
	if code := call(t, h, "POST", "/api/show", nil, nil); code != http.StatusServiceUnavailable || shown != 1 {
		t.Fatalf("while closing: HTTP %d, activations %d", code, shown)
	}
}
