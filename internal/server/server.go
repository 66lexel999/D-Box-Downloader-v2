// Package server exposes the engine over a local REST API + SSE stream and
// serves the embedded single-file web UI (stand-in for the reference repo's
// gRPC-gateway + React SPA).
package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"myidm/internal/engine"
	"myidm/internal/procutil"
	"myidm/internal/updater"
	"myidm/internal/ytdlp"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/add.html
var addHTML []byte

//go:embed web/done.html
var doneHTML []byte

//go:embed web/detail.html
var detailHTML []byte

//go:embed web/icons/appicon.png
var appiconPNG []byte

//go:embed web/icons/picture.png
var picturePNG []byte

//go:embed web/icons/video.png
var videoPNG []byte

//go:embed web/icons/compress.png
var compressPNG []byte

// extCategory maps a lowercase file extension to an IDM-style category name,
// so the add dialog can preselect the right category. General is the fallback.
// The table lives in the engine, which the Link Grabber also uses.
var extCategory = engine.ExtCategory

// categoryOrder is the display order of categories in the add dialog.
var categoryOrder = []string{"General", "Compressed", "Documents", "Music", "Video", "Programs", "Images"}

type Server struct {
	eng    *engine.Engine
	log    *slog.Logger
	probes *probeCache
	dialog func(q url.Values) error             // opens the native New Download window; nil = headless
	detail func(id string) error                // opens the native per-connection progress window; nil = headless
	done   func(id string) error                // opens the native completion window on demand; nil = headless
	pick   func(initial string) (string, error) // native "choose folder" dialog (Options); nil = none
	quit   func()                               // request a real app exit (close-to-tray mode); nil = N/A

	startupGet func() (bool, error) // is D BOX set to start with Windows? nil = option not offered
	startupSet func(on bool) error  // turn starting with Windows on/off
	show       func() bool          // bring the main window up; false = can't (closing); nil = no window

	fileIcon    func(path string) ([]byte, bool, error) // shell icon as PNG + degraded flag; nil = none
	iconGeneric func(ext string) bool                   // ext (".zip") has only a generic/Explorer default handler
	iconMu      sync.Mutex
	iconCache   map[string]iconEntry

	webMode        bool       // single-window HTML UI (Wails): relay prompts to the page over SSE
	promptMu       sync.Mutex // guards pendingPrompts
	pendingPrompts []string   // encoded query strings awaiting the frontend (POST /api/prompt from the extension)
	activate       func()     // brings the single window to the foreground (Wails); nil = no-op

	closeMu      sync.Mutex       // guards popupCloseAt
	popupCloseAt map[string]int64 // task id -> client wall-clock ms of the latest popup-close request

	ctxs ctxStore // browser context (referer/headers) handed from /api/prompt to the dialog

	ui uiState // theme / zoom / text size shared by every window (uitheme.go)

	grabs    *engine.Grabber // Link Grabber jobs (grab.go)
	hasYtdlp func() bool     // ytdlp.Available; tests stub it

	updCurrent  string     // running version ("dev" disables updates)
	updManifest string     // latest.json URL ("" disables updates)
	updExit     func()     // fully exit the process (for the post-update relaunch)
	updMu       sync.Mutex // guards the fields below
	updProg     updater.Progress
	updApplying bool
}

// iconEntry is one memoized shell-icon extraction; png == nil records a miss,
// degraded marks an extension-fallback result — both retry after a short TTL
// (see iconFor).
type iconEntry struct {
	png      []byte
	at       time.Time
	degraded bool
}

func New(eng *engine.Engine, log *slog.Logger) *Server {
	s := &Server{eng: eng, log: log, popupCloseAt: map[string]int64{}, grabs: engine.NewGrabber(eng),
		hasYtdlp: ytdlp.Available}
	s.probes = newProbeCache(func(ctx context.Context, url string, o ytdlp.Opts) (*ytdlp.ProbeResult, error) {
		return ytdlp.Probe(ctx, url, o)
	})
	return s
}

// SetWebMode makes the server relay /api/prompt requests (e.g. from the browser
// extension) to the single-window HTML UI over SSE instead of opening a native
// window. Used by the Wails front end.
func (s *Server) SetWebMode(v bool) { s.webMode = v }

// SetActivateFunc registers a callback that brings the single window to the
// foreground (called when a New Download prompt arrives so the user sees it).
func (s *Server) SetActivateFunc(f func()) { s.activate = f }

// queuePrompt stores an encoded New Download query for the frontend to poll, and
// raises the window so the in-page dialog is visible.
func (s *Server) queuePrompt(q string) {
	s.promptMu.Lock()
	s.pendingPrompts = append(s.pendingPrompts, q)
	s.promptMu.Unlock()
	if s.activate != nil {
		s.activate()
	}
}

// drainPrompts returns and clears the queued prompts.
func (s *Server) drainPrompts() []string {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	if len(s.pendingPrompts) == 0 {
		return nil
	}
	out := s.pendingPrompts
	s.pendingPrompts = nil
	return out
}

// SetDialogOpener wires the native "New Download" window. When set (GUI mode),
// POST /api/prompt opens it; when nil (headless), callers fall back to a popup.
func (s *Server) SetDialogOpener(f func(url.Values) error) { s.dialog = f }

// SetDetailOpener wires the native per-connection progress window (POST /api/detail).
func (s *Server) SetDetailOpener(f func(id string) error) { s.detail = f }

// SetDoneOpener wires the native completion window so it can be opened on demand
// (POST /api/done, used by the UI's "Preview" button). The engine's completion
// notifier pops the same window normally when a download finishes.
func (s *Server) SetDoneOpener(f func(id string) error) { s.done = f }

// SetFolderPicker wires the native "choose folder" dialog used by the Options
// panel (POST /api/pick-folder). nil = no native picker (UI falls back to typing).
func (s *Server) SetFolderPicker(f func(initial string) (string, error)) { s.pick = f }

// SetQuit wires a real application exit (File → Exit, when the window otherwise
// closes to the tray). nil = no-op.
func (s *Server) SetQuit(f func()) { s.quit = f }

