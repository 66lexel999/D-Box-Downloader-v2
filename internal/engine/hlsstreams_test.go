package engine

// Stream fixes: variant choice, size estimates, segment validation (web pages,
// image-disguised TS, junk), gzip playlists, audio-only naming, Inspect for
// DASH / offline / DRM / AES-256, and the live-edge start.

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRankVariants(t *testing.T) {
	pl, err := parseHLS("#EXTM3U\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=2500000,CODECS=\"mp4a.40.2\"\naudio.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=900000,CODECS=\"avc1.64001e,mp4a.40.2\"\nvideo.m3u8\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := pickVariant(pl.Variants); v.URI != "video.m3u8" {
		t.Errorf("no RESOLUTION: picked %q, want the video variant over the higher-bitrate audio-only one", v.URI)
	}
	// Apple's advanced masters: the same picture with AAC, AC-3 and E-AC-3.
	pl, _ = parseHLS("#EXTM3U\n"+
		"#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=7900000,BANDWIDTH=8100000,CODECS=\"avc1.640028,ac-3\",RESOLUTION=1920x1080,AUDIO=\"a2\"\nv9/ac3.m3u8\n"+
		"#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=7700000,BANDWIDTH=7900000,CODECS=\"avc1.640028,mp4a.40.2\",RESOLUTION=1920x1080,AUDIO=\"a1\"\nv9/aac.m3u8\n"+
		"#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=7800000,BANDWIDTH=8000000,CODECS=\"avc1.640028,ec-3\",RESOLUTION=1920x1080,AUDIO=\"a3\"\nv9/ec3.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS=\"avc1.64001f,mp4a.40.2\",RESOLUTION=1280x720\nv5.m3u8\n", nil)
	v, _ := pickVariant(pl.Variants)
	if v.URI != "v9/aac.m3u8" {
		t.Errorf("picked %q, want the AAC 1080p variant", v.URI)
	}
	if v.AvgBandwidth != 7700000 || v.Bandwidth != 7900000 || v.sizeRate() != 7700000 {
		t.Errorf("bandwidths = %d/%d", v.AvgBandwidth, v.Bandwidth)
	}
	// A clearly better AC-3 variant (twice the bitrate) still wins.
	pl, _ = parseHLS("#EXTM3U\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"avc1.640028,ac-3\",RESOLUTION=1280x720\nhi.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS=\"avc1.640028,mp4a.40.2\",RESOLUTION=1280x720\nlo.m3u8\n", nil)
	if v, _ := pickVariant(pl.Variants); v.URI != "hi.m3u8" {
		t.Errorf("picked %q, want hi.m3u8", v.URI)
	}
	a := hlsVariant{Codecs: "mp4a.40.34"}
	if !a.audioOnly() || a.audioExt() != "mp3" {
		t.Errorf("mp3 variant: audioOnly=%v ext=%q", a.audioOnly(), a.audioExt())
	}
	if (hlsVariant{}).audioOnly() {
		t.Error("a variant without CODECS must not count as audio-only")
	}
}

func TestLiveEdgeStart(t *testing.T) {
	mk := func(n int, typ string) *hlsPlaylist {
		pl := &hlsPlaylist{PlaylistType: typ}
		for i := 0; i < n; i++ {
			pl.Segments = append(pl.Segments, hlsSegment{Seq: int64(i), Duration: 6})
		}
		return pl
	}
	if got := liveEdgeStart(mk(300, "")); got != 297 {
		t.Errorf("sliding window: start %d, want 297", got)
	}
	if got := liveEdgeStart(mk(300, "EVENT")); got != 0 {
		t.Errorf("EVENT: start %d, want 0", got)
	}
	if got := liveEdgeStart(mk(3, "")); got != 0 {
		t.Errorf("short window: start %d", got)
	}
}

