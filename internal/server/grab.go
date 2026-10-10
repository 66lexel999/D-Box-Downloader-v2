package server

// Link Grabber endpoints. The crawl itself lives in engine.Grabber; this file
// turns the UI's requests into jobs (with the browser context resolved the
// same way downloads resolve it) and relays "open the grabber for this page"
// from secondary windows and the extension to the main window.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"myidm/internal/engine"
	"myidm/internal/ytdlp"
)

// grabRequest is POST /api/grab's body: a page to crawl (url) or links pasted
// as text (text / urls), plus the optional browser context.
type grabRequest struct {
	URL   string   `json:"url,omitempty"`
	Text  string   `json:"text,omitempty"`
	URLs  []string `json:"urls,omitempty"`
	Title string   `json:"title,omitempty"` // the page's title, when the caller knows it
	browserContext
}

// handleGrabStart starts a grab job and answers with its first snapshot (the
// id is what GET/DELETE /api/grab/{id} take).
func (s *Server) handleGrabStart(w http.ResponseWriter, r *http.Request) {
	var req grabRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.URL) == "" && strings.TrimSpace(req.Text) == "" && len(req.URLs) == 0 {
		writeError(w, http.StatusBadRequest, "missing url or text")
		return
	}
	referer, headers := s.resolveContext(req.browserContext)
	id, err := s.grabs.Start(engine.GrabRequest{
		URL: req.URL, Text: req.Text, URLs: req.URLs, Title: req.Title,
		Referer: referer, Headers: headers,
		ProbeVideo: s.videoProbe(),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	snap, _ := s.grabs.Get(id)
	writeJSON(w, http.StatusOK, snap)
}

// videoProbe routes the grabber's yt-dlp runs through the shared probe cache,
// so the New Download dialog, /api/probe and the grabber never probe the same
// page twice. nil when yt-dlp is not installed.
func (s *Server) videoProbe() func(context.Context, string, ytdlp.Opts) (*ytdlp.ProbeResult, error) {
	if !s.hasYtdlp() {
		return nil
	}
	return s.probes.get
}

func (s *Server) handleGrabGet(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.grabs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "grab job not found (finished jobs expire after 10 minutes)")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleGrabCancel(w http.ResponseWriter, r *http.Request) {
	if !s.grabs.Cancel(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "grab job not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGrabShow asks the main window to open the Link Grabber for a page
// (add.html's "Show everything on this page", the extension). It rides the
// same prompt queue as New Download prompts; the page context travels as a
// short-lived ctx token, never in the query string.
func (s *Server) handleGrabShow(w http.ResponseWriter, r *http.Request) {
	var req grabRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	q := url.Values{}
	q.Set("grab", "1")
	q.Set("url", strings.TrimSpace(req.URL))
	if t := strings.TrimSpace(req.Title); t != "" {
		q.Set("title", t)
	}
	if referer, headers := s.resolveContext(req.browserContext); referer != "" || len(headers) > 0 {
		q.Set("ctx", s.ctxs.put(referer, headers))
	}
	s.queuePrompt(q.Encode())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