// SetShowFunc wires POST /api/show: a second launch of D BOX handing over to
// this one. f brings the main window up, or reports false when the app is
// closing (the new launch then takes over once this one has exited).
func (s *Server) SetShowFunc(f func() bool) { s.show = f }

// SetStartup wires the "Start D BOX when Windows starts" option in Settings:
// get reports the current state, set changes it. Unset = option hidden.
func (s *Server) SetStartup(get func() (bool, error), set func(on bool) error) {
	s.startupGet, s.startupSet = get, set
}

// SetIconResolver wires native per-file icon extraction (GET /api/icon) and the
// "does this extension only have a generic default handler?" check. The icon
// func's degraded flag means "extension fallback used although the file exists"
// — cached briefly so the real icon can take over on a retry.
func (s *Server) SetIconResolver(icon func(string) ([]byte, bool, error), generic func(string) bool) {
	s.fileIcon, s.iconGeneric = icon, generic
}

// handleIcon serves the Windows shell icon for ?path= as a PNG. For an archive
// whose only handler is the generic Windows one (no WinRAR etc.), it 404s so the
// UI shows its own category icon instead.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || s.fileIcon == nil {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(path))
	if extCategory[strings.TrimPrefix(ext, ".")] == "Compressed" && s.iconGeneric != nil && s.iconGeneric(ext) {
		http.NotFound(w, r)
		return
	}
	b, degraded := s.iconFor(path)
	if len(b) == 0 {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"%x-%x"`, len(b), fnv32(b))
	w.Header().Set("ETag", etag)
	if r.URL.Query().Get("v") != "" && !degraded {
		// The UI versions the URL with the file's mtime+size, so a given URL
		// always means the same icon: let the WebView keep it instead of
		// revalidating on every table repaint.
		w.Header().Set("Cache-Control", "private, max-age=86400")
	} else {
		// Revalidate: a degraded icon (extension fallback while AV held the
		// file) upgrades to the real one within ~45s.
		w.Header().Set("Cache-Control", "no-cache")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(b)
}

// fnv32 is a tiny content hash for icon ETags.
func fnv32(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h = (h ^ uint32(c)) * 16777619
	}
	return h
}

// iconFor extracts (and memoizes) a file's icon. The cache key is the file's
// identity — path plus modification time and size — not just its path: a new
// download that lands on the same path as an earlier file (the earlier one was
// deleted, or a "setup.exe" replaced) must show ITS icon, not the old app's.
// Clean successes cache for the file's lifetime. Failures AND degraded results
// (extension fallback while the file exists — e.g. AV still scanning a fresh
// .exe) cache only briefly so a retry can produce the real icon.
func (s *Server) iconFor(path string) ([]byte, bool) {
	const retryTTL = 45 * time.Second
	key := path
	if st, err := os.Stat(path); err == nil {
		key = fmt.Sprintf("%s|%d|%d", path, st.ModTime().UnixNano(), st.Size())
	}
	s.iconMu.Lock()
	if e, ok := s.iconCache[key]; ok {
		if (e.png != nil && !e.degraded) || time.Since(e.at) < retryTTL {
			s.iconMu.Unlock()
			return e.png, e.degraded
		}
		delete(s.iconCache, key) // expired miss/degraded entry — try again
	}
	s.iconMu.Unlock()
	b, degraded, err := s.fileIcon(path)
	if err != nil {
		b = nil
	}
	s.iconMu.Lock()
	if s.iconCache == nil {
		s.iconCache = map[string]iconEntry{}
	}
	if len(s.iconCache) > 2000 { // bound memory over a long session
		s.iconCache = map[string]iconEntry{}
	}
	s.iconCache[key] = iconEntry{png: b, at: time.Now(), degraded: degraded}
	s.iconMu.Unlock()
	return b, degraded
}

// probeCache memoizes yt-dlp probe results so the browser overlay can prefetch
// a video's formats the moment its download button appears and then open the
// menu instantly. Concurrent callers for the same URL share one yt-dlp run
// (singleflight); the run uses a background context so a client that navigates
// away mid-prefetch still warms the cache for the next visitor. Entries are
// keyed by URL + Referer, since an embedded player may only answer with the
// right page as Referer.
type probeCache struct {
	mu      sync.Mutex
	entries map[string]*probeEntry
	okTTL   time.Duration
	errTTL  time.Duration
	probe   func(context.Context, string, ytdlp.Opts) (*ytdlp.ProbeResult, error)
}

type probeEntry struct {
	url  string
	done chan struct{}
	res  *ytdlp.ProbeResult
	err  error
	at   time.Time // when res/err were stored (zero while in flight)
}

func newProbeCache(probe func(context.Context, string, ytdlp.Opts) (*ytdlp.ProbeResult, error)) *probeCache {
	return &probeCache{
		entries: make(map[string]*probeEntry),
		okTTL:   10 * time.Minute, // probe yields stable metadata; URLs re-resolve at download time
		errTTL:  15 * time.Second, // let failures retry soon without hammering yt-dlp
		probe:   probe,
	}
}

// get returns a cached or freshly-probed result for url, deduping concurrent
// callers. waitCtx bounds only the caller's wait, never the probe itself.
func (c *probeCache) get(waitCtx context.Context, url string, o ytdlp.Opts) (*ytdlp.ProbeResult, error) {
	key := probeKey(url, o)
	c.mu.Lock()
	if e := c.entries[key]; e != nil && e.fresh(c.okTTL, c.errTTL) {
		c.mu.Unlock()
		return e.wait(waitCtx)
	}
	if len(c.entries) > 64 { // opportunistic prune of expired entries
		for k, ent := range c.entries {
			if !ent.fresh(c.okTTL, c.errTTL) {
				delete(c.entries, k)
			}
		}
	}
	e := &probeEntry{url: url, done: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := c.probe(ctx, url, o)
		c.mu.Lock()
		e.res, e.err, e.at = res, err, time.Now()
		c.mu.Unlock()
		close(e.done)
	}()
	return e.wait(waitCtx)
}

// probeKey identifies a probe: the normalized URL (so /api/probe, /api/inspect
// and the grabber share entries whatever spelling they got), the Referer, and
// a hash of everything else that changes what the site answers — the cookies
// and headers sent, the User-Agent, and whether the shared cookie jar has a
// login for the site. A logged-out teaser probed before the extension shared
// cookies must not be served to a request that carries them.
func probeKey(rawURL string, o ytdlp.Opts) string {
	norm := strings.TrimSpace(rawURL)
	if u, err := url.Parse(norm); err == nil && u.Host != "" {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		if p := u.Port(); (u.Scheme == "https" && p == "443") || (u.Scheme == "http" && p == "80") {
			u.Host = u.Hostname()
		}
		if u.Path == "" {
			u.Path = "/"
		}
		u.Fragment, u.RawFragment = "", ""
		norm = u.String()
	}
	keys := make([]string, 0, len(o.Headers))
	for k := range o.Headers {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(o.UserAgent)
	for _, k := range keys {
		for hk, v := range o.Headers {
			if strings.ToLower(hk) == k {
				b.WriteString("\n" + k + ":" + v)
			}
		}
	}
	if ytdlp.HasCookiesFor(rawURL) {
		b.WriteString("\njar")
	}
	if o.PlaylistItem > 0 {
		b.WriteString(" item=" + strconv.Itoa(o.PlaylistItem))
	}
	if o.Impersonate {
		b.WriteString("\nimpersonate")
	}
	return norm + "\x00" + o.Referer + "\x00" + strconv.FormatUint(uint64(fnv32([]byte(b.String()))), 36)
}

// forget drops cached entries whose URL matches pred — used when fresh cookies
// arrive so a prior anonymous failure re-probes at once instead of serving the
// stale "login required" for the rest of its error-TTL.
func (c *probeCache) forget(pred func(url string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		select {
		case <-e.done: // only evict finished entries; leave in-flight singleflights alone
			if pred(e.url) {
				delete(c.entries, k)
			}
		default:
		}
	}
}

// fresh reports whether the entry is still in flight or within its TTL.
// Caller holds c.mu.
func (e *probeEntry) fresh(okTTL, errTTL time.Duration) bool {
	select {
	case <-e.done:
		ttl := okTTL
		switch {
		case e.err != nil:
			ttl = errTTL
		case e.res == nil || (len(e.res.Options) == 0 && len(e.res.Entries) == 0):
			ttl = min(okTTL, time.Minute) // "nothing found" may just be a logged-out view
		}
		return time.Since(e.at) < ttl
	default:
		return true // still running — reuse it
	}
}

// wait blocks until the probe completes or the caller's context is done.
func (e *probeEntry) wait(ctx context.Context) (*ytdlp.ProbeResult, error) {
	select {
	case <-e.done:
		return e.res, e.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Every window's page gets the saved theme stamped into <html data-theme>
	// (uitheme.go) so separate popup processes paint the same theme pre-first-frame.
	mux.HandleFunc("GET /{$}", s.servePage(indexHTML))
	// Static icon assets (menu-bar logo, browser tab, the Images category icon).
	pngAsset := func(b []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			// no-cache (revalidate) so a rebuilt icon shows immediately rather than
			// being served stale from WebView2's disk cache at the same URL.
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(b)
		}
	}
	mux.HandleFunc("GET /icons/appicon.png", pngAsset(appiconPNG))
	mux.HandleFunc("GET /icons/picture.png", pngAsset(picturePNG))
	mux.HandleFunc("GET /icons/video.png", pngAsset(videoPNG))
	mux.HandleFunc("GET /icons/compress.png", pngAsset(compressPNG))
	mux.HandleFunc("GET /favicon.ico", pngAsset(appiconPNG))
	// Shared design system: /ui/theme.css + /ui/icons.js (see assets.go).
	mux.Handle("GET /ui/", uiAssets())
	// Real per-file Windows shell icon (the app's own icon for an .exe, the
	// default associated app's for others) — rendered in the downloads table.
	mux.HandleFunc("GET /api/icon", s.handleIcon)
	// /add is the IDM-style "New Download" dialog the browser extension opens.
	mux.HandleFunc("GET /add", s.servePage(addHTML))
	// /done and /detail back the native completion + per-connection windows.
	mux.HandleFunc("GET /done", s.servePage(doneHTML))
	mux.HandleFunc("GET /detail", s.servePage(detailHTML))
	mux.HandleFunc("POST /api/done-suppress", s.handleDoneSuppress)
	mux.HandleFunc("POST /api/detail", s.handleOpenDetail)
	mux.HandleFunc("POST /api/done", s.handleOpenDone)
	// Popup close coordination: the completion + status windows for one download
	// are siblings — closing either closes both (they poll GET, signal via POST).
	mux.HandleFunc("GET /api/popups/{id}/close", s.handlePopupCloseGet)
	mux.HandleFunc("POST /api/popups/{id}/close", s.handlePopupClose)
	mux.HandleFunc("GET /api/categories", s.handleCategories)
	// Folder settings (Options panel): change the base download folder, pin a
	// category to a remembered folder, reset all paths, and the native picker.
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("POST /api/settings/dir", s.handleSetDir)
	mux.HandleFunc("POST /api/settings/category", s.handleSetCategory)
	mux.HandleFunc("POST /api/settings/reset", s.handleResetPaths)
	mux.HandleFunc("POST /api/settings/concurrent", s.handleSetConcurrent)
	mux.HandleFunc("POST /api/settings/shutdown", s.handleSetShutdown)
	mux.HandleFunc("GET /api/settings/ui", s.handleGetUISettings)
	mux.HandleFunc("POST /api/settings/ui", s.handleSetUISettings)
	mux.HandleFunc("POST /api/settings/startup", s.handleSetStartup)
	mux.HandleFunc("POST /api/show", s.handleShow)
	mux.HandleFunc("POST /api/pick-folder", s.handlePickFolder)
	mux.HandleFunc("POST /api/quit", s.handleQuit)
	mux.HandleFunc("GET /api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("POST /api/update/apply", s.handleUpdateApply)
	mux.HandleFunc("GET /api/update/status", s.handleUpdateStatus)
	mux.HandleFunc("GET /api/probe", s.handleProbe)
	mux.HandleFunc("GET /api/inspect", s.handleInspect)
	mux.HandleFunc("POST /api/grab", s.handleGrabStart)
	mux.HandleFunc("POST /api/grab/show", s.handleGrabShow)
	mux.HandleFunc("GET /api/grab/{id}", s.handleGrabGet)
	mux.HandleFunc("DELETE /api/grab/{id}", s.handleGrabCancel)
	mux.HandleFunc("POST /api/cookies", s.handleCookies)
	mux.HandleFunc("POST /api/video", s.handleVideo)
	mux.HandleFunc("GET /api/playlist", s.handlePlaylistInfo)
	mux.HandleFunc("POST /api/playlist", s.handlePlaylistDownload)
	mux.HandleFunc("GET /api/tasks", s.handleList)
	mux.HandleFunc("POST /api/tasks", s.handleCreate)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleGet)
	mux.HandleFunc("POST /api/tasks/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/tasks/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/tasks/{id}/schedule", s.handleSchedule)
	mux.HandleFunc("POST /api/tasks/{id}/skip", s.handleSkip)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.handleDelete)
	mux.HandleFunc("GET /api/tasks/{id}/file", s.handleFile)
	mux.HandleFunc("POST /api/tasks/{id}/open", s.handleOpen)
	mux.HandleFunc("POST /api/tasks/{id}/reveal", s.handleReveal)
	mux.HandleFunc("POST /api/tasks/{id}/rename", s.handleRename)
	mux.HandleFunc("POST /api/tasks/{id}/move", s.handleMove)
	mux.HandleFunc("POST /api/prompt", s.handlePrompt)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/prompts", s.handlePromptsPoll)
	return mux
}

// handlePromptsPoll returns and clears any queued New Download prompts. Polled by
// the single-window UI (Wails) since SSE can't stream through its asset server.
func (s *Server) handlePromptsPoll(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"prompts": s.drainPrompts()})
}

// browserContext is the optional page context a caller (the browser extension,
// the New Download dialog) attaches so the download's requests look like the
// page's own: many video hosts refuse requests without the right Referer or
// the site's cookies.
type browserContext struct {
	Referer   string            `json:"referer,omitempty"`   // page the link / video was found on
	Referrer  string            `json:"referrer,omitempty"`  // alias (the DOM's document.referrer spelling)
	PageURL   string            `json:"pageUrl,omitempty"`   // alias of referer
	Headers   map[string]string `json:"headers,omitempty"`   // extra request headers (Origin, Authorization, …)
	Cookies   string            `json:"cookies,omitempty"`   // "a=1; b=2" for the file's site
	UserAgent string            `json:"userAgent,omitempty"` // the browser's own agent (Cloudflare ties cookies to it)
	Ctx       string            `json:"ctx,omitempty"`       // token from /api/prompt (dialog round-trip)
}

// resolveContext merges a stored prompt context with explicit fields.
func (s *Server) resolveContext(bc browserContext) (referer string, headers map[string]string) {
	headers = map[string]string{}
	if bc.Ctx != "" {
		if rc, ok := s.ctxs.get(bc.Ctx); ok {
			referer = rc.referer
			for k, v := range rc.headers {
				headers[k] = v
			}
		}
	}
	for _, r := range []string{bc.Referer, bc.Referrer, bc.PageURL} {
		if strings.TrimSpace(r) != "" {
			referer = r
			break
		}
	}
	for k, v := range bc.Headers {
		headers[k] = v
	}
	if c := strings.TrimSpace(bc.Cookies); c != "" {
		headers["Cookie"] = c
	}
	if ua := strings.TrimSpace(bc.UserAgent); ua != "" {
		headers["User-Agent"] = ua
	}
	return referer, engine.CleanHeaders(headers)
}

type createRequest struct {
	URL         string `json:"url"`
	FileName    string `json:"fileName,omitempty"`
	Segments    int    `json:"segments,omitempty"`
	Category    string `json:"category,omitempty"`    // IDM-style category -> folder
	Dir         string `json:"dir,omitempty"`         // explicit folder; wins over Category
	ScheduleAt  int64  `json:"scheduleAt,omitempty"`  // epoch ms; >0 holds the task until then
	Later       bool   `json:"later,omitempty"`       // hold in the "Download Later" queue (start manually)
	Description string `json:"description,omitempty"` // user note (New Download dialog)
	Title       string `json:"title,omitempty"`       // naming hint (page/video title) for generic server names
	browserContext
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	dir := req.Dir
	if dir == "" && req.Category != "" {
		dir = s.eng.CategoryDir(req.Category)
	}
	referer, headers := s.resolveContext(req.browserContext)
	view, err := s.eng.AddWithOptions(req.URL, engine.AddOptions{
		FileName: req.FileName, Segments: req.Segments, Dir: dir,
		ScheduledAt: epochMillis(req.ScheduleAt), Later: req.Later,
		Referer: referer, Headers: headers, Title: req.Title, Description: req.Description,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// epochMillis converts an optional epoch-millisecond timestamp (0 = unset) to a
// time.Time, returning the zero time when unset.
func epochMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

type categoryView struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// handleCategories feeds the add dialog: ordered category->folder list plus the
// extension->category map for preselection.
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	cats := s.eng.Categories()
	seen := make(map[string]bool, len(cats))
	list := make([]categoryView, 0, len(cats))
	for _, name := range categoryOrder {
		if d, ok := cats[name]; ok {
			list = append(list, categoryView{Name: name, Dir: d})
			seen[name] = true
		}
	}
	for name, d := range cats {
		if !seen[name] {
			list = append(list, categoryView{Name: name, Dir: d})
		}
	}
	// "gui" => the server pops its OWN native top-level windows for dialogs and
	// completion (walk/webview). When false (e.g. Wails single-window), the HTML
	// renders those as in-page modals instead. Keyed off whether a native dialog
	// opener was wired, NOT cfg.GUI (Wails is a GUI but uses in-page modals).
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": list, "extMap": extCategory, "ytdlp": ytdlp.Available(), "gui": s.dialog != nil})
}

// settingsView is the folder-settings payload shared by the settings endpoints.
func (s *Server) settingsView() map[string]any {
	dir, cats, custom, def, maxc := s.eng.Settings()
	v := map[string]any{"downloadDir": dir, "categories": cats, "custom": custom, "defaultDir": def,
		"maxConcurrent": maxc, "shutdownWhenDone": s.eng.ShutdownWhenDone(),
		"startWithWindowsSupported": s.startupGet != nil}
	if s.startupGet != nil {
		on, err := s.startupGet()
		if err != nil {
			s.log.Warn("start with Windows: can't read the sign-in entry", "err", err)
		}
		v["startWithWindows"] = on
	}
	return v
}

// handleSetStartup turns "Start D BOX when Windows starts" on or off.
func (s *Server) handleSetStartup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if s.startupSet == nil {
		writeError(w, http.StatusNotImplemented, "starting with Windows isn't available in this build")
		return
	}
	if err := s.startupSet(req.On); err != nil {
		writeError(w, http.StatusInternalServerError, "couldn't change the Windows startup setting: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handleShow brings the main window up — sent by a second launch of D BOX
// (desktop icon, Start menu) to the copy already running, e.g. hidden in the
// tray since sign-in. 503 while this copy is closing, so the new launch waits
// and takes over instead of handing off to a window that's going away.
func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	if s.show == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"shown": false}) // headless: no window to show
		return
	}
	if !s.show() {
		writeError(w, http.StatusServiceUnavailable, "D BOX is closing")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"shown": true})
}

// handleSetShutdown arms/disarms "turn off the PC when all downloads finish".
func (s *Server) handleSetShutdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	s.eng.SetShutdownWhenDone(req.On)
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handleSetConcurrent changes how many downloads run at once (Options panel).
func (s *Server) handleSetConcurrent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		N int `json:"n"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.eng.SetMaxConcurrent(req.N); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handleSetDir changes the base download folder (new downloads only).
func (s *Server) handleSetDir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.eng.SetDownloadDir(req.Dir); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handleSetCategory pins a category to a remembered folder ("remember this path").
func (s *Server) handleSetCategory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Category string `json:"category"`
		Dir      string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.eng.SetCategoryDir(req.Category, req.Dir); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

func (s *Server) handleResetPaths(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.ResetPaths(); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handlePickFolder shows the native folder chooser (Options "Browse"). Returns
// the chosen path, or an empty path if cancelled / no native picker is wired.
// handleQuit performs a real app exit (File → Exit) for the close-to-tray UI.
func (s *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	if s.quit != nil {
		go s.quit() // async so this response is sent before the app tears down
	}
}

// SetUpdateSource wires the in-app updater: current is the running version and
// manifestURL is the static latest.json to check. exit fully terminates the
// process (used after the new exe is swapped in, so the relauncher can take
// over). Any empty argument disables update checks.
func (s *Server) SetUpdateSource(current, manifestURL string, exit func()) {
	s.updCurrent, s.updManifest, s.updExit = current, manifestURL, exit
}

func (s *Server) updatesEnabled() bool {
	return s.updManifest != "" && s.updCurrent != "" && s.updCurrent != "dev"
}

// handleUpdateCheck fetches latest.json and reports whether a newer build exists.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.updatesEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "current": s.updCurrent})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	rel, err := updater.Fetch(ctx, s.updManifest, s.eng.UserAgent())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "current": s.updCurrent, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"current":   s.updCurrent,
		"latest":    rel.Version,
		"available": updater.Newer(rel.Version, s.updCurrent),
		"notes":     rel.Notes,
		"date":      rel.Date,
		"mandatory": rel.Mandatory,
	})
}