func TestMediaPayloadAndDisguise(t *testing.T) {
	id3 := []byte("ID3\x04\x00\x00\x00\x00\x00\x05PRIV\x00")
	cases := map[string][]byte{
		"ts":  tsSegment(1, 3),
		"aac": append(append([]byte{}, id3...), 0xFF, 0xF1, 0x50, 0x80),
		"mp3": {0xFF, 0xFB, 0x90, 0x64, 0x00},
		"mp4": []byte("\x00\x00\x00\x18stypmsdh"),
		"":    []byte("\x80\x81\x82\x83 not media"),
	}
	for want, b := range cases {
		if got := mediaPayload(b); got != want {
			t.Errorf("mediaPayload(%q…) = %q, want %q", b[:min(len(b), 8)], got, want)
		}
	}
	ts := tsSegment(7, 5)
	for _, fake := range [][]byte{pngPrefix(), []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), []byte("GIF89a\x01\x00\x01\x00")} {
		got, err := stripDisguise(append(append([]byte{}, fake...), ts...))
		if err != nil || !bytes.Equal(got, ts) {
			t.Errorf("disguise %q not stripped (err %v, %d bytes)", fake[:4], err, len(got))
		}
	}
	if _, err := stripDisguise(pngPrefix()); err == nil {
		t.Error("a picture without video inside was accepted")
	}
	if got, _ := stripDisguise(ts); !bytes.Equal(got, ts) {
		t.Error("plain TS was changed")
	}
}

// pngPrefix is a fake picture header like the ones hosts put before TS data.
func pngPrefix() []byte {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")
	return append(b, bytes.Repeat([]byte{0x10, 0x20, 0x30}, 30)...)
}

// mediaServer serves a VOD media playlist of n segments named by segName,
// each answered by seg(i, r).
func mediaServer(t *testing.T, n int, segName string, seg func(i int, w http.ResponseWriter, r *http.Request)) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/m/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.0,\n"+segName+"\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/m/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/m/")
		digits := strings.TrimFunc(name, func(c rune) bool { return c < '0' || c > '9' })
		i, err := strconv.Atoi(digits)
		if err != nil || i < 0 || i >= n {
			http.NotFound(w, r)
			return
		}
		seg(i, w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHLSImageDisguisedSegments(t *testing.T) {
	const n = 4
	srv := mediaServer(t, n, "img%d.png", func(i int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(append(pngPrefix(), tsSegment(i, 10+i)...))
	})
	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/m/index.m3u8", AddOptions{Title: "Disguised"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	var want []byte
	for i := 0; i < n; i++ {
		want = append(want, tsSegment(i, 10+i)...)
	}
	if got := readFile(t, v.FilePath); !bytes.Equal(got, want) {
		t.Fatalf("output %d bytes, want the %d bytes of TS without the picture headers", len(got), len(want))
	}
}

// TestHLSExpiredTokenRefresh: a segment answers 200 with an HTML "token
// expired" page for the playlist's first token; a re-read playlist carries a
// fresh token that works.
func TestHLSExpiredTokenRefresh(t *testing.T) {
	const n = 5
	var issued atomic.Int32
	var mu sync.Mutex
	dead := map[int]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/r/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		tok := issued.Add(1)
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts?tok=%d\n", i, tok)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/r/", func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/r/seg"), ".ts"))
		tok := r.URL.Query().Get("tok")
		mu.Lock()
		if _, seen := dead[i]; !seen && i >= 2 {
			dead[i] = tok // the first token this segment sees has "expired"
		}
		expired := i >= 2 && dead[i] == tok
		mu.Unlock()
		if expired {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>403 token expired</body></html>")
			return
		}
		w.Write(tsSegment(i, 8))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/r/index.m3u8", AddOptions{Title: "Token clip"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	var want []byte
	for i := 0; i < n; i++ {
		want = append(want, tsSegment(i, 8)...)
	}
	if got := readFile(t, v.FilePath); !bytes.Equal(got, want) {
		t.Fatalf("output %d bytes, want %d", len(got), len(want))
	}
}

func TestHLSWebPageSegmentsFail(t *testing.T) {
	srv := mediaServer(t, 3, "s%d.ts", func(i int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>403 token expired</html>")
	})
	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/m/index.m3u8", AddOptions{Title: "Expired"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e, v.ID, StatusCompleted, StatusFailed)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "web page") {
		t.Fatalf("status %s err %q: HTML segments must fail clearly", v.Status, v.Error)
	}
}

func TestHLSJunkSegmentsFail(t *testing.T) {
	junk := make([]byte, 3000)
	for k := range junk {
		junk[k] = byte(0x80 + k%100)
	}
	srv := mediaServer(t, 3, "s%d.ts", func(i int, w http.ResponseWriter, r *http.Request) { w.Write(junk) })
	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/m/index.m3u8", AddOptions{Title: "Junk"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e, v.ID, StatusCompleted, StatusFailed)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "aren't audio or video") {
		t.Fatalf("status %s err %q: a junk capture must fail, not complete", v.Status, v.Error)
	}
}

