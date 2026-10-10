package engine

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"myidm/internal/config"
	"myidm/internal/store"
	"myidm/internal/ytdlp"
)

// newRunningEngine builds a real engine over temp folders and starts it.
func newRunningEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.DownloadDir = filepath.Join(dir, "dl")
	cfg.Categories = config.DefaultCategories(cfg.DownloadDir)
	cfg.MaxConcurrent = 4
	cfg.GUI = false
	st, err := store.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	e := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.Shutdown(3 * time.Second)
		cancel()
	})
	return e
}

// waitStatus polls until the task reaches one of the wanted states.
func waitStatus(t *testing.T, e *Engine, id string, want ...Status) TaskView {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		v, err := e.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if v.Status == w {
				return v
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	v, _ := e.Get(id)
	t.Fatalf("task %s stuck in %s (want %v): err=%q note=%q", id, v.Status, want, v.Error, v.Note)
	return v
}

func mustComplete(t *testing.T, e *Engine, id string) TaskView {
	t.Helper()
	v := waitStatus(t, e, id, StatusCompleted, StatusFailed)
	if v.Status != StatusCompleted {
		t.Fatalf("download failed: %s", v.Error)
	}
	return v
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tsSegment makes fake MPEG-TS bytes (sync byte every 188) unique to i.
func tsSegment(i, packets int) []byte {
	b := make([]byte, 188*packets)
	for p := 0; p < packets; p++ {
		b[p*188] = 0x47
		for k := 1; k < 188; k++ {
			b[p*188+k] = byte(i*31 + p*7 + k)
		}
	}
	return b
}

func encryptCBC(plain, key, iv []byte) []byte {
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	in := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(in))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, in)
	return out
}