// handleUpdateApply downloads the new executable, swaps it in, and schedules a
// relaunch. It returns immediately; the UI polls /api/update/status. On success
// the process exits (via s.updExit) so the detached relauncher can start the
// new build.
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.updatesEnabled() {
		writeError(w, http.StatusBadRequest, "updates are not enabled for this build")
		return
	}
	s.updMu.Lock()
	if s.updApplying {
		s.updMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"started": true, "alreadyRunning": true})
		return
	}
	s.updApplying = true
	s.updProg = updater.Progress{Phase: "checking"}
	s.updMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		rel, err := updater.Fetch(ctx, s.updManifest, s.eng.UserAgent())
		if err == nil && !updater.Newer(rel.Version, s.updCurrent) {
			err = fmt.Errorf("already up to date")
		}
		if err != nil {
			s.setUpdateProgress(updater.Progress{Phase: "error", Error: err.Error()})
			s.updMu.Lock()
			s.updApplying = false
			s.updMu.Unlock()
			return
		}
		err = updater.Apply(ctx, rel, s.eng.UserAgent(), s.setUpdateProgress)
		if err != nil {
			s.setUpdateProgress(updater.Progress{Phase: "error", Error: err.Error()})
			s.updMu.Lock()
			s.updApplying = false
			s.updMu.Unlock()
			return
		}
		// New exe is in place and the relauncher is armed — persist state and exit
		// so the port frees and the relauncher starts the new build.
		s.log.Info("update applied — restarting", "to", rel.Version)
		time.Sleep(600 * time.Millisecond) // let the UI poll the "restarting" phase once
		if s.updExit != nil {
			s.updExit()
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

func (s *Server) setUpdateProgress(p updater.Progress) {
	s.updMu.Lock()
	s.updProg = p
	s.updMu.Unlock()
}

// handleUpdateStatus reports the in-flight apply progress for the UI to poll.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	s.updMu.Lock()
	p := s.updProg
	applying := s.updApplying
	s.updMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"applying":   applying,
		"phase":      p.Phase,
		"downloaded": p.Downloaded,
		"total":      p.Total,
		"error":      p.Error,
	})
}

