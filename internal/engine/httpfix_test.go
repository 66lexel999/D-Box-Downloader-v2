package engine

// Tests for the plain-file path's defenses against servers that don't send
// the file: soft refusals, landing pages, forced compression, wrong names,
// redirectors, bad ranges and one-time links.

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hfBinary returns n deterministic non-text bytes starting with magic.
func hfBinary(magic string, n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	copy(b, magic)
	return b
}

func hfServe(w http.ResponseWriter, r *http.Request, ct string, data []byte) {
	w.Header().Set("Content-Type", ct)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func hfFail(t *testing.T, e *Engine, id, want string) {
	t.Helper()
	v := waitStatus(t, e, id, StatusCompleted, StatusFailed)
	if v.Status != StatusFailed {
		t.Fatalf("completed as %q; want a failure mentioning %q", v.FilePath, want)
	}
	if !strings.Contains(v.Error, want) {
		t.Fatalf("error = %q, want it to mention %q", v.Error, want)
	}
}

// A file that only answers a same-site Referer, and otherwise redirects to the
// site's home page (dev-c.com): the probe must see through the 200 HTML and
// try the next Referer.
func TestSoftRefusalTriesNextReferer(t *testing.T) {
	data := hfBinary("PK\x03\x04", 300_000, 1)
	var origin string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!DOCTYPE html><!--[if lt IE 7]><html class=ie6><![endif]--><html><body>Welcome to the site</body></html>")
	})
	mux.HandleFunc("/files/ScriptHook_1.0.zip", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Referer"), origin) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		hfServe(w, r, "application/zip", data)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	origin = srv.URL

	e := newRunningEngine(t)
	res, err := e.Inspect(context.Background(), srv.URL+"/files/ScriptHook_1.0.zip", "", nil, "")
	if err != nil || res.Kind != kindFile || res.Size != int64(len(data)) {
		t.Fatalf("inspect = %+v, %v; want the zip's own size", res, err)
	}
	v, err := e.AddWithOptions(srv.URL+"/files/ScriptHook_1.0.zip", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	if !bytes.Equal(readFile(t, v.FilePath), data) || filepath.Base(v.FilePath) != "ScriptHook_1.0.zip" {
		t.Fatalf("got %s with wrong content", v.FilePath)
	}
}

// Landing pages: a countdown with <meta refresh>, and a page whose only
// link named like the file is the real one (foobar2000's /getfile → /files).
func TestLandingPageFollowsDownloadLink(t *testing.T) {
	exe := hfBinary("MZ", 200_000, 2)
	var landingReferer atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/vlc/tool-3.0-win64.exe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><meta http-equiv="refresh" content="5;URL='/mirror-a/pub/tool-3.0-win64.exe'"></head>
			<body>Your download will start shortly. <a href="/about">About</a></body></html>`)
	})
	mux.HandleFunc("/mirror-a/pub/tool-3.0-win64.exe", func(w http.ResponseWriter, r *http.Request) {
		landingReferer.Store(r.Header.Get("Referer"))
		hfServe(w, r, "application/octet-stream", exe)
	})
	mux.HandleFunc("/getfile/app_v2.1.exe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><body><nav><a href="/news">News</a><a href="/download">Download</a></nav>
			<p>Thank you!</p><a href="/files/app_v2.1.exe">click here</a><a href="/files/app_v2.0.exe">older</a></body></html>`)
	})
	mux.HandleFunc("/files/app_v2.1.exe", func(w http.ResponseWriter, r *http.Request) {
		hfServe(w, r, "application/octet-stream", exe)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	for _, c := range []struct{ link, want string }{
		{"/vlc/tool-3.0-win64.exe", "tool-3.0-win64.exe"},
		{"/getfile/app_v2.1.exe", "app_v2.1.exe"},
	} {
		v, err := e.AddWithOptions(srv.URL+c.link, AddOptions{})
		if err != nil {
			t.Fatal(err)
		}
		v = mustComplete(t, e, v.ID)
		if !bytes.Equal(readFile(t, v.FilePath), exe) || filepath.Base(v.FilePath) != c.want {
			t.Errorf("%s: got %s (content ok=%v)", c.link, v.FilePath, bytes.Equal(readFile(t, v.FilePath), exe))
		}
	}
	if ref, _ := landingReferer.Load().(string); ref != srv.URL+"/vlc/tool-3.0-win64.exe" {
		t.Errorf("mirror asked with Referer %q, want the landing page", ref)
	}
}

// A landing page with nothing to follow fails with a clear message instead of
// being saved as the .rar or handed to yt-dlp as a "video page".
func TestLandingPageWithoutLinkFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/mirror/Game.rar", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/mirror/wait.html?f=Game.rar", http.StatusFound)
	})
	mux.HandleFunc("/mirror/wait.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!DOCTYPE html><html><body>Please wait 10 seconds…</body></html>")
	})
	mux.HandleFunc("/api/get/pack.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, `{"status":"error","message":"token expired"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	if _, err := e.Inspect(context.Background(), srv.URL+"/mirror/Game.rar", "", nil, ""); err == nil ||
		!strings.Contains(err.Error(), "web page instead of the file") {
		t.Fatalf("inspect err = %v", err)
	}
	for _, link := range []string{"/mirror/Game.rar", "/api/get/pack.zip"} {
		v, err := e.AddWithOptions(srv.URL+link, AddOptions{})
		if err != nil {
			t.Fatal(err)
		}
		hfFail(t, e, v.ID, "web page instead of the file")
	}
}

