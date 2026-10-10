package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// UI appearance (theme, zoom, text size), persisted server-side in <data>/ui.json.
//
// Why on the server: in the default Wails mode the New Download / status /
// completion windows are separate processes with their own WebView2 profile and
// origin, so a theme kept only in the main window's localStorage never reaches
// them. Instead every page handler (/, /add, /detail, /done) stamps the saved
// theme into the <html> tag before sending (data-theme="<id>", "slate" for the
// default), so each window paints the right colours on its very first frame. A
// page's pre-paint script falls back to localStorage only when the attribute is
// missing (e.g. an old cached page).

// uiThemes are the theme ids /ui/theme.css defines. "slate" is the default (no
// dedicated CSS block: the :root values).
var uiThemes = map[string]bool{"slate": true, "midnight": true, "ocean": true, "forest": true, "classic": true, "light": true}

const defaultUITheme = "slate"

// normalizeUITheme maps legacy/empty ids to the default; ok=false for unknown ids.
func normalizeUITheme(id string) (string, bool) {
	switch id {
	case "", "graphite", "default": // "graphite" = the old default's id (now "classic" is that gray)
		return defaultUITheme, true
	}
	return id, uiThemes[id]
}

// uiPrefs is ui.json. Zoom/Text are percentages (100 = normal); 0 = never set.
type uiPrefs struct {
	Theme string `json:"theme"`
	Zoom  int    `json:"zoom,omitempty"`
	Text  int    `json:"text,omitempty"`
}

type uiState struct {
	mu      sync.Mutex
	path    string // <data>/ui.json; "" = keep in memory only (tests, headless without a data dir)
	prefs   uiPrefs
	saved   bool            // prefs came from disk or a POST (vs. built-in defaults)
	onTheme func(id string) // e.g. the Wails window's title bar/background; nil = none
}

// SetDataDir enables persistence of the UI appearance in dir/ui.json and loads
// any saved choice. Call once at startup, before serving.
func (s *Server) SetDataDir(dir string) {
	s.ui.mu.Lock()
	defer s.ui.mu.Unlock()
	s.ui.path = ""
	if dir == "" {
		return
	}
	s.ui.path = filepath.Join(dir, "ui.json")
	b, err := os.ReadFile(s.ui.path)
	if err != nil {
		return
	}
	var p uiPrefs
	if json.Unmarshal(b, &p) != nil {
		return
	}
	if id, ok := normalizeUITheme(p.Theme); ok {
		p.Theme = id
	} else {
		p.Theme = defaultUITheme
	}
	p.Zoom, p.Text = clampPct(p.Zoom), clampPct(p.Text)
	s.ui.prefs, s.ui.saved = p, true
}

// SetUIThemeHook registers a callback run (in its own goroutine) whenever the
// theme changes — the Wails front end repaints its title bar/background with it.
func (s *Server) SetUIThemeHook(f func(id string)) {
	s.ui.mu.Lock()
	s.ui.onTheme = f
	s.ui.mu.Unlock()
}

// UITheme is the current theme id ("slate" when nothing was chosen).
func (s *Server) UITheme() string {
	s.ui.mu.Lock()
	defer s.ui.mu.Unlock()
	if s.ui.prefs.Theme == "" {
		return defaultUITheme
	}
	return s.ui.prefs.Theme
}

func clampPct(v int) int {
	if v == 0 {
		return 0
	}
	if v < 50 {
		return 50
	}
	if v > 250 {
		return 250
	}
	return v
}

func (s *Server) uiView() map[string]any {
	s.ui.mu.Lock()
	defer s.ui.mu.Unlock()
	t := s.ui.prefs.Theme
	if t == "" {
		t = defaultUITheme
	}
	z, x := s.ui.prefs.Zoom, s.ui.prefs.Text
	if z == 0 {
		z = 100
	}
	if x == 0 {
		x = 100
	}
	return map[string]any{"theme": t, "zoom": z, "text": x, "saved": s.ui.saved}
}

// handleGetUISettings: GET /api/settings/ui → {theme, zoom, text, saved}.
func (s *Server) handleGetUISettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.uiView())
}

// handleSetUISettings: POST /api/settings/ui {theme?, zoom?, text?} — absent
// fields keep their value. Unknown theme ids are rejected.
func (s *Server) handleSetUISettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Theme *string `json:"theme"`
		Zoom  *int    `json:"zoom"`
		Text  *int    `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var theme string
	if req.Theme != nil {
		id, ok := normalizeUITheme(*req.Theme)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown theme")
			return
		}
		theme = id
	}
	s.ui.mu.Lock()
	p := s.ui.prefs
	changed := false
	if theme != "" {
		changed = theme != p.Theme && !(p.Theme == "" && theme == defaultUITheme)
		p.Theme = theme
	}
	if p.Theme == "" {
		p.Theme = defaultUITheme
	}
	if req.Zoom != nil {
		p.Zoom = clampPct(*req.Zoom)
	}
	if req.Text != nil {
		p.Text = clampPct(*req.Text)
	}
	s.ui.prefs, s.ui.saved = p, true
	path, hook := s.ui.path, s.ui.onTheme
	if path != "" {
		if b, err := json.MarshalIndent(p, "", "  "); err == nil {
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			tmp := path + ".tmp"
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, path)
			}
		}
	}
	s.ui.mu.Unlock()
	if changed && hook != nil {
		go hook(p.Theme)
	}
	writeJSON(w, http.StatusOK, s.uiView())
}

// injectTheme stamps data-theme="<id>" into the page's <html …> tag (the ids are
// from the fixed uiThemes set, so no escaping is needed). A page that already
// carries a data-theme attribute is left alone.
func injectTheme(page []byte, id string) []byte {
	i := bytes.Index(page, []byte("<html"))
	if i < 0 {
		return page
	}
	j := bytes.IndexByte(page[i:], '>')
	if j < 0 {
		return page
	}
	j += i
	if bytes.Contains(page[i:j], []byte("data-theme")) {
		return page
	}
	out := make([]byte, 0, len(page)+32)
	out = append(out, page[:j]...)
	out = append(out, ` data-theme="`...)
	out = append(out, id...)
	out = append(out, '"')
	out = append(out, page[j:]...)
	return out
}

// servePage serves one of the embedded HTML windows with the saved theme stamped in.
func (s *Server) servePage(page []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(injectTheme(page, s.UITheme()))
	}
}