func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	if s.pick == nil {
		writeJSON(w, http.StatusOK, map[string]any{"path": "", "native": false})
		return
	}
	var req struct {
		Initial string `json:"initial"`
	}
	json.NewDecoder(r.Body).Decode(&req) // body optional
	p, err := s.pick(req.Initial)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "native": true})
}

// handleDoneSuppress hides the completion popup for a while (the "don't show for
// 2 hours" checkbox in the native done window). Defaults to 2h.
func (s *Server) handleDoneSuppress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds int `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	d := 2 * time.Hour
	if req.Seconds > 0 {
		d = time.Duration(req.Seconds) * time.Second
	}
	s.eng.SuppressCompletionPopup(d)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleOpenDetail opens the native per-connection progress window for a task.
func (s *Server) handleOpenDetail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, "missing id")
		return
	}
	if s.detail == nil {
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	if err := s.detail(req.ID); err != nil {
		s.log.Warn("failed to open detail window", "id", req.ID, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"native": true})
}

// handleOpenDone opens the native completion window for a task on demand (the
// "Preview" button). Normally the engine pops this when a download finishes.
func (s *Server) handleOpenDone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, "missing id")
		return
	}
	if s.done == nil {
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	if err := s.done(req.ID); err != nil {
		s.log.Warn("failed to open completion window", "id", req.ID, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"native": true})
}

// Popup close coordination ---------------------------------------------------
//
// A download's completion ("done") and status ("detail") windows are siblings:
// closing either should close both. In the default Wails UI each popup is its
// OWN process, so they can't share an in-process handle — they coordinate
// through the server instead. A popup's Close POSTs the current wall-clock time
// here; the sibling, which polls this value, closes itself once it sees a close
// stamped AFTER it opened. Comparing client timestamps is safe because every
// popup runs on the same machine (one wall clock), and "newer than my open time"
// (rather than a bare flag) means reopening a window for the same id later isn't
// instantly dismissed by a stale signal.

func (s *Server) handlePopupClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		At int64 `json:"at"`
	}
	json.NewDecoder(r.Body).Decode(&req) // best-effort; a missing/zero "at" is a harmless no-op
	id := r.PathValue("id")
	s.closeMu.Lock()
	if req.At > s.popupCloseAt[id] {
		s.popupCloseAt[id] = req.At
	}
	at := s.popupCloseAt[id]
	s.closeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"at": at})
}

func (s *Server) handlePopupCloseGet(w http.ResponseWriter, r *http.Request) {
	s.closeMu.Lock()
	at := s.popupCloseAt[r.PathValue("id")]
	s.closeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"at": at})
}

// handleProbe asks yt-dlp what's downloadable at a page URL (for the overlay's
// quality dropdown).
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	referer, headers := s.resolveContext(browserContext{Referer: r.URL.Query().Get("referer"), Ctx: r.URL.Query().Get("ctx")})
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.probes.get(ctx, u, ytdlp.Opts{UserAgent: s.eng.UserAgent(), Referer: referer, Headers: headers})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// inspectResponse is GET /api/inspect's answer.
type inspectResponse struct {
	engine.InspectResult
	Error      string             `json:"error,omitempty"`
	Video      *ytdlp.ProbeResult `json:"video,omitempty"`      // formats, when the URL is a video page / DASH
	VideoError string             `json:"videoError,omitempty"` // why yt-dlp found no video
	Ytdlp      bool               `json:"ytdlp"`
	// Entries/Recommended repeat video.entries/recommended: every media item
	// of a page that holds several, and the 1-based index of the main one.
	Entries     []ytdlp.ProbeEntry `json:"entries,omitempty"`
	Recommended int                `json:"recommended,omitempty"`
	// ProbeError is the native probe's refusal (403, Cloudflare, timeout) when
	// yt-dlp got the video anyway; "error" is then left empty and kind is "page".
	ProbeError string `json:"probeError,omitempty"`
}

// Inspect budgets: the native probe gets inspectNative, the whole answer
// (with yt-dlp running alongside) inspectTotal — under the dialog's 90 s abort.
const (
	inspectNative = 30 * time.Second
	inspectTotal  = 75 * time.Second
)

// handleInspect runs the download probe server-side for the New Download
// dialog: real file name, size and kind (file / stream / page). Browsers can't
// do this themselves — a cross-origin HEAD from the dialog is blocked by CORS
// on nearly every site, which is why it used to show "unknown" and the raw URL
// name. For a web page or DASH manifest it also asks yt-dlp for the video's
// title and qualities — started at once, alongside the native probe, when the
// address looks like a page, and also when the native probe is refused
// (Cloudflare, hotlink 403, timeout): a download of that address would hand
// it to yt-dlp too. Errors come back as 200 + "error" for the dialog to show.
func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u := strings.TrimSpace(q.Get("url"))
	if u == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	referer, headers := s.resolveContext(browserContext{Referer: q.Get("referer"), Ctx: q.Get("ctx")})
	out := inspectResponse{Ytdlp: s.hasYtdlp()}
	ctx, cancel := context.WithTimeout(r.Context(), inspectTotal)
	defer cancel()

	type videoAnswer struct {
		v   *ytdlp.ProbeResult
		err error
	}
	var video chan videoAnswer
	startVideo := func(target string) {
		if video != nil || !out.Ytdlp {
			return
		}
		video = make(chan videoAnswer, 1)
		go func() {
			v, err := s.probes.get(ctx, target, ytdlp.Opts{UserAgent: s.eng.UserAgent(), Referer: referer, Headers: headers})
			video <- videoAnswer{v, err}
		}()
	}
	if pageLikeURL(u) {
		startVideo(u)
	}

	nctx, ncancel := context.WithTimeout(ctx, inspectNative)
	res, err := s.eng.Inspect(nctx, u, referer, headers, q.Get("title"))
	ncancel()
	if err != nil {
		out.Error = err.Error()
		out.URL, out.Kind, out.Size = u, "unknown", -1
		startVideo(u)
	} else {
		out.InspectResult = res
		if res.Kind == "page" || res.Kind == "dash" {
			startVideo(res.URL)
		}
	}
	if video != nil && (err != nil || res.Kind == "page" || res.Kind == "dash") {
		select {
		case a := <-video:
			switch {
			case a.err != nil:
				out.VideoError = a.err.Error()
			case a.v != nil:
				out.Video, out.Entries, out.Recommended = a.v, a.v.Entries, a.v.Recommended
				if err != nil {
					// The page refused our plain request but yt-dlp read it:
					// it's a video page, and the download will work.
					out.ProbeError, out.Error, out.Kind = out.Error, "", "page"
				}
			}
		case <-ctx.Done():
			out.VideoError = "yt-dlp is taking too long to read this page"
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// pageLikeURL reports an address that is probably a web page (no file
// extension, or a server-page one): worth asking yt-dlp right away rather
// than after the native probe.
func pageLikeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(u.Path), "."))
	switch ext {
	case "", "html", "htm", "shtml", "php", "asp", "aspx", "jsp", "cfm":
		return true
	}
	return false
}

// handleCookies stores browser-supplied login cookies for a domain so yt-dlp can
// read login-gated media (Instagram stories/reels, etc.). The extension holds
// the live browser session and posts the cookies here; yt-dlp's own
// --cookies-from-browser can't (the running browser locks its cookie DB).
func (s *Server) handleCookies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string         `json:"url"`
		Cookies []ytdlp.Cookie `json:"cookies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	if err := ytdlp.WriteCookies(req.URL, req.Cookies); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A previously-cached anonymous failure for this site should re-probe now.
	if dom := ytdlp.RegistrableDomain(req.URL); dom != "" {
		s.probes.forget(func(u string) bool { return ytdlp.RegistrableDomain(u) == dom })
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(req.Cookies)})
}