// Google Drive's "can't scan this file" page: its GET form is the download.
func TestLandingTargetDriveForm(t *testing.T) {
	page := `<html><body><p>img.zip (1.3G) is too large for Google to scan for viruses.</p>
		<form id="download-form" action="https://drive.usercontent.google.com/download" method="get">
		<input type="submit" value="Download anyway"><input type="hidden" name="id" value="0B7EVK8r0v71p">
		<input type="hidden" name="export" value="download"><input type="hidden" name="confirm" value="t">
		<input type="hidden" name="uuid" value="1234"></form></body></html>`
	got := landingTarget("https://drive.usercontent.google.com/download?id=0B7EVK8r0v71p&export=download", []byte(page), expectation{})
	u, err := url.Parse(got)
	if err != nil || u.Host != "drive.usercontent.google.com" || u.Query().Get("confirm") != "t" || u.Query().Get("id") != "0B7EVK8r0v71p" {
		t.Fatalf("target = %q", got)
	}
	// A video page with a lone "Download" button to an MP4 stays a page for yt-dlp.
	video := `<html><body><video src="/v/clip.mp4"></video><a href="/v/clip.mp4">Download video</a></body></html>`
	if got := landingTarget("https://site.test/watch/1", []byte(video), expectation{}); got != "" {
		t.Errorf("video page target = %q, want none", got)
	}
}

// Servers that gzip everything despite "Accept-Encoding: identity": a page
// is still recognized as a page, a file is saved decoded, and a real .gz is
// saved as sent.
func TestForcedGzip(t *testing.T) {
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(b)
		zw.Close()
		return buf.Bytes()
	}
	csv := []byte(strings.Repeat("id,name\n1,alpha\n", 500))
	tarball := hfBinary("ustar", 50_000, 3)
	mux := http.NewServeMux()
	mux.HandleFunc("/gz/data.csv", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/csv")
		w.Write(gz(csv))
	})
	mux.HandleFunc("/gz/pack.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/zip")
		w.Write(gz([]byte("<html><body>Access denied</body></html>")))
	})
	mux.HandleFunc("/gz/src.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip") // the classic Apache misconfiguration
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(gz(tarball))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, _ := e.AddWithOptions(srv.URL+"/gz/data.csv", AddOptions{})
	if v = mustComplete(t, e, v.ID); !bytes.Equal(readFile(t, v.FilePath), csv) {
		t.Error("data.csv was saved compressed")
	}
	v, _ = e.AddWithOptions(srv.URL+"/gz/src.tar.gz", AddOptions{})
	if v = mustComplete(t, e, v.ID); !bytes.Equal(readFile(t, v.FilePath), gz(tarball)) {
		t.Error("a .tar.gz must be saved as sent")
	}
	v, _ = e.AddWithOptions(srv.URL+"/gz/pack.zip", AddOptions{})
	hfFail(t, e, v.ID, "web page instead of the file")
}