func TestHLSGzipPlaylist(t *testing.T) {
	const n = 3
	mux := http.NewServeMux()
	mux.HandleFunc("/g/pl.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.0,\ns%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		var z bytes.Buffer
		zw := gzip.NewWriter(&z)
		zw.Write([]byte(b.String()))
		zw.Close()
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Encoding", "gzip") // although we asked for identity
		w.Write(z.Bytes())
	})
	mux.HandleFunc("/g/", func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/g/s"), ".ts"))
		w.Write(tsSegment(i, 6))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newRunningEngine(t)
	res, err := e.Inspect(context.Background(), srv.URL+"/g/pl.m3u8", "", nil, "Gzip playlist")
	if err != nil || res.Kind != "hls" || res.Duration != 12 {
		t.Fatalf("inspect = %+v, %v", res, err)
	}
	v, err := e.AddWithOptions(srv.URL+"/g/pl.m3u8", AddOptions{Title: "Gzip playlist"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	var want []byte
	for i := 0; i < n; i++ {
		want = append(want, tsSegment(i, 6)...)
	}
	if got := readFile(t, v.FilePath); !bytes.Equal(got, want) {
		t.Fatalf("output %d bytes (%s), want %d", len(got), filepath.Base(v.FilePath), len(want))
	}
}

// aacSegment is packed ADTS audio behind an ID3 timestamp tag, as HLS sends it.
func aacSegment(i int) []byte {
	b := []byte("ID3\x04\x00\x00\x00\x00\x00\x05PRIV\x00")
	for f := 0; f < 20; f++ {
		b = append(b, 0xFF, 0xF1, 0x50, 0x80, 0x0C, 0x1F, 0xFC)
		for k := 0; k < 90; k++ {
			b = append(b, byte(0x80+(i*13+f*7+k)%120))
		}
	}
	return b
}

func mp3Segment(i int) []byte {
	var b []byte
	for f := 0; f < 20; f++ {
		b = append(b, 0xFF, 0xFB, 0x90, 0x64)
		for k := 0; k < 100; k++ {
			b = append(b, byte(0x80+(i*11+f*5+k)%120))
		}
	}
	return b
}

func TestHLSAudioOnlyNames(t *testing.T) {
	e := newRunningEngine(t)

	// AAC segments: the capture is audio (.aac raw without ffmpeg, .m4a with).
	aac := mediaServer(t, 3, "a%d.aac", func(i int, w http.ResponseWriter, r *http.Request) { w.Write(aacSegment(i)) })
	res, err := e.Inspect(context.Background(), aac.URL+"/m/index.m3u8", "", nil, "Radio Show")
	if err != nil || res.FileName != "Radio Show.m4a" {
		t.Errorf("inspect AAC: name %q err %v, want Radio Show.m4a", res.FileName, err)
	}
	v, err := e.AddWithOptions(aac.URL+"/m/index.m3u8", AddOptions{Title: "Radio Show"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	if ext := filepath.Ext(v.FilePath); ext != ".aac" && ext != ".m4a" {
		t.Errorf("AAC capture saved as %q", filepath.Base(v.FilePath))
	}

	// MP3 segments behind .ts names: recognized by their bytes, kept as .mp3.
	mp3 := mediaServer(t, 3, "s%d.ts", func(i int, w http.ResponseWriter, r *http.Request) { w.Write(mp3Segment(i)) })
	res, err = e.Inspect(context.Background(), mp3.URL+"/m/index.m3u8", "", nil, "Podcast")
	if err != nil || res.FileName != "Podcast.mp3" {
		t.Errorf("inspect MP3: name %q err %v, want Podcast.mp3", res.FileName, err)
	}
	v, err = e.AddWithOptions(mp3.URL+"/m/index.m3u8", AddOptions{Title: "Podcast"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	if base := filepath.Base(v.FilePath); base != "Podcast.mp3" {
		t.Errorf("MP3 capture saved as %q, want Podcast.mp3", base)
	}

	// An audio-only master (CODECS without video).
	mux := http.NewServeMux()
	mux.HandleFunc("/am.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=128000,CODECS=\"mp4a.40.2\"\n"+aac.URL+"/m/index.m3u8\n")
	})
	am := httptest.NewServer(mux)
	defer am.Close()
	res, err = e.Inspect(context.Background(), am.URL+"/am.m3u8", "", nil, "Live Radio")
	if err != nil || res.FileName != "Live Radio.m4a" || res.Size != 128000/8*12 {
		t.Errorf("inspect audio master: %+v err %v", res, err)
	}
}

func TestInspectStreamSizes(t *testing.T) {
	mux := http.NewServeMux()
	media := func(n int, seg string) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:6\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "#EXTINF:6.0,\n"+seg+"\n", i)
		}
		return b.String() + "#EXT-X-ENDLIST\n"
	}
	// Peak 8 Mb/s, average 1.8 Mb/s (shaka's angel-one): the average sizes it.
	mux.HandleFunc("/avg/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=8062646,AVERAGE-BANDWIDTH=1824731,RESOLUTION=768x576,CODECS=\"avc1.4d401f,mp4a.40.2\"\nv.m3u8\n")
	})
	mux.HandleFunc("/avg/v.m3u8", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, media(10, "s%d.ts")) })
	// Byte ranges for video and for the separate audio rendition.
	mux.HandleFunc("/br/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"en\",DEFAULT=YES,URI=\"a.m3u8\"\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\",AUDIO=\"a\"\nv.m3u8\n")
	})
	mux.HandleFunc("/br/v.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MAP:URI=\"v.mp4\",BYTERANGE=\"800@0\"\n"+
			"#EXTINF:6,\n#EXT-X-BYTERANGE:100000@800\nv.mp4\n#EXTINF:6,\n#EXT-X-BYTERANGE:120000\nv.mp4\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/br/a.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MAP:URI=\"a.mp4\",BYTERANGE=\"600@0\"\n"+
			"#EXTINF:6,\n#EXT-X-BYTERANGE:9000@600\na.mp4\n#EXTINF:6,\n#EXT-X-BYTERANGE:9500\na.mp4\n#EXT-X-ENDLIST\n")
	})
	// A bare media playlist without any bitrate: measured from segments.
	seg := bytes.Repeat([]byte{0x47, 0x01, 0x02, 0x03}, 12500) // 50000 bytes
	mux.HandleFunc("/nobw/index.m3u8", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, media(4, "c%d.ts")) })
	mux.HandleFunc("/nobw/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "c.ts", time.Time{}, bytes.NewReader(seg))
	})
	// Redundant variants whose playlists are offline.
	mux.HandleFunc("/off/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360\nhttp://"+r.Host+"/gone/a.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360\nhttp://"+r.Host+"/gone/b.m3u8\n")
	})
	// A DASH manifest.
	mux.HandleFunc("/dash/Manifest.mpd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprint(w, `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT60S"><Period></Period></MPD>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e := newRunningEngine(t)
	ctx := context.Background()

	res, err := e.Inspect(ctx, srv.URL+"/avg/master.m3u8", "", nil, "Angel One")
	if want := avgRate(1824731, 60); err != nil || res.Size != want || !res.SizeEstimated {
		t.Errorf("average bandwidth: size %d err %v, want %d", res.Size, err, want)
	}
	res, err = e.Inspect(ctx, srv.URL+"/br/master.m3u8", "", nil, "Byte ranges")
	if want := int64(100000 + 120000 + 9000 + 9500); err != nil || res.Size != want {
		t.Errorf("byte ranges: size %d err %v, want %d (video + audio rendition)", res.Size, err, want)
	}
	res, err = e.Inspect(ctx, srv.URL+"/nobw/index.m3u8", "", nil, "No bitrate")
	if want := int64(4 * 50000); err != nil || res.Size != want {
		t.Errorf("no bitrate: size %d err %v, want %d from segment sizes", res.Size, err, want)
	}
	if _, err = e.Inspect(ctx, srv.URL+"/off/master.m3u8", "", nil, ""); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("offline variants: err %v, want an 'offline' error", err)
	}
	res, err = e.Inspect(ctx, srv.URL+"/dash/Manifest.mpd", "", nil, "Big Buck Bunny")
	if err != nil || res.Kind != "dash" || res.FileName != "Big Buck Bunny.mp4" || res.Size != -1 {
		t.Errorf("dash: %+v err %v", res, err)
	}
}