type videoRequest struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Selector    string `json:"selector"`
	Ext         string `json:"ext"`
	Audio       bool   `json:"audio"`
	Category    string `json:"category"`
	Dir         string `json:"dir"`
	ScheduleAt  int64  `json:"scheduleAt,omitempty"`  // epoch ms; >0 holds the task until then
	Later       bool   `json:"later,omitempty"`       // hold in the "Download Later" queue
	Description string `json:"description,omitempty"` // user note (New Download dialog)
	Size        int64  `json:"size,omitempty"`        // estimated bytes from the probe (shown until real totals arrive)
	// PlaylistItem picks one entry (1-based, ytdlp --playlist-items) of a
	// page holding several videos — the Link Grabber's per-entry rows.
	PlaylistItem int `json:"playlistItem,omitempty"`
	browserContext
}

// handleVideo queues a yt-dlp download chosen in the overlay.
func (s *Server) handleVideo(w http.ResponseWriter, r *http.Request) {
	var req videoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	dir := req.Dir
	if dir == "" {
		cat := req.Category
		if cat == "" {
			cat = "Video"
		}
		dir = s.eng.CategoryDir(cat)
	}
	referer, headers := s.resolveContext(req.browserContext)
	view, err := s.eng.AddVideoWithOptions(req.URL, engine.VideoOptions{
		Title: req.Title, Selector: req.Selector, Ext: req.Ext, Dir: dir, Audio: req.Audio,
		ScheduledAt: epochMillis(req.ScheduleAt), Later: req.Later,
		Referer: referer, Headers: headers, SizeHint: req.Size, PlaylistItem: req.PlaylistItem,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if d := strings.TrimSpace(req.Description); d != "" {
		s.eng.SetDescription(view.ID, d)
		view.Description = d
	}
	writeJSON(w, http.StatusCreated, view)
}

// handlePlaylistInfo reports whether ?url= is a playlist and, if so, its title
// and video count — so the UI can offer "download the whole playlist". A probe
// failure is reported as not-a-playlist so the caller falls back to single-video.
func (s *Server) handlePlaylistInfo(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	q := r.URL.Query()
	referer, headers := s.resolveContext(browserContext{Referer: q.Get("referer"), Ctx: q.Get("ctx")})
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	pl, err := ytdlp.ProbePlaylist(ctx, u, ytdlp.Opts{UserAgent: s.eng.UserAgent(), Referer: referer, Headers: headers})
	if err != nil || pl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"isPlaylist": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"isPlaylist": true, "title": pl.Title, "count": len(pl.Entries), "entries": pl.Entries, "mix": pl.Mix})
}