// TestHLSMasterEncrypted downloads a master playlist → best variant → AES-128
// segments (explicit and sequence-derived IVs) from a server that insists on
// the page Referer, and checks the joined output byte for byte and its name.
func TestHLSMasterEncrypted(t *testing.T) {
	key := []byte("0123456789abcdef")
	explicitIV := []byte("fedcba9876543210")
	const n = 6
	var plain [][]byte
	for i := 0; i < n; i++ {
		plain = append(plain, tsSegment(i, 20+i))
	}
	const referer = "https://site.example/watch/cool-clip"
	var lowHits atomic.Int32
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Referer") != referer {
				http.Error(w, "hotlink", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/live/master.m3u8", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=300000,RESOLUTION=640x360\nlow/index.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=1280x720,CODECS=\"avc1.4d401f,mp4a.40.2\"\nhigh/index.m3u8\n")
	}))
	mux.HandleFunc("/live/low/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		lowHits.Add(1)
		http.Error(w, "should pick the best variant", http.StatusTeapot)
	})
	mux.HandleFunc("/live/high/index.m3u8", guard(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:100\n")
		b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"/keys/k1\",IV=0x" + fmt.Sprintf("%x", explicitIV) + "\n")
		for i := 0; i < n; i++ {
			if i == 3 { // switch to sequence-number IVs half way
				b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"/keys/k1\"\n")
			}
			fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts?tok=abc\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	}))
	mux.HandleFunc("/keys/k1", guard(func(w http.ResponseWriter, r *http.Request) { w.Write(key) }))
	mux.HandleFunc("/live/high/", guard(func(w http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/live/high/seg"), ".ts"))
		if err != nil || i < 0 || i >= n {
			http.NotFound(w, r)
			return
		}
		iv := explicitIV
		if i >= 3 {
			iv = seqIV(int64(100 + i))
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(encryptCBC(plain[i], key, iv))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/live/master.m3u8", AddOptions{Referer: referer})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	want := bytes.Join(plain, nil)
	if got := readFile(t, v.FilePath); !bytes.Equal(got, want) {
		t.Fatalf("joined stream differs: got %d bytes, want %d", len(got), len(want))
	}
	if base := filepath.Base(v.FilePath); base != "cool-clip.ts" {
		t.Errorf("file name = %q, want cool-clip.ts (named from the page, not index.m3u8)", base)
	}
	if lowHits.Load() != 0 {
		t.Error("fetched the low-quality variant")
	}
	if v.Kind != "hls" || v.SizeEst {
		t.Errorf("kind=%q sizeEstimated=%v", v.Kind, v.SizeEst)
	}
	if _, err := os.Stat(e.hlsWorkDir(&Task{FileName: "cool-clip.mp4", ID: v.ID, Dir: v.Dir})); !os.IsNotExist(err) {
		t.Error("segment work folder was not cleaned up")
	}
}

// TestHLSByteRangeFMP4 covers EXT-X-MAP init sections and EXT-X-BYTERANGE
// (implicit offsets) into a single resource.
func TestHLSByteRangeFMP4(t *testing.T) {
	init := []byte("\x00\x00\x00\x18ftypiso5\x00\x00\x00\x01iso5dash")
	body := bytes.Repeat([]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99}, 3000) // 30000 bytes of binary "fragments" (a text body is refused as a web page)
	mux := http.NewServeMux()
	mux.HandleFunc("/v.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MAP:URI=\"init.mp4\"\n"+
			"#EXTINF:2,\n#EXT-X-BYTERANGE:10000@0\nmedia.mp4\n"+
			"#EXTINF:2,\n#EXT-X-BYTERANGE:12000\nmedia.mp4\n"+
			"#EXTINF:2,\n#EXT-X-BYTERANGE:8000\nmedia.mp4\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) { w.Write(init) })
	mux.HandleFunc("/media.mp4", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "media.mp4", time.Time{}, bytes.NewReader(body))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/v.m3u8", AddOptions{Title: "My Show S01E02"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	want := append(append([]byte{}, init...), body...)
	if got := readFile(t, v.FilePath); !bytes.Equal(got, want) {
		t.Fatalf("fMP4 output differs: got %d bytes want %d", len(got), len(want))
	}
	if base := filepath.Base(v.FilePath); base != "My Show S01E02.mp4" {
		t.Errorf("name = %q", base)
	}
}

// TestHLSLiveRecordAndStop records a growing live playlist and saves the
// capture when the user presses Pause ("Stop").
func TestHLSLiveRecordAndStop(t *testing.T) {
	start := time.Now()
	var mu sync.Mutex
	served := map[int]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		// A new 1s segment appears every 150ms; the window holds the last 3.
		head := int(time.Since(start)/(150*time.Millisecond)) + 3
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", head-3)
		for i := head - 3; i < head; i++ {
			fmt.Fprintf(&b, "#EXTINF:1.0,\ns/%d.ts\n", i)
		}
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/s/", func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/s/"), ".ts"))
		mu.Lock()
		served[i] = true
		mu.Unlock()
		w.Write(tsSegment(i, 4))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/live.m3u8", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Record until a few segments have landed, then stop.
	deadline := time.Now().Add(15 * time.Second)
	for {
		cur, _ := e.Get(v.ID)
		if cur.Live && cur.Downloaded >= int64(3*4*188) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live recording never progressed: %+v", cur)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err := e.Pause(v.ID); err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	got := readFile(t, v.FilePath)
	if len(got) == 0 || len(got)%(4*188) != 0 || got[0] != 0x47 {
		t.Fatalf("recording is not a whole number of segments: %d bytes", len(got))
	}
	if v.Live {
		t.Error("completed recording still flagged live")
	}
}

// TestProbeRefererFallback: a hotlink-protected file that only answers
// same-site requests downloads without the caller supplying a Referer.
func TestProbeRefererFallback(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 200_000) // 1.6 MB -> several segments
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Referer"), srvURL) {
			http.Error(w, "no hotlinking", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/get?id=42", AddOptions{Title: "Project Files"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	if !bytes.Equal(readFile(t, v.FilePath), data) {
		t.Fatal("content mismatch")
	}
	if base := filepath.Base(v.FilePath); base != "Project Files.zip" {
		t.Errorf("name = %q, want \"Project Files.zip\"", base)
	}
	if v.Referer == "" {
		t.Error("the working Referer wasn't recorded for the segment requests")
	}
}

// TestWeakETagResumes: segments must not send a weak ETag in If-Range (a
// spec-following server then returns 200 + the whole file, which used to fail
// every multi-connection download from such servers).
func TestWeakETagResumes(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 400_000)
	mux := http.NewServeMux()
	mux.HandleFunc("/f.bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"v1"`)
		if ir := r.Header.Get("If-Range"); ir != "" && strings.HasPrefix(ir, "W/") {
			r.Header.Del("Range") // weak validators never match If-Range
		}
		http.ServeContent(w, r, "f.bin", time.Time{}, bytes.NewReader(data))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/f.bin", AddOptions{Segments: 6})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	if len(v.Segments) < 2 {
		t.Fatalf("expected a multi-connection download, got %d segments", len(v.Segments))
	}
	if !bytes.Equal(readFile(t, v.FilePath), data) {
		t.Fatal("content mismatch")
	}
}