// MP3 bytes under a browser-suggested ".mp4" name are saved as .mp3; a
// picture where a video was promised is an error, not "clip.mp4".
func TestMagicBeatsWrongExtension(t *testing.T) {
	mp3 := hfBinary("ID3\x04\x00", 100_000, 4)
	png := hfBinary("\x89PNG\r\n\x1a\n", 20_000, 5)
	mux := http.NewServeMux()
	mux.HandleFunc("/dl/track_mp3_320.mp3", func(w http.ResponseWriter, r *http.Request) { hfServe(w, r, "audio/mpeg", mp3) })
	mux.HandleFunc("/v/clip.mp4", func(w http.ResponseWriter, r *http.Request) { hfServe(w, r, "image/png", png) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, _ := e.AddWithOptions(srv.URL+"/dl/track_mp3_320.mp3", AddOptions{FileName: "Fast goal video.mp4"})
	if v = mustComplete(t, e, v.ID); filepath.Base(v.FilePath) != "Fast goal video.mp3" {
		t.Errorf("name = %q, want \"Fast goal video.mp3\"", filepath.Base(v.FilePath))
	}
	v, _ = e.AddWithOptions(srv.URL+"/v/clip.mp4", AddOptions{})
	hfFail(t, e, v.ID, "image instead of the video")
}

func TestReconcileExt(t *testing.T) {
	for _, c := range []struct{ name, magic, want string }{
		{"a.mp4", "mp3", "a.mp3"},
		{"a.mp4", "mov", "a.mp4"},
		{"a.m4a", "mp4", "a.m4a"},
		{"a.docx", "zip", "a.docx"},
		{"a.apk", "zip", "a.apk"},
		{"a.mka", "mkv", "a.mka"},
		{"a.exe", "png", "a.png"},
		{"a.zip", "mp3", "a.zip"}, // frame sync is too weak against a non-media name
		{"a.txt", "pdf", "a.txt"},
		{"a.bin", "zip", "a.bin"},
		{"a.zip", "html", "a.zip"},
		{"a.webm", "mkv", "a.webm"},
	} {
		if got := reconcileExt(c.name, c.magic); got != c.want {
			t.Errorf("reconcileExt(%q, %q) = %q, want %q", c.name, c.magic, got, c.want)
		}
	}
	if got := fixUserExt("videoplayback", "x.mp4", "mp4"); got != "videoplayback.mp4" {
		t.Errorf("fixUserExt bare name = %q", got)
	}
}

// A random-image redirector without validators (picsum.photos/400/300): every
// segment must come from the one file the probe saw, not a new random pick.
func TestRedirectorWithoutValidators(t *testing.T) {
	imgs := [][]byte{hfBinary("\xff\xd8\xff\xe0", 3_000_000, 6), hfBinary("\xff\xd8\xff\xe0", 3_000_000, 7)}
	var n atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/400/300", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("/id/%d/400/300.jpg", n.Add(1)%2), http.StatusFound)
	})
	mux.HandleFunc("/id/", func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(strings.Split(r.URL.Path, "/")[2])
		hfServe(w, r, "image/jpeg", imgs[i])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, _ := e.AddWithOptions(srv.URL+"/400/300", AddOptions{Segments: 4})
	v = mustComplete(t, e, v.ID)
	got := readFile(t, v.FilePath)
	if !bytes.Equal(got, imgs[0]) && !bytes.Equal(got, imgs[1]) {
		t.Fatal("segments were stitched from different images")
	}
}

// A CDN that answers every range with "bytes 0-…" must not corrupt the file:
// the engine notices, restarts, and ends on a single plain stream.
func TestWrongContentRangeIsCaught(t *testing.T) {
	data := hfBinary("PK\x03\x04", 3_000_000, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/f/big.zip", func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" || strings.HasPrefix(rng, "bytes=0-") {
			hfServe(w, r, "application/zip", data)
			return
		}
		var start, end int64
		fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
		body := data[:end-start+1] // the wrong bytes, honestly labelled
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, _ := e.AddWithOptions(srv.URL+"/f/big.zip", AddOptions{Segments: 4})
	v = mustComplete(t, e, v.ID)
	if !bytes.Equal(readFile(t, v.FilePath), data) {
		t.Fatal("file corrupted by mislabelled ranges")
	}
}

func TestContentRangeAndRetryAfterParsing(t *testing.T) {
	if s, e, tot, ok := parseContentRange("bytes 100-199/1000"); !ok || s != 100 || e != 199 || tot != 1000 {
		t.Errorf("parseContentRange = %d %d %d %v", s, e, tot, ok)
	}
	if _, _, tot, ok := parseContentRange("bytes 0-9/*"); !ok || tot != -1 {
		t.Errorf("unknown total: %d %v", tot, ok)
	}
	for _, bad := range []string{"", "bytes */1000", "items 0-1/2", "bytes 5-1/10", "bytes 0-9/5"} {
		if _, _, _, ok := parseContentRange(bad); ok {
			t.Errorf("parseContentRange(%q) accepted", bad)
		}
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if d, ok := parseRetryAfter("7", now); !ok || d != 7*time.Second {
		t.Errorf("seconds: %v %v", d, ok)
	}
	if d, ok := parseRetryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now); !ok || d != 30*time.Second {
		t.Errorf("date: %v %v", d, ok)
	}
	if d, _ := parseRetryAfter("86400", now); d != maxRetryAfter {
		t.Errorf("cap: %v", d)
	}
}

// A 429 with Retry-After on a segment is waited out rather than burning
// through the retry budget with short backoffs.
func TestSegmentHonorsRetryAfter(t *testing.T) {
	data := hfBinary("PK\x03\x04", 100_000, 9)
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/f/a.zip", func(w http.ResponseWriter, r *http.Request) {
		// The probe (ranged, first request) passes; the first segment request
		// is told to come back in 2 s; later ones pass.
		if hits.Add(1) == 2 {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		hfServe(w, r, "application/zip", data)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	start := time.Now()
	v, _ := e.AddWithOptions(srv.URL+"/f/a.zip", AddOptions{Segments: 1})
	mustComplete(t, e, v.ID)
	if el := time.Since(start); el < 2*time.Second {
		t.Errorf("finished after %v; Retry-After: 2 was not honored", el)
	}
}

// Cookie-check redirects (rumble: 307 + Set-Cookie back to itself) resolve
// with the engine's cookie jar.
func TestCookieCheckRedirect(t *testing.T) {
	data := hfBinary("\x00\x00\x00\x18ftypmp42", 80_000, 10)
	mux := http.NewServeMux()
	mux.HandleFunc("/video/clip.mp4", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("check"); err != nil || c.Value != "ok" {
			http.SetCookie(w, &http.Cookie{Name: "check", Value: "ok", Path: "/"})
			http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
			return
		}
		hfServe(w, r, "video/mp4", data)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, _ := e.AddWithOptions(srv.URL+"/video/clip.mp4", AddOptions{})
	if v = mustComplete(t, e, v.ID); !bytes.Equal(readFile(t, v.FilePath), data) {
		t.Fatal("content mismatch")
	}
}

// Empty answers, and a one-time link whose later request gets an HTML page,
// fail instead of completing as junk under the file's name.
func TestFinalizeRejectsJunk(t *testing.T) {
	zip := hfBinary("PK\x03\x04", 5_000, 11)
	var once atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/empty/setup.exe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "0")
	})
	mux.HandleFunc("/once/big.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		if once.Swap(true) {
			w.Write([]byte("<html>This link was already used</html>"))
			return
		}
		w.Write(zip[:100]) // chunked: size unknown, so the download can't check it
		w.(http.Flusher).Flush()
		w.Write(zip[100:])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, _ := e.AddWithOptions(srv.URL+"/empty/setup.exe", AddOptions{})
	hfFail(t, e, v.ID, "empty file")
	v, _ = e.AddWithOptions(srv.URL+"/once/big.zip", AddOptions{})
	hfFail(t, e, v.ID, "web page instead of the file")
}

func TestParseDownloadURLNormalizesShareLinks(t *testing.T) {
	u, err := parseDownloadURL("https://www.dropbox.com/s/abc123/model.net?dl=0")
	if err != nil || u.Query().Get("dl") != "1" {
		t.Errorf("dropbox = %v, %v", u, err)
	}
	u, err = parseDownloadURL("https://drive.google.com/file/d/1AbCdEfGhIjKlMnOp/view?usp=sharing")
	if err != nil || u.Path != "/uc" || u.Query().Get("id") != "1AbCdEfGhIjKlMnOp" {
		t.Errorf("drive = %v, %v", u, err)
	}
}

// A server that never sends headers can't hold the New Download dialog past
// the probe's own time limits.
func TestProbeHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	e := newRunningEngine(t)
	start := time.Now()
	_, err := e.probeWith(context.Background(), srv.URL+"/slow.zip", reqInfo{},
		probeOpts{headerWait: 300 * time.Millisecond, budget: 2 * time.Second})
	if err == nil || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}
