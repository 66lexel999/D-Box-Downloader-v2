package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"myidm/internal/ytdlp"
)

// grabTestSite is a small web site with the things a grabber must find (and
// skip): a <video>, og:video, JSON-LD, a zip link, a download endpoint with
// Content-Disposition, a "Download" button to a landing page, an iframe with
// its own video, a YouTube embed, a tracking pixel, a tiny and a big image.
func grabTestSite(t *testing.T) *httptest.Server {
	t.Helper()
	body := func(magic string, n int) []byte {
		b := bytes.Repeat([]byte{0x11}, n)
		copy(b, magic)
		return b
	}
	files := map[string]struct {
		ct, cd string
		data   []byte
	}{
		"/media/ep.mp4":          {"video/mp4", "", body("\x00\x00\x00\x18ftypmp42", 50000)},
		"/media/bonus-scene.mp4": {"video/mp4", "", body("\x00\x00\x00\x18ftypmp42", 200000)},
		"/media/frame.webm":      {"video/webm", "", body("\x1a\x45\xdf\xa3", 30000)},
		"/files/pack.zip":        {"application/zip", "", body("PK\x03\x04", 3000)},
		"/files/real.7z":         {"application/x-7z-compressed", "", body("7z\xbc\xaf\x27\x1c", 7000)},
		"/download/report":       {"application/pdf", `attachment; filename="report.pdf"`, body("%PDF-1.4\n", 4000)},
		"/img/thumb.png":         {"image/png", "", body("\x89PNG\r\n\x1a\n", 100)},
		"/img/big.jpg":           {"image/jpeg", "", body("\xff\xd8\xff\xe0", 20000)},
	}
	pages := map[string]string{
		"/page": `<html><head><title>Show Page - ExampleTV</title>
<meta property="og:video" content="/media/ep.mp4">
<script type="application/ld+json">{"@type":"VideoObject","name":"Main Episode","contentUrl":"/media/bonus-scene.mp4"}</script>
</head><body>
<video src="/media/ep.mp4"></video>
<a href="/files/pack.zip">Pack</a>
<a href="/download/report">Get the report</a>
<a class="download-button" href="/download/landing">Download</a>
<a href="/about">About us</a>
<iframe src="/frame"></iframe>
<iframe src="https://www.youtube.com/embed/dQw4w9WgXcQ"></iframe>
<img src="/pixel/t.gif" width="1" height="1">
<img src="/img/thumb.png"><img src="/img/big.jpg">
<img src="/img/missing.jpg">
</body></html>`,
		"/frame":            `<html><body><video><source src="/media/frame.webm" type="video/webm"></video></body></html>`,
		"/download/landing": `<html><body><img src="/img/landing-art.jpg"><a href="/files/real.7z">real.7z</a></body></html>`,
		"/about":            `<html><body>About</body></html>`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if p, ok := pages[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(p))
			return
		}
		if f, ok := files[r.URL.Path]; ok {
			w.Header().Set("Content-Type", f.ct)
			if f.cd != "" {
				w.Header().Set("Content-Disposition", f.cd)
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.data))
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeVideo stands in for yt-dlp: the page is one video (+ audio), the
// YouTube embed another; everything else has no video.
func fakeVideo(calls *int32) func(context.Context, string, ytdlp.Opts) (*ytdlp.ProbeResult, error) {
	return func(_ context.Context, u string, _ ytdlp.Opts) (*ytdlp.ProbeResult, error) {
		atomic.AddInt32(calls, 1)
		switch {
		case strings.HasSuffix(u, "/page"):
			return &ytdlp.ProbeResult{Title: "Main Episode", HasVideo: true, Options: []ytdlp.DownOption{
				{Label: "MP4 - 720p", Selector: "bv*[height<=720]+ba", Ext: "mp4", Size: 900000},
				{Label: "MP3 - audio", Selector: "ba", Ext: "mp3", Audio: true, Size: 90000},
			}}, nil
		case strings.Contains(u, "youtube.com/embed/"):
			// hasVideo left false: a video option must still make a video row
			return &ytdlp.ProbeResult{Title: "Embedded Clip", Options: []ytdlp.DownOption{{Label: "Best", Selector: "bv*+ba", Ext: "mp4"}}}, nil
		}
		return nil, errors.New("Unsupported URL")
	}
}

func waitGrab(t *testing.T, g *Grabber, id string) GrabSnapshot {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s, ok := g.Get(id)
		if !ok {
			t.Fatalf("job %s vanished", id)
		}
		if s.Status == GrabDone || s.Status == GrabError {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return GrabSnapshot{}
}

func itemByURL(s GrabSnapshot, suffix string) *GrabItem {
	for i := range s.Items {
		if strings.HasSuffix(s.Items[i].URL, suffix) {
			return &s.Items[i]
		}
	}
	return nil
}

func TestGrabPage(t *testing.T) {
	srv := grabTestSite(t)
	var calls int32
	g := NewGrabber(testEngine())
	id, err := g.Start(GrabRequest{URL: srv.URL + "/page", ProbeVideo: fakeVideo(&calls)})
	if err != nil {
		t.Fatal(err)
	}
	s := waitGrab(t, g, id)
	defer func() {
		if t.Failed() {
			for _, it := range s.Items {
				t.Logf("  %-3s %-8s %-7s %-12s %8d %-26q small=%v rec=%v %s (on %s) %s", it.ID, it.Kind, it.Status, it.Source, it.Size, it.Name, it.Small, it.Recommended, it.URL, it.FoundOn, it.Error)
			}
		}
	}()
	if s.Status != GrabDone || s.Direct || s.Mode != "page" {
		t.Fatalf("status=%s direct=%v mode=%s err=%s", s.Status, s.Direct, s.Mode, s.Error)
	}
	if s.PageTitle != "Show Page - ExampleTV" || s.Title == "" {
		t.Errorf("titles: %q / %q", s.PageTitle, s.Title)
	}
	if s.Ytdlp != "done" || s.Pending != 0 {
		t.Errorf("ytdlp=%q pending=%d", s.Ytdlp, s.Pending)
	}

	type want struct {
		suffix, name, kind, category string
		size                         int64
		foundOn                      string
	}
	for _, w := range []want{
		{"/media/ep.mp4", "ep.mp4", "video", "Video", 50000, "/page"},
		{"/media/bonus-scene.mp4", "bonus-scene.mp4", "video", "Video", 200000, "/page"},
		{"/files/pack.zip", "pack.zip", "archive", "Compressed", 3000, "/page"},
		{"/download/report", "report.pdf", "document", "Documents", 4000, "/page"},
		{"/media/frame.webm", "frame.webm", "video", "Video", 30000, "/frame"},            // one iframe deep
		{"/files/real.7z", "real.7z", "archive", "Compressed", 7000, "/download/landing"}, // behind the Download button
		{"/img/big.jpg", "big.jpg", "image", "Images", 20000, "/page"},
		{"/img/thumb.png", "thumb.png", "image", "Images", 100, "/page"},
	} {
		it := itemByURL(s, w.suffix)
		if it == nil {
			t.Errorf("missing %s", w.suffix)
			continue
		}
		if it.Name != w.name || it.Kind != w.kind || it.Category != w.category || it.Size != w.size ||
			it.Status != "ok" || it.FoundOn != srv.URL+w.foundOn {
			t.Errorf("%s: got name=%q kind=%s cat=%s size=%d status=%s foundOn=%s", w.suffix, it.Name, it.Kind, it.Category, it.Size, it.Status, it.FoundOn)
		}
	}
	if it := itemByURL(s, "/img/thumb.png"); it != nil && !it.Small {
		t.Error("a 100-byte image should be marked small")
	}
	if it := itemByURL(s, "/img/big.jpg"); it != nil && it.Small {
		t.Error("a 20 KB image is not small")
	}
	if it := itemByURL(s, "/img/missing.jpg"); it == nil || it.Status != "error" {
		t.Errorf("a 404 stays listed as an error row: %+v", it)
	}
	for _, junk := range []string{"/about", "/pixel/t.gif", "/download/landing", "/frame", "/img/landing-art.jpg"} {
		if it := itemByURL(s, junk); it != nil {
			t.Errorf("should not be listed: %s (%s, %s)", junk, it.Kind, it.Source)
		}
	}

	// yt-dlp: the page's video (recommended) + its audio, and the embed.
	var video, audio, embed *GrabItem
	for i := range s.Items {
		it := &s.Items[i]
		if it.Source != "ytdlp" {
			continue
		}
		switch {
		case strings.Contains(it.URL, "youtube.com"):
			embed = it
		case it.Kind == "video":
			video = it
		case it.Kind == "audio":
			audio = it
		}
	}
	if video == nil || video.Name != "Main Episode.mp4" || !video.Recommended || video.Video == nil ||
		len(video.Video.Options) != 1 || video.Size != 900000 || !video.SizeEstimated || video.FoundOn != srv.URL+"/page" {
		t.Errorf("yt-dlp video item: %+v", video)
	}
	if audio == nil || audio.Name != "Main Episode.mp3" || audio.Category != "Music" || audio.Video == nil || !audio.Video.Options[0].Audio {
		t.Errorf("yt-dlp audio item: %+v", audio)
	}
	if embed == nil || embed.Name != "Embedded Clip.mp4" || embed.FoundOn != srv.URL+"/page" || embed.Size != -1 {
		t.Errorf("embed item: %+v", embed)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("yt-dlp ran %d times, want 2 (page + embed)", n)
	}
	if s.Counts["video"] < 4 || s.Counts["archive"] != 2 || s.Counts["document"] != 1 || s.Counts["small"] != 1 {
		t.Errorf("counts: %v", s.Counts)
	}
	ids := map[string]bool{}
	for _, it := range s.Items {
		if ids[it.ID] {
			t.Errorf("duplicate item id %s", it.ID)
		}
		ids[it.ID] = true
	}
}

func TestGrabDirect(t *testing.T) {
	srv := grabTestSite(t)
	var calls int32
	g := NewGrabber(testEngine())
	id, err := g.Start(GrabRequest{URL: srv.URL + "/files/pack.zip", ProbeVideo: fakeVideo(&calls)})
	if err != nil {
		t.Fatal(err)
	}
	s := waitGrab(t, g, id)
	if !s.Direct || s.Status != GrabDone || len(s.Items) != 1 {
		t.Fatalf("direct link: %+v", s)
	}
	if it := s.Items[0]; it.Name != "pack.zip" || it.Size != 3000 || it.Source != "direct" || it.Kind != "archive" {
		t.Errorf("direct item: %+v", it)
	}
	if calls != 0 {
		t.Error("a direct file must not run yt-dlp")
	}
}

func TestGrabText(t *testing.T) {
	srv := grabTestSite(t)
	g := NewGrabber(testEngine())
	text := "grab these: " + srv.URL + "/files/pack.zip and <a href=\"" + srv.URL + "/media/ep.mp4\">ep</a> " + srv.URL + "/about"
	var calls int32
	id, err := g.Start(GrabRequest{Text: text, Referer: srv.URL + "/page", ProbeVideo: fakeVideo(&calls)})
	if err != nil {
		t.Fatal(err)
	}
	s := waitGrab(t, g, id)
	if s.Mode != "text" || s.Direct || len(s.Items) != 3 {
		t.Fatalf("text job: %+v", s)
	}
	if it := itemByURL(s, "/files/pack.zip"); it == nil || it.Size != 3000 || it.FoundOn != srv.URL+"/page" || it.Source != "text" {
		t.Errorf("pack.zip: %+v", it)
	}
	if it := itemByURL(s, "/about"); it == nil || it.Kind != "page" || it.Status != "ok" {
		t.Errorf("a pasted page link stays in text mode: %+v", it)
	}
	if _, err := g.Start(GrabRequest{Text: "nothing here"}); err == nil {
		t.Error("text without links should be refused")
	}
}

func TestGrabPageError(t *testing.T) {
	srv := grabTestSite(t)
	g := NewGrabber(testEngine())
	var calls int32
	id, _ := g.Start(GrabRequest{URL: srv.URL + "/nope", ProbeVideo: fakeVideo(&calls)})
	s := waitGrab(t, g, id)
	if s.Status != GrabError || s.Error == "" || len(s.Items) != 0 {
		t.Errorf("unreadable page: %+v", s)
	}
}

func TestGrabCancel(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	g := NewGrabber(testEngine())
	var calls int32
	id, _ := g.Start(GrabRequest{URL: srv.URL + "/slow", ProbeVideo: fakeVideo(&calls)})
	if !g.Cancel(id) {
		t.Fatal("cancel of a running job failed")
	}
	if _, ok := g.Get(id); ok {
		t.Error("a cancelled job must be forgotten")
	}
	if g.Cancel(id) {
		t.Error("second cancel should report unknown job")
	}
}

func TestGrabExpiry(t *testing.T) {
	g := NewGrabber(testEngine())
	old := &grabJob{doneAt: time.Now().Add(-grabJobTTL - time.Minute)}
	fresh := &grabJob{doneAt: time.Now()}
	running := &grabJob{}
	g.jobs = map[string]*grabJob{"old": old, "fresh": fresh, "run": running}
	g.pruneLocked()
	if _, ok := g.jobs["old"]; ok {
		t.Error("finished job older than the TTL should expire")
	}
	if len(g.jobs) != 2 {
		t.Errorf("jobs left: %d", len(g.jobs))
	}
}

// TestGrabVideoEntries: a page with several videos lists one row per entry
// (with its playlist index for --playlist-items) and preselects the one
// yt-dlp recommends — not the first ad clip.
func TestGrabVideoEntries(t *testing.T) {
	yes, no := true, false
	best := []GrabOption{{Label: "1080p", Selector: "bv*+ba", Ext: "mp4", Size: 400 << 20}}
	j := &grabJob{e: testEngine(), seen: map[string]bool{}, pageURL: "https://tv.example/ep/5"}
	p := grabProbe{
		Title:       "Episode 5",
		Options:     best,
		Recommended: 2,
		Entries: []grabEntry{
			{Index: 1, Title: "Sponsor clip", HasVideo: &yes, Size: 1 << 20, Options: []GrabOption{{Label: "360p", Selector: "18", Ext: "mp4", Size: 1 << 20}}},
			{Index: 2, Title: "Episode 5", HasVideo: &yes, Size: 400 << 20, Duration: 1400, Options: best},
			{Index: 3, Title: "Theme song", HasVideo: &no, Ext: "m4a", Options: []GrabOption{{Label: "audio", Selector: "ba", Ext: "m4a", Audio: true, Size: 3 << 20}}},
		},
	}
	j.addVideoResult(p, j.pageURL, j.pageURL)
	s := j.snapshot()
	if len(s.Items) != 3 {
		t.Fatalf("items: %+v", s.Items)
	}
	ep := s.Items[1]
	if !ep.Recommended || ep.PlaylistItem != 2 || ep.Video.PlaylistItem != 2 || ep.Name != "Episode 5.mp4" || ep.Duration != 1400 {
		t.Errorf("recommended entry: %+v", ep)
	}
	if s.Items[0].Recommended || s.Items[0].PlaylistItem != 1 || s.Items[0].Size != 1<<20 {
		t.Errorf("ad entry: %+v", s.Items[0])
	}
	if a := s.Items[2]; a.Kind != "audio" || a.Name != "Theme song.m4a" || a.Category != "Music" {
		t.Errorf("audio entry: %+v", a)
	}
}

func TestGrabRequestContext(t *testing.T) {
	j := &grabJob{pageURL: "https://www.site.example/watch/1",
		ri: reqInfo{Referer: "https://www.site.example/", Headers: map[string]string{"Cookie": "sid=1", "Origin": "https://www.site.example"}}}
	same := j.riFor("https://cdn.site.example/v.mp4", "https://www.site.example/watch/1")
	if same.Headers["Cookie"] != "sid=1" {
		t.Error("same-site request lost the page's cookies")
	}
	other := j.riFor("https://cdn.other.example/v.mp4", "https://www.site.example/watch/1")
	if other.Headers["Cookie"] != "" || other.Headers["Origin"] == "" || other.Referer != "https://www.site.example/watch/1" {
		t.Errorf("third-party request context: %+v", other)
	}
}

func TestGrabKindAndCategory(t *testing.T) {
	cases := []struct{ probe, ext, ct, hint, kind, cat string }{
		{"hls", "m3u8", "", "", "stream", "Video"},
		{"file", "mp3", "", "", "audio", "Music"},
		{"file", "", "video/mp4", "", "video", "Video"},
		{"file", "exe", "", "", "program", "Programs"},
		{"file", "srt", "", "", "subtitle", "General"},
		{"file", "bin", "application/octet-stream", "", "other", "General"},
		{"file", "", "", "image", "image", "Images"},
	}
	for _, c := range cases {
		k := grabKindOf(c.probe, c.ext, c.ct, c.hint)
		if k != c.kind || grabCategory(c.ext, k) != c.cat {
			t.Errorf("%+v: kind=%s cat=%s", c, k, grabCategory(c.ext, k))
		}
	}
	if archiveItemID("https://archive.org/details/Popeye_forPresident") != "Popeye_forPresident" || archiveItemID("https://archive.org/search?q=x") != "" {
		t.Error("archiveItemID")
	}
	if grabArchiveLength("01:02:03") != 3723 || grabArchiveLength("12.5") != 12.5 {
		t.Error("grabArchiveLength")
	}
}