// TestUnknownSizeRanged: a server that honors ranges but never states the total
// ("Content-Range: bytes 0-4095/*", chunked bodies) used to "complete" with an
// empty file.
func TestUnknownSizeRanged(t *testing.T) {
	data := bytes.Repeat([]byte("xyz"), 100_000)
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK) // no length on HEAD either
			return
		}
		start, end := int64(0), int64(len(data)-1)
		if rg := r.Header.Get("Range"); rg != "" {
			spec := strings.TrimPrefix(rg, "bytes=")
			a, b, _ := strings.Cut(spec, "-")
			start, _ = strconv.ParseInt(a, 10, 64)
			if b != "" {
				end, _ = strconv.ParseInt(b, 10, 64)
			}
			if end >= int64(len(data)) {
				end = int64(len(data) - 1)
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, end))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.(http.Flusher).Flush() // force chunked: no Content-Length
		w.Write(data[start : end+1])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/stream", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	if got := readFile(t, v.FilePath); !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want %d", len(got), len(data))
	}
	if v.Size != int64(len(data)) {
		t.Errorf("size = %d, want learned %d", v.Size, len(data))
	}
}

// TestNameFromRedirectAndSniff: a hash-named CDN redirect target loses to the
// original link's real name; a bare name gains its extension from the bytes.
func TestNameFromRedirectAndSniff(t *testing.T) {
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("q"), 5000)...)
	mux := http.NewServeMux()
	mux.HandleFunc("/files/Annual-Report-2025.pdf", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/cdn/9f86d081884c7d659a2feaa0c55ad015", http.StatusFound)
	})
	mux.HandleFunc("/cdn/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(pdf))
	})
	mux.HandleFunc("/dl/manual", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(pdf))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	for path, want := range map[string]string{
		"/files/Annual-Report-2025.pdf": "Annual-Report-2025.pdf",
		"/dl/manual":                    "manual.pdf",
	} {
		v, err := e.AddWithOptions(srv.URL+path, AddOptions{})
		if err != nil {
			t.Fatal(err)
		}
		v = mustComplete(t, e, v.ID)
		if base := filepath.Base(v.FilePath); base != want {
			t.Errorf("%s: name = %q, want %q", path, base, want)
		}
	}
}

// TestWebPageNeedsExtractor: a pasted web page is recognized as a page (not
// saved as a meaningless HTML "file"); without yt-dlp the error says why.
func TestWebPageNeedsExtractor(t *testing.T) {
	if ytdlp.Available() {
		t.Skip("yt-dlp is installed; this checks the no-extractor message")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/watch/clip-123", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!DOCTYPE html><html><head><title>Clip</title></head><body><video src=x.mp4></video></body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/watch/clip-123", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e, v.ID, StatusCompleted, StatusFailed)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "web page") {
		t.Fatalf("status=%s err=%q; want a clear web-page error", v.Status, v.Error)
	}
}

// TestHLSPauseResume pauses a VOD stream mid-way and resumes it: segments
// fetched before the pause are kept, not re-downloaded, and the result is exact.
func TestHLSPauseResume(t *testing.T) {
	const n = 12
	var hits [n]atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-PLAYLIST-TYPE:VOD\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "#EXTINF:2,\n%d.ts\n", i)
		}
		fmt.Fprint(w, b.String()) // no ENDLIST: PLAYLIST-TYPE:VOD alone marks it finished
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
		if err != nil || i < 0 || i >= n {
			http.NotFound(w, r)
			return
		}
		hits[i].Add(1)
		time.Sleep(40 * time.Millisecond)
		w.Write(tsSegment(i, 8))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/vod.m3u8", AddOptions{Segments: 2, Title: "Lecture 1"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		cur, _ := e.Get(v.ID)
		if cur.Downloaded >= int64(3*8*188) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no progress before pause")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := e.Pause(v.ID); err != nil {
		t.Fatal(err)
	}
	p := waitStatus(t, e, v.ID, StatusPaused)
	if p.Size <= 0 || !p.SizeEst {
		t.Errorf("paused stream should show an estimated size, got size=%d est=%v", p.Size, p.SizeEst)
	}
	if err := e.Resume(v.ID); err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	var want []byte
	for i := 0; i < n; i++ {
		want = append(want, tsSegment(i, 8)...)
		if h := hits[i].Load(); h > 2 {
			t.Errorf("segment %d fetched %d times", i, h)
		}
	}
	if !bytes.Equal(readFile(t, v.FilePath), want) {
		t.Fatal("resumed stream differs")
	}
	refetched := 0
	for i := 0; i < n; i++ {
		if hits[i].Load() > 1 {
			refetched++
		}
	}
	if refetched > 2 { // at most the in-flight segments are fetched again
		t.Errorf("%d segments re-downloaded after resume", refetched)
	}
	if base := filepath.Base(v.FilePath); base != "Lecture 1.ts" {
		t.Errorf("name = %q", base)
	}
}
