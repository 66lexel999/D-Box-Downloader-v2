package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"myidm/internal/engine"
	"myidm/internal/ytdlp"
)

// stubYtdlp makes the server believe yt-dlp is installed and answers every
// probe with probe (counting calls) — tests never run the real tool.
func stubYtdlp(s *Server, probe func(string, ytdlp.Opts) (*ytdlp.ProbeResult, error)) *int32 {
	var calls int32
	s.hasYtdlp = func() bool { return true }
	s.probes = newProbeCache(func(_ context.Context, u string, o ytdlp.Opts) (*ytdlp.ProbeResult, error) {
		atomic.AddInt32(&calls, 1)
		return probe(u, o)
	})
	return &calls
}

func grabFixture(t *testing.T) *httptest.Server {
	t.Helper()
	zip := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{1}, 2044)...)
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Files</title></head><body><a href="/f/pack.zip">pack</a></body></html>`))
	})
	mux.HandleFunc("/f/pack.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(zip))
	})
	mux.HandleFunc("/f/get", func(w http.ResponseWriter, r *http.Request) { // a file behind an extension-less URL
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="pack.zip"`)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(zip))
	})
	mux.HandleFunc("/guarded", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden) // a Cloudflare/hotlink refusal
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func pollGrab(t *testing.T, h http.Handler, id string) engine.GrabSnapshot {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var snap engine.GrabSnapshot
		if code := call(t, h, "GET", "/api/grab/"+id, nil, &snap); code != http.StatusOK {
			t.Fatalf("GET /api/grab/%s: %d", id, code)
		}
		if snap.Status == engine.GrabDone || snap.Status == engine.GrabError {
			return snap
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("grab job did not finish")
	return engine.GrabSnapshot{}
}

func TestGrabEndpoints(t *testing.T) {
	s, _, h := newTestServer(t)
	calls := stubYtdlp(s, func(string, ytdlp.Opts) (*ytdlp.ProbeResult, error) {
		return &ytdlp.ProbeResult{Title: "Files"}, nil // no video here
	})
	srv := grabFixture(t)

	var start engine.GrabSnapshot
	if code := call(t, h, "POST", "/api/grab", map[string]any{"url": srv.URL + "/page", "referer": "https://portal.example/"}, &start); code != http.StatusOK {
		t.Fatalf("POST /api/grab: %d", code)
	}
	if start.ID == "" || start.Mode != "page" {
		t.Fatalf("start snapshot: %+v", start)
	}
	snap := pollGrab(t, h, start.ID)
	if snap.Status != engine.GrabDone || snap.Direct || snap.PageTitle != "Files" {
		t.Fatalf("snapshot: %+v", snap)
	}
	if len(snap.Items) != 1 || snap.Items[0].Name != "pack.zip" || snap.Items[0].Size != 2048 ||
		snap.Items[0].Category != "Compressed" || snap.Items[0].FoundOn != srv.URL+"/page" {
		t.Errorf("items: %+v", snap.Items)
	}
	if atomic.LoadInt32(calls) != 1 {
		t.Errorf("yt-dlp probe calls = %d, want 1 (through the shared cache)", *calls)
	}
	// The raw JSON uses the documented field names.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/grab/"+start.ID, nil))
	for _, k := range []string{`"items":[`, `"counts":{`, `"foundOn":`, `"status":"done"`, `"direct":false`} {
		if !strings.Contains(rec.Body.String(), k) {
			t.Errorf("GET /api/grab JSON lacks %s: %s", k, rec.Body.String())
		}
	}

	if code := call(t, h, "DELETE", "/api/grab/"+start.ID, nil, nil); code != http.StatusOK {
		t.Errorf("DELETE: %d", code)
	}
	if code := call(t, h, "GET", "/api/grab/"+start.ID, nil, nil); code != http.StatusNotFound {
		t.Errorf("GET after DELETE: %d", code)
	}
	if code := call(t, h, "DELETE", "/api/grab/nope", nil, nil); code != http.StatusNotFound {
		t.Errorf("DELETE unknown: %d", code)
	}
	if code := call(t, h, "POST", "/api/grab", map[string]any{}, nil); code != http.StatusBadRequest {
		t.Errorf("empty POST: %d", code)
	}

	// Text mode: links pasted as text, probed without a crawl.
	var txt engine.GrabSnapshot
	call(t, h, "POST", "/api/grab", map[string]any{"text": "get " + srv.URL + "/f/pack.zip now"}, &txt)
	txt = pollGrab(t, h, txt.ID)
	if txt.Mode != "text" || len(txt.Items) != 1 || txt.Items[0].Size != 2048 {
		t.Errorf("text mode: %+v", txt)
	}
	// A direct file link is reported as such (the UI opens New Download).
	var direct engine.GrabSnapshot
	call(t, h, "POST", "/api/grab", map[string]any{"url": srv.URL + "/f/pack.zip"}, &direct)
	if direct = pollGrab(t, h, direct.ID); !direct.Direct {
		t.Errorf("direct link: %+v", direct)
	}
}

func TestGrabShowQueuesPrompt(t *testing.T) {
	s, _, h := newTestServer(t)
	body := map[string]any{"url": "https://site.example/watch/1", "title": "Episode 1",
		"referer": "https://site.example/", "cookies": "sid=abc"}
	if code := call(t, h, "POST", "/api/grab/show", body, nil); code != http.StatusOK {
		t.Fatalf("POST /api/grab/show: %d", code)
	}
	var polled struct {
		Prompts []string `json:"prompts"`
	}
	call(t, h, "GET", "/api/prompts", nil, &polled)
	if len(polled.Prompts) != 1 {
		t.Fatalf("prompts: %v", polled.Prompts)
	}
	q, _ := url.ParseQuery(polled.Prompts[0])
	if q.Get("grab") != "1" || q.Get("url") != "https://site.example/watch/1" || q.Get("title") != "Episode 1" {
		t.Errorf("prompt: %v", q)
	}
	if strings.Contains(polled.Prompts[0], "sid=abc") {
		t.Error("cookies must travel as a ctx token, not in the prompt")
	}
	rc, ok := s.ctxs.get(q.Get("ctx"))
	if !ok || rc.referer != "https://site.example/" || rc.headers["Cookie"] != "sid=abc" {
		t.Errorf("ctx token: %+v %v", rc, ok)
	}
	if code := call(t, h, "POST", "/api/grab/show", map[string]any{}, nil); code != http.StatusBadRequest {
		t.Errorf("missing url: %d", code)
	}
}

// TestInspectRefusedAsksYtdlp: a page that refuses our plain request (403)
// still gets its videos from yt-dlp, with every entry and the recommended one.
func TestInspectRefusedAsksYtdlp(t *testing.T) {
	s, _, h := newTestServer(t)
	stubYtdlp(s, func(u string, _ ytdlp.Opts) (*ytdlp.ProbeResult, error) {
		return &ytdlp.ProbeResult{Title: "Episode", HasVideo: true, Recommended: 2,
			Options: []ytdlp.DownOption{{Label: "720p", Selector: "22", Ext: "mp4", Size: 5 << 20}},
			Entries: []ytdlp.ProbeEntry{{Index: 1, Title: "Ad"}, {Index: 2, Title: "Episode"}}}, nil
	})
	srv := grabFixture(t)
	var out struct {
		Kind        string             `json:"kind"`
		Error       string             `json:"error"`
		ProbeError  string             `json:"probeError"`
		Video       *ytdlp.ProbeResult `json:"video"`
		Entries     []ytdlp.ProbeEntry `json:"entries"`
		Recommended int                `json:"recommended"`
	}
	call(t, h, "GET", "/api/inspect?url="+url.QueryEscape(srv.URL+"/guarded"), nil, &out)
	if out.Error != "" || !strings.Contains(out.ProbeError, "403") || out.Kind != "page" || out.Video == nil ||
		len(out.Entries) != 2 || out.Recommended != 2 {
		t.Errorf("inspect of a refused page: %+v", out)
	}

	// Without yt-dlp's help the refusal is reported as before.
	stubYtdlp(s, func(string, ytdlp.Opts) (*ytdlp.ProbeResult, error) {
		return nil, ytdlp.ErrNotInstalled
	})
	out.ProbeError, out.Error, out.Video = "", "", nil
	call(t, h, "GET", "/api/inspect?url="+url.QueryEscape(srv.URL+"/guarded"), nil, &out)
	if !strings.Contains(out.Error, "403") || out.Video != nil {
		t.Errorf("refused, no video: %+v", out)
	}
}

func TestInspectFileSkipsYtdlpWait(t *testing.T) {
	s, _, h := newTestServer(t)
	block := make(chan struct{})
	defer close(block)
	stubYtdlp(s, func(string, ytdlp.Opts) (*ytdlp.ProbeResult, error) {
		<-block // a slow yt-dlp must not hold up a plain file's answer
		return nil, ytdlp.ErrNotInstalled
	})
	srv := grabFixture(t)
	start := time.Now()
	var out struct {
		Kind     string `json:"kind"`
		FileName string `json:"fileName"`
		Size     int64  `json:"size"`
	}
	call(t, h, "GET", "/api/inspect?url="+url.QueryEscape(srv.URL+"/f/get"), nil, &out)
	if out.Kind != "file" || out.FileName != "pack.zip" || out.Size != 2048 || time.Since(start) > 5*time.Second {
		t.Errorf("file inspect: %+v in %v", out, time.Since(start))
	}
}

func TestProbeKey(t *testing.T) {
	o := ytdlp.Opts{UserAgent: "UA", Referer: "https://a.example/"}
	a := probeKey("https://Site.Example:443/watch?v=1#t=5", o)
	if b := probeKey("https://site.example/watch?v=1", o); a != b {
		t.Errorf("spellings of one URL differ:\n%q\n%q", a, b)
	}
	withCookie := o
	withCookie.Headers = map[string]string{"Cookie": "sid=1"}
	if probeKey("https://site.example/watch?v=1", withCookie) == a {
		t.Error("cookies must change the key")
	}
	otherUA := o
	otherUA.UserAgent = "UA2"
	if probeKey("https://site.example/watch?v=1", otherUA) == a {
		t.Error("the User-Agent must change the key")
	}
	if probeKey("https://site.example/watch?v=2", o) == a {
		t.Error("different videos share a key")
	}
}

func TestProbeCacheEmptyResultTTL(t *testing.T) {
	e := &probeEntry{done: make(chan struct{}), res: &ytdlp.ProbeResult{Title: "x"}, at: time.Now().Add(-2 * time.Minute)}
	close(e.done)
	if e.fresh(10*time.Minute, 15*time.Second) {
		t.Error("an empty result should expire after a minute")
	}
	e.res.Options = []ytdlp.DownOption{{Selector: "best"}}
	if !e.fresh(10*time.Minute, 15*time.Second) {
		t.Error("a result with formats keeps the long TTL")
	}
}

func TestVideoAcceptsPlaylistItem(t *testing.T) {
	var req videoRequest
	if err := json.Unmarshal([]byte(`{"url":"https://x.example/p","playlistItem":3,"referer":"https://x.example/"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.PlaylistItem != 3 || req.Referer != "https://x.example/" {
		t.Errorf("decoded: %+v", req)
	}
	// (Queuing itself needs yt-dlp installed; engine tests cover the task side.)
}
