package engine

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestResolveFileName(t *testing.T) {
	mp4 := []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2avc1mp41")
	cases := []struct {
		name string
		h    nameHints
		want string
	}{
		{"disposition wins", nameHints{Disposition: `attachment; filename="Report Q3.pdf"`, OrigURL: "https://x.test/dl?id=1"}, "Report Q3.pdf"},
		{"rfc5987 utf-8", nameHints{Disposition: `attachment; filename*=UTF-8''na%C3%AFve%20caf%C3%A9.txt`}, "naïve café.txt"},
		{"unquoted with spaces", nameHints{Disposition: `attachment; filename=My File (1).zip`}, "My File (1).zip"},
		{"percent-encoded plain", nameHints{Disposition: `attachment; filename="Setup%20v2.exe"`}, "Setup v2.exe"},
		{"latin-1 bytes", nameHints{Disposition: "attachment; filename=\"caf\xe9.txt\""}, "café.txt"},
		{"s3 presigned", nameHints{OrigURL: "https://bucket.s3.amazonaws.com/abc123?response-content-disposition=attachment%3B%20filename%3D%22Big%20Movie.mkv%22&X-Amz-Signature=zz"}, "Big Movie.mkv"},
		{"azure rscd", nameHints{OrigURL: "https://acct.blob.core.windows.net/c/9f8e?rscd=attachment%3B+filename%3Dbackup.7z&sig=q"}, "backup.7z"},
		{"filename param", nameHints{OrigURL: "https://x.test/get.php?filename=driver_pack.zip&t=5"}, "driver_pack.zip"},
		{"loose param needs ext", nameHints{OrigURL: "https://x.test/get?file=12345", ContentType: "application/pdf"}, "get.pdf"},
		{"redirect to hash keeps original", nameHints{OrigURL: "https://x.test/files/App-1.2.exe", FinalURL: "https://cdn.test/o/9f86d081884c7d659a2feaa0c55ad015"}, "App-1.2.exe"},
		{"redirect from generic to named", nameHints{OrigURL: "https://x.test/download?id=9", FinalURL: "https://cdn.test/files/tool-3.0.msi"}, "tool-3.0.msi"},
		{"generic name gets title", nameHints{OrigURL: "https://rr3.googlevideo.test/videoplayback?itag=22", Title: "Cat plays piano", Sniff: mp4}, "Cat plays piano.mp4"},
		{"generic name gets referer slug", nameHints{OrigURL: "https://cdn.test/v/stream", Referer: "https://site.test/videos/sunset-timelapse.html", Sniff: mp4}, "sunset-timelapse.mp4"},
		{"script ext replaced", nameHints{OrigURL: "https://x.test/download.php?id=4", ContentType: "application/x-rar-compressed"}, "download.rar"},
		{"no ext from content-type", nameHints{OrigURL: "https://x.test/media/clip", ContentType: "video/webm; codecs=vp9"}, "clip.webm"},
		{"sniff beats octet-stream", nameHints{OrigURL: "https://x.test/f/latest", ContentType: "application/octet-stream", Sniff: []byte("PK\x03\x04rest")}, "latest.zip"},
		{"keeps real ext", nameHints{OrigURL: "https://x.test/a/notes.txt", ContentType: "application/octet-stream", Sniff: []byte("%PDF")}, "notes.txt"},
		{"numeric ext is not an ext", nameHints{OrigURL: "https://x.test/v/video.123456", Sniff: mp4}, "video.123456.mp4"},
		{"nothing at all", nameHints{OrigURL: "https://x.test/"}, "download"},
	}
	for _, c := range cases {
		if got := resolveFileName(c.h); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStreamBaseName(t *testing.T) {
	cases := []struct{ title, u, ref, want string }{
		{"Episode 4", "https://cdn.test/hls/master.m3u8", "", "Episode 4"},
		{"", "https://cdn.test/shows/the-finale.m3u8", "", "the-finale"},
		{"", "https://cdn.test/hls/index.m3u8?tok=1", "https://site.test/watch/ocean-waves", "ocean-waves"},
		{"", "https://cdn.test/hls/index.m3u8", "https://www.site.test/", "site.test video"},
		{"", "https://www.cdn.test/9f86d081884c7d65/master.m3u8", "", "cdn.test video"},
	}
	cases = append(cases,
		struct{ title, u, ref, want string }{"Learn Node.js in 10 minutes", "https://cdn.test/a.m3u8", "", "Learn Node.js in 10 minutes"},
		struct{ title, u, ref, want string }{"", "https://cdn.test/i/index.m3u8", "https://site.test/v/the-best-cat-video-compilation-2024", "the-best-cat-video-compilation-2024"},
		struct{ title, u, ref, want string }{"", "https://cdn.test/aGVsbG8gd29ybGQgMTIzNDU2Nzg5/index.m3u8", "", "cdn.test video"},
	)
	// With nothing better than the host, the name carries the date and time
	// ("cdn.test video 2026-09-29 20.12") so repeated captures don't collide.
	dated := regexp.MustCompile(` \d{4}-\d{2}-\d{2} \d{2}\.\d{2}$`)
	for _, c := range cases {
		got := streamBaseName(c.title, c.u, c.ref)
		if strings.HasSuffix(c.want, " video") {
			got = dated.ReplaceAllString(got, "")
		}
		if got != c.want {
			t.Errorf("streamBaseName(%q,%q,%q) = %q, want %q", c.title, c.u, c.ref, got, c.want)
		}
	}
}

func TestSanitizeFileName(t *testing.T) {
	cases := map[string]string{
		`a<b>c:d"e|f?g*h.txt`:           "a_b_c_d_e_f_g_h.txt",
		"..\\..\\evil.exe":              "evil.exe",
		"line1\nline2\ttab.mp4":         "line1 line2 tab.mp4",
		"photo\u202Egnp.exe":            "photognp.exe", // RLO spoofing stripped
		"zero\u200Bwidth.txt":           "zerowidth.txt",
		"CON.txt":                       "_CON.txt",
		"nul":                           "_nul",
		"trailing dots... ":             "trailing dots",
		"   ":                           "download",
		"multiple    spaces   here.mkv": "multiple spaces here.mkv",
		"emoji 🎬 title.mp4":             "emoji 🎬 title.mp4",
		"ok.name.with.dots.tar.gz":      "ok.name.with.dots.tar.gz",
		"\xff\xfeinvalid utf8 name.bin": "invalid utf8 name.bin",
	}
	for in, want := range cases {
		if got := SanitizeFileName(in); got != want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("😀", 200) + ".mp4" // 400 UTF-16 units of emoji
	got := SanitizeFileName(long)
	if !strings.HasSuffix(got, ".mp4") || utf16Len(got) > maxNameUnits {
		t.Errorf("long name not capped with its extension: %d units, %q...", utf16Len(got), got[:20])
	}
}

func TestSniffAndClassify(t *testing.T) {
	cases := []struct {
		u, ct string
		body  string
		kind  string
	}{
		{"https://x.test/a.m3u8", "application/vnd.apple.mpegurl", "#EXTM3U\n#EXT-X-VERSION:3", kindHLS},
		{"https://x.test/playlist?id=3", "text/plain", "\uFEFF#EXTM3U\n", kindHLS},
		{"https://x.test/stream.m3u8", "video/mp4", "\x00\x00\x00\x18ftypmp42", kindFile}, // mislabeled URL, real MP4
		{"https://x.test/manifest.mpd", "application/dash+xml", `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash">`, kindDASH},
		{"https://x.test/watch?v=1", "text/html; charset=utf-8", "<!DOCTYPE html><html>", kindPage},
		{"https://x.test/index.html", "text/html", "<html>", kindFile}, // an explicit .html file is a file
		{"https://x.test/file.bin", "application/octet-stream", "MZ\x90\x00", kindFile},
	}
	for _, c := range cases {
		if got := classify(c.u, c.ct, []byte(c.body)); got != c.kind {
			t.Errorf("classify(%s, %s) = %s, want %s", c.u, c.ct, got, c.kind)
		}
	}
	sigs := map[string]string{
		"\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01webm": "webm",
		"ID3\x04\x00":          "mp3",
		"Rar!\x1a\x07\x01\x00": "rar",
		"7z\xbc\xaf\x27\x1c":   "7z",
		"\x89PNG\r\n":          "png",
		"\xff\xd8\xff\xe0":     "jpg",
		"OggS\x00\x02" + strings.Repeat("\x00", 20) + "OpusHead": "opus",
		"RIFF\x00\x00\x00\x00WAVEfmt ":                           "wav",
	}
	for body, want := range sigs {
		if got := sniffExt([]byte(body)); got != want {
			t.Errorf("sniffExt(%q) = %q, want %q", body, got, want)
		}
	}
	if got := detectExt([]byte("PK\x03\x04"), "application/vnd.android.package-archive"); got != "apk" {
		t.Errorf("zip container with APK type = %q, want apk", got)
	}
}

func TestParseHLS(t *testing.T) {
	base, _ := url.Parse("https://cdn.test/show/master.m3u8?token=abc")
	master := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="audio/en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Deutsch",LANGUAGE="de",URI="audio/de.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2",AUDIO="aud"
720/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,AUDIO="aud"
https://other.test/1080/index.m3u8
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=100000,URI="iframes.m3u8"
`
	pl, err := parseHLS(master, base)
	if err != nil || !pl.Master || len(pl.Variants) != 2 {
		t.Fatalf("master parse: %v %+v", err, pl)
	}
	v, _ := pickVariant(pl.Variants)
	if v.Height != 1080 || v.URI != "https://other.test/1080/index.m3u8" {
		t.Errorf("best variant = %+v", v)
	}
	if pl.Variants[0].URI != "https://cdn.test/show/720/index.m3u8" || pl.Variants[0].Codecs != "avc1.64001f,mp4a.40.2" {
		t.Errorf("relative variant / quoted attr parse: %+v", pl.Variants[0])
	}
	a, ok := pickAudio(pl, v)
	if !ok || a.Language != "en" || a.URI != "https://cdn.test/show/audio/en.m3u8" {
		t.Errorf("audio rendition = %+v %v", a, ok)
	}

	media := `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:7
#EXT-X-KEY:METHOD=AES-128,URI="https://keys.test/k?id=1",IV=0x1
#EXTINF:6.006,
seg7.ts
#EXT-X-KEY:METHOD=NONE
#EXTINF:5.5,title
#EXT-X-BYTERANGE:1000@200
big.ts
#EXTINF:4,
#EXT-X-BYTERANGE:500
big.ts
#EXT-X-ENDLIST
`
	mp, err := parseHLS(media, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(mp.Segments) != 3 || mp.live() || mp.duration() != 15.506 {
		t.Fatalf("media parse: %d segs live=%v dur=%v", len(mp.Segments), mp.live(), mp.duration())
	}
	s0, s1, s2 := mp.Segments[0], mp.Segments[1], mp.Segments[2]
	if s0.Seq != 7 || s0.Key == nil || len(s0.Key.IV) != 16 || s0.Key.IV[15] != 1 || s0.Key.URI != "https://keys.test/k?id=1" {
		t.Errorf("segment 0 = %+v key=%+v", s0, s0.Key)
	}
	if s1.Key != nil || s1.Off != 200 || s1.Len != 1000 || s2.Off != 1200 || s2.Len != 500 || s2.Seq != 9 {
		t.Errorf("byte ranges: %+v / %+v", s1, s2)
	}
	if _, err := parseHLS("<html>nope</html>", base); err == nil {
		t.Error("non-playlist accepted")
	}
}

func TestAES128Decrypt(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := []byte("hello, segment!!and some more bytes")
	enc := encryptCBC(plain, key, seqIV(42))
	got, err := aes128Decrypt(enc, key, seqIV(42))
	if err != nil || string(got) != string(plain) {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
	if _, err := aes128Decrypt(enc[:len(enc)-3], key, seqIV(42)); err == nil {
		t.Error("truncated ciphertext accepted")
	}
	if k := normalizeKey([]byte("30313233343536373839616263646566")); string(k) != "0123456789abcdef" {
		t.Errorf("hex key = %q", k)
	}
}

func TestRefererCandidatesAndHeaders(t *testing.T) {
	got := refererCandidates("https://media.cdn.example.com/v.mp4", "https://page.test/watch")
	want := []string{"https://page.test/watch", "", "https://media.cdn.example.com/", "https://example.com/"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("refererCandidates = %q", got)
	}
	h := CleanHeaders(map[string]string{"cookie": "a=1", "Range": "bytes=0-", "X-Evil": "a\r\nInjected: 1", "origin": " https://p.test "})
	if len(h) != 2 || h["Cookie"] != "a=1" || h["Origin"] != "https://p.test" {
		t.Errorf("CleanHeaders = %v", h)
	}
	if got := mergeCookies("a=1; b=2", "b=9; c=3"); got != "a=1; b=2; c=3" {
		t.Errorf("mergeCookies = %q", got)
	}
	if strongETag(`W/"x"`) != "" || strongETag(`"x"`) != `"x"` {
		t.Error("strongETag")
	}
}