// handlePlaylistDownload queues a yt-dlp task per video (best quality, or audio).
// The UI sends the exact videos the user checked in "entries"; if absent, the
// whole playlist is enumerated and queued. Body: {url, audio, category, dir,
// later, entries:[{url,title}]} plus the optional browser context (referer,
// ctx, cookies, headers), which every queued video carries.
func (s *Server) handlePlaylistDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string                `json:"url"`
		Audio    bool                  `json:"audio"`
		Category string                `json:"category"`
		Dir      string                `json:"dir"`
		Later    bool                  `json:"later"`
		Entries  []ytdlp.PlaylistEntry `json:"entries"`
		browserContext
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	referer, headers := s.resolveContext(req.browserContext)
	entries := req.Entries
	title := ""
	if len(entries) == 0 { // no explicit selection — enumerate the whole playlist
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		pl, err := ytdlp.ProbePlaylist(ctx, req.URL, ytdlp.Opts{UserAgent: s.eng.UserAgent(), Referer: referer, Headers: headers})
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if pl == nil {
			writeError(w, http.StatusBadRequest, "not a playlist")
			return
		}
		entries, title = pl.Entries, pl.Title
	}
	dir := req.Dir
	if dir == "" {
		cat := req.Category
		if cat == "" {
			cat = "Video"
		}
		dir = s.eng.CategoryDir(cat)
	}
	ext := "mp4"
	if req.Audio {
		ext = "mp3"
	}
	added := 0
	for _, e := range entries {
		if strings.TrimSpace(e.URL) == "" {
			continue
		}
		if _, err := s.eng.AddVideoWithOptions(e.URL, engine.VideoOptions{Title: e.Title, Ext: ext, Dir: dir,
			Audio: req.Audio, Later: req.Later, Referer: referer, Headers: headers}); err == nil {
			added++
		}
	}
	s.log.Info("playlist queued", "videos", added, "later", req.Later)
	writeJSON(w, http.StatusOK, map[string]any{"count": added, "title": title})
}