// TestHLSRedundantVariantFallback: the best variant's playlist is gone, the
// backup copy downloads.
func TestHLSRedundantVariantFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360\ngone/v.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360\nbackup/v.m3u8\n")
	})
	mux.HandleFunc("/backup/v.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\ns0.ts\n#EXTINF:4,\ns1.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/backup/", func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/backup/s"), ".ts"))
		w.Write(tsSegment(i, 5))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/master.m3u8", AddOptions{Title: "Backup"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	if got := readFile(t, v.FilePath); !bytes.Equal(got, append(tsSegment(0, 5), tsSegment(1, 5)...)) {
		t.Fatalf("output %d bytes", len(got))
	}
}

func TestHLSEncryptionKinds(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	iv := []byte("0123456789abcdef")
	const n = 3
	mux := http.NewServeMux()
	playlist := func(keyLine string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n" + keyLine + "\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "#EXTINF:4,\n/seg/%d.ts\n", i)
			}
			fmt.Fprint(w, b.String()+"#EXT-X-ENDLIST\n")
		}
	}
	mux.HandleFunc("/aes256.m3u8", playlist(`#EXT-X-KEY:METHOD=AES-256,URI="/key256",IV=0x`+fmt.Sprintf("%x", iv)))
	mux.HandleFunc("/sample.m3u8", playlist(`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="/key256",KEYFORMAT="identity"`))
	mux.HandleFunc("/ctr.m3u8", playlist(`#EXT-X-KEY:METHOD=AES-512-GCM,URI="/key256"`))
	mux.HandleFunc("/ctrok.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-KEY:METHOD=AES-256-CTR,URI=\"/key256\",IV=0x%x\n"+
			"#EXTINF:4,\n/ctrseg.ts\n#EXT-X-ENDLIST\n", iv)
	})
	mux.HandleFunc("/ctrseg.ts", func(w http.ResponseWriter, r *http.Request) {
		out, _ := aesCTR(tsSegment(9, 7), key, iv) // CTR is its own inverse
		w.Write(out)
	})
	mux.HandleFunc("/key256", func(w http.ResponseWriter, r *http.Request) { w.Write(key) })
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/seg/"), ".ts"))
		w.Write(encryptCBC(tsSegment(i, 7), key, iv))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e := newRunningEngine(t)
	ctx := context.Background()

	if res, err := e.Inspect(ctx, srv.URL+"/aes256.m3u8", "", nil, "Clear key"); err != nil || res.Kind != "hls" {
		t.Errorf("AES-256 inspect: kind %q err %v, want hls (not DRM)", res.Kind, err)
	}
	if res, err := e.Inspect(ctx, srv.URL+"/sample.m3u8", "", nil, ""); err != nil || res.Kind != "drm" {
		t.Errorf("SAMPLE-AES inspect: kind %q err %v, want drm", res.Kind, err)
	}
	if _, err := e.Inspect(ctx, srv.URL+"/ctr.m3u8", "", nil, ""); err == nil || strings.Contains(err.Error(), "DRM") {
		t.Errorf("unknown method inspect: err %v, want an 'unsupported encryption' error", err)
	}
	cv, err := e.AddWithOptions(srv.URL+"/ctrok.m3u8", AddOptions{Title: "Counter mode"})
	if err != nil {
		t.Fatal(err)
	}
	cv = mustComplete(t, e, cv.ID)
	if got := readFile(t, cv.FilePath); !bytes.Equal(got, tsSegment(9, 7)) {
		t.Fatalf("AES-256-CTR output %d bytes", len(got))
	}
	v, err := e.AddWithOptions(srv.URL+"/aes256.m3u8", AddOptions{Title: "Clear key"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	var want []byte
	for i := 0; i < n; i++ {
		want = append(want, tsSegment(i, 7)...)
	}
	if got := readFile(t, v.FilePath); !bytes.Equal(got, want) {
		t.Fatalf("AES-256 output %d bytes, want %d", len(got), len(want))
	}
}

// avgRate is the size a bitrate gives over secs, as Inspect computes it.
func avgRate(bps int64, secs float64) int64 { return int64(float64(bps) / 8 * secs) }
