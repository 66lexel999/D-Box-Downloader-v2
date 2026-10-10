package server

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"myidm/internal/engine"
)

// Auto download (New Download window → "Auto download"): while it is on, a
// download handed to D BOX skips the New Download window and starts at once —
// or waits in the queue while "max at once" downloads run. The state itself
// lives in the engine (engine/autodl.go); here it's served to the windows and
// applied to /api/prompt.

func (s *Server) handleGetAutoDownload(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.eng.AutoDownloadState())
}

// handleSetAutoDownload turns auto download on or off. Body {mode, n}: mode
// "forever", "minutes" (n of them) or "files" (the next n), or "off".
func (s *Server) handleSetAutoDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
		N    int    `json:"n"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	v, err := s.eng.SetAutoDownload(req.Mode, req.N)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// autoAdd starts a prompted download the way the New Download window's Start
// does with what it would pre-fill: the suggested name, the category folder
// that name (or the link) belongs to, and the page context.
func (s *Server) autoAdd(req promptRequest) (engine.TaskView, error) {
	referer, headers := s.resolveContext(req.browserContext)
	if req.Video {
		return s.eng.AddVideoWithOptions(req.URL, engine.VideoOptions{
			Title: req.Title, Selector: req.Selector, Ext: req.Ext, Audio: req.Audio,
			Dir: s.eng.CategoryDir("Video"), Referer: referer, Headers: headers,
		})
	}
	return s.eng.AddWithOptions(req.URL, engine.AddOptions{
		FileName: req.Name, Title: req.Title, Referer: referer, Headers: headers,
		Dir: s.eng.CategoryDir(autoCategory(req.Name, req.URL, req.ExpectedMime)),
	})
}

// autoCategory picks the category (download folder) the New Download window
// would pre-select: by the file name's extension, else the link's, else the
// type the browser expects. Streams are videos. Unknown is "General".
func autoCategory(name, rawURL, mimeType string) string {
	exts := []string{path.Ext(name)}
	if u, err := url.Parse(rawURL); err == nil {
		exts = append(exts, path.Ext(u.Path))
	}
	for _, e := range exts {
		e = strings.ToLower(strings.TrimPrefix(e, "."))
		if c := extCategory[e]; c != "" {
			return c
		}
		if e == "m3u8" || e == "mpd" {
			return "Video"
		}
	}
	mt, _, _ := mime.ParseMediaType(mimeType)
	switch {
	case strings.HasPrefix(mt, "video/"), strings.Contains(mt, "mpegurl"), strings.Contains(mt, "dash+xml"):
		return "Video"
	case strings.HasPrefix(mt, "audio/"):
		return "Music"
	case strings.HasPrefix(mt, "image/"):
		return "Images"
	}
	return "General"
}