// handleSkip marks or clears a Download Later task's "skip" flag (excluded from
// Start All). Body {skip: true|false}.
func (s *Server) handleSkip(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Skip bool `json:"skip"`
	}
	json.NewDecoder(r.Body).Decode(&req) // missing => false (un-skip)
	if err := s.eng.SetSkip(r.PathValue("id"), req.Skip); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleSchedule sets or clears a task's scheduled start. Body {at: <epoch ms>}:
// a future time holds the task until then; 0 or omitted cancels the schedule.
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		At int64 `json:"at"`
	}
	json.NewDecoder(r.Body).Decode(&req) // a missing/zero "at" cancels the schedule
	id := r.PathValue("id")
	var err error
	if req.At <= 0 {
		err = s.eng.Unschedule(id)
	} else {
		err = s.eng.Schedule(id, time.UnixMilli(req.At))
	}
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.eng.List()})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	view, err := s.eng.Get(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.Pause(r.PathValue("id")); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.Resume(r.PathValue("id")); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	removeFile := r.URL.Query().Get("file") == "1"
	if err := s.eng.Delete(r.PathValue("id"), removeFile); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleFile streams a completed download back through the browser
// (equivalent of the reference repo's GetDownloadTaskFile).
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	path, err := s.eng.FilePath(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	http.ServeFile(w, r, path)
}

type renameRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	view, err := s.eng.RenameFile(r.PathValue("id"), req.Name)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type moveRequest struct {
	Dir string `json:"dir"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	view, err := s.eng.MoveFile(r.PathValue("id"), req.Dir)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type promptRequest struct {
	URL      string `json:"url"`
	Name     string `json:"name,omitempty"`
	Video    bool   `json:"video,omitempty"`    // true => the dialog's Start posts /api/video, not /api/tasks
	Selector string `json:"selector,omitempty"` // yt-dlp format selector
	Ext      string `json:"ext,omitempty"`      // expected container (mp4 / mp3)
	Audio    bool   `json:"audio,omitempty"`    // audio-only
	Title    string `json:"title,omitempty"`    // video title / naming hint (page title)
	browserContext
}

// handlePrompt opens the native "New Download" window for url (GUI mode). The
// response's "native" flag tells the caller (e.g. the browser extension)
// whether MyIDM handled it or they should fall back to their own popup.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var req promptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	if s.dialog == nil && !s.webMode {
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	q := url.Values{}
	q.Set("url", req.URL)
	if req.Name != "" {
		q.Set("name", req.Name)
	}
	if req.Title != "" {
		q.Set("title", req.Title)
	}
	// The page context rides along as a short-lived server-side token, not in
	// the dialog URL: that URL becomes a command-line argument of the popup
	// process, where cookies would be visible to other programs.
	if referer, headers := s.resolveContext(req.browserContext); referer != "" || len(headers) > 0 {
		q.Set("ctx", s.ctxs.put(referer, headers))
		if referer != "" {
			q.Set("referer", referer)
		}
	}
	if req.Video { // carry the chosen yt-dlp format through to the dialog
		q.Set("video", "1")
		q.Set("selector", req.Selector)
		q.Set("vext", req.Ext)
		if req.Audio {
			q.Set("audio", "1")
		}
	}
	if s.dialog != nil { // native window (walk/webview)
		if err := s.dialog(q); err != nil {
			s.log.Warn("failed to open native download dialog", "err", err)
			writeJSON(w, http.StatusOK, map[string]any{"native": false})
			return
		}
	} else { // Wails single-window: relay to the page over SSE
		q.Set("dialog", "1")
		s.queuePrompt(q.Encode())
	}
	writeJSON(w, http.StatusOK, map[string]any{"native": true})
}

// handleOpen launches the completed file with the OS default application
// (the browser can't open a local file directly).
func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	path, err := s.eng.FilePath(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "file is no longer on disk")
		return
	}
	if err := openWithDefaultApp(path); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// openWithDefaultApp opens path in whatever application the OS associates with
// its type. On Windows we route through PowerShell's Start-Process with a
// single-quoted -LiteralPath so spaces, Unicode, and shell metacharacters
// (&, %, parentheses) in the filename are handled safely.
func openWithDefaultApp(path string) error {
	switch runtime.GOOS {
	case "windows":
		// Invoke-Item — NOT Start-Process, which has no -LiteralPath parameter and
		// so errored on every open (the file never launched). Invoke-Item opens the
		// file with its default app exactly like a double-click; -LiteralPath takes
		// the path verbatim so spaces, Unicode, and []/&/% in the name are safe.
		script := "Invoke-Item -LiteralPath '" + strings.ReplaceAll(path, "'", "''") + "'"
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
		procutil.Hidden(cmd)
		return cmd.Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// handleReveal opens Explorer with the file selected. Local-app nicety.
func (s *Server) handleReveal(w http.ResponseWriter, r *http.Request) {
	path, err := s.eng.FilePath(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if runtime.GOOS == "windows" {
		// explorer exits non-zero even on success; fire and forget.
		cmd := exec.Command("explorer.exe", "/select,", filepath.Clean(path))
		procutil.Hidden(cmd)
		cmd.Start()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleEvents pushes a full task snapshot every 500ms over SSE.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() bool {
		// Relay any queued New Download prompts (from the extension) as a named
		// SSE event so the single-window UI can pop the in-page dialog.
		if prompts := s.drainPrompts(); len(prompts) > 0 {
			if pb, err := json.Marshal(prompts); err == nil {
				if _, err := fmt.Fprintf(w, "event: prompt\ndata: %s\n\n", pb); err != nil {
					return false
				}
			}
		}
		b, err := json.Marshal(map[string]any{"tasks": s.eng.List()})
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeEngineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, engine.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
