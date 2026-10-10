package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInjectTheme(t *testing.T) {
	cases := []struct{ in, id, want string }{
		{`<!doctype html><html lang="en"><head>`, "midnight", `<!doctype html><html lang="en" data-theme="midnight"><head>`},
		{`<html><body>`, "slate", `<html data-theme="slate"><body>`},
		{`<html lang="en" data-theme="light"><head>`, "ocean", `<html lang="en" data-theme="light"><head>`}, // already set: untouched
		{`no tag here`, "ocean", `no tag here`},
	}
	for _, c := range cases {
		if got := string(injectTheme([]byte(c.in), c.id)); got != c.want {
			t.Errorf("injectTheme(%q,%q) = %q, want %q", c.in, c.id, got, c.want)
		}
	}
}

func TestNormalizeUITheme(t *testing.T) {
	for in, want := range map[string]string{"": "slate", "graphite": "slate", "slate": "slate", "classic": "classic", "light": "light"} {
		if got, ok := normalizeUITheme(in); !ok || got != want {
			t.Errorf("normalizeUITheme(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := normalizeUITheme("neon"); ok {
		t.Error("unknown theme accepted")
	}
}

func getBody(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func postJSON(t *testing.T, h http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func TestUISettingsPersistAndInject(t *testing.T) {
	s, _, h := newTestServer(t)
	dir := t.TempDir()
	s.SetDataDir(dir)
	hooked := make(chan string, 4)
	s.SetUIThemeHook(func(id string) { hooked <- id })

	// Defaults: slate, not saved; every page carries data-theme="slate".
	code, body := getBody(t, h, "/api/settings/ui")
	if code != 200 || !strings.Contains(body, `"theme":"slate"`) || !strings.Contains(body, `"saved":false`) {
		t.Fatalf("GET defaults = %d %s", code, body)
	}
	for _, p := range []string{"/", "/add", "/detail", "/done"} {
		if _, b := getBody(t, h, p); !strings.Contains(b, `data-theme="slate"`) {
			t.Errorf("%s: missing default data-theme", p)
		}
	}

	if code, _ := postJSON(t, h, "/api/settings/ui", `{"theme":"neon"}`); code != http.StatusBadRequest {
		t.Errorf("unknown theme: code %d, want 400", code)
	}
	code, m := postJSON(t, h, "/api/settings/ui", `{"theme":"midnight","zoom":110,"text":1000}`)
	if code != 200 || m["theme"] != "midnight" || m["zoom"] != float64(110) || m["text"] != float64(250) || m["saved"] != true {
		t.Fatalf("POST = %d %v", code, m)
	}
	select {
	case id := <-hooked:
		if id != "midnight" {
			t.Errorf("hook got %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Error("theme hook not called")
	}
	for _, p := range []string{"/", "/add", "/detail", "/done"} {
		_, b := getBody(t, h, p)
		if !strings.Contains(b, `data-theme="midnight"`) {
			t.Errorf("%s: saved theme not injected", p)
		}
		if strings.Count(b, "data-theme=\"midnight\"") != 1 && p != "/" {
			t.Errorf("%s: theme injected more than once", p)
		}
	}
	// Partial update keeps the theme; legacy "graphite" maps to the default.
	if _, m := postJSON(t, h, "/api/settings/ui", `{"zoom":90}`); m["theme"] != "midnight" || m["zoom"] != float64(90) {
		t.Errorf("partial POST = %v", m)
	}

	// Persisted to ui.json and reloaded by a fresh server.
	b, err := os.ReadFile(filepath.Join(dir, "ui.json"))
	if err != nil || !strings.Contains(string(b), `"midnight"`) {
		t.Fatalf("ui.json = %q, %v", b, err)
	}
	s2, _, _ := newTestServer(t)
	s2.SetDataDir(dir)
	if s2.UITheme() != "midnight" {
		t.Errorf("reloaded theme = %q", s2.UITheme())
	}
	if _, m := postJSON(t, h, "/api/settings/ui", `{"theme":"graphite"}`); m["theme"] != "slate" {
		t.Errorf("graphite → %v", m["theme"])
	}
}
