package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findCand returns the candidate whose URL is want (or starts with it when
// want ends in "*").
func findCand(cs []grabCand, want string) (grabCand, bool) {
	prefix := strings.HasSuffix(want, "*")
	want = strings.TrimSuffix(want, "*")
	for _, c := range cs {
		if c.URL == want || (prefix && strings.HasPrefix(c.URL, want)) {
			return c, true
		}
	}
	return grabCand{}, false
}

// TestExtractFixtures runs the scanner over trimmed copies of real pages the
// grabber must handle (testdata/grab) and checks the files people want are
// found and the junk around them is not.
func TestExtractFixtures(t *testing.T) {
	cases := []struct {
		file, base string
		want       []string // URLs (or "prefix*") that must be found
		not        []string // substrings no candidate URL may contain
		check      func(t *testing.T, s pageScan)
	}{
		{
			file: "mediafire.html", base: "https://www.mediafire.com/file/8ipst0t9u6sibpx/Galaxy-s9-wallpaper-1.png/file",
			want: []string{"https://download937.mediafire.com/*"},
			check: func(t *testing.T, s pageScan) {
				c, _ := findCand(s.Cands, "https://download937.mediafire.com/*")
				if !c.Linked || c.Name != "" {
					t.Errorf("download button: linked=%v name=%q (want linked, no \"Download (2.97MB)\" name)", c.Linked, c.Name)
				}
			},
		},
		{
			file: "gdrive-confirm.html", base: "https://drive.google.com/uc?id=0B7EVK8r0v71pZjFTYXZWM3FlRnM&export=download",
			check: func(t *testing.T, s pageScan) {
				c, ok := findCand(s.Cands, "https://drive.usercontent.google.com/download?*")
				if !ok || !strings.Contains(c.URL, "confirm=t") || !strings.Contains(c.URL, "id=0B7EVK8r0v71pZjFTYXZWM3FlRnM") {
					t.Errorf("Drive 'Download anyway' form not turned into a link: %+v", s.Cands)
				}
			},
		},
		{
			file: "moddb-start.html", base: "https://www.moddb.com/downloads/start/317674",
			want: []string{"https://www.moddb.com/downloads/mirror/317674/124/efe19e2dd5e892401e6d48be72b76656"},
			check: func(t *testing.T, s pageScan) {
				c, _ := findCand(s.Cands, "https://www.moddb.com/downloads/mirror/317674/124/efe19e2dd5e892401e6d48be72b76656")
				if !c.Follow || c.Name != "download UZDoom-5.0.3.4.zip" {
					t.Errorf("mirror link: follow=%v name=%q", c.Follow, c.Name)
				}
			},
		},
		{
			file: "moddb-file.html", base: "https://www.moddb.com/mods/qol-power-trip/downloads/qol-power-trip-v60-uzdoom-503-bundle",
			want: []string{"https://www.moddb.com/downloads/start/317674"},
			not:  []string{"/downloads/top", "/downloads/latest"}, // listing tabs, not downloads
			check: func(t *testing.T, s pageScan) {
				if c, _ := findCand(s.Cands, "https://www.moddb.com/downloads/start/317674"); !c.Follow {
					t.Error("\"Download Now\" button should be followed to its landing page")
				}
				if c, ok := findCand(s.Cands, "https://media.moddb.com/images/games/1/1/172/icon.png"); ok && c.Width != 32 {
					t.Errorf("icon dimensions lost: %+v", c)
				}
			},
		},
		{
			file: "commons.html", base: "https://commons.wikimedia.org/wiki/File:Big_Buck_Bunny_4K.webm",
			want: []string{
				"https://upload.wikimedia.org/wikipedia/commons/c/c0/Big_Buck_Bunny_4K.webm",
				"https://upload.wikimedia.org/wikipedia/commons/transcoded/c/c0/Big_Buck_Bunny_4K.webm/Big_Buck_Bunny_4K.webm.1080p.vp9.webm",
			},
			not: []string{"commons.wikimedia.org/wiki/"}, // pages named after the file
		},
		{
			file: "podcast.html", base: "https://theaudacitytopodcast.com/tap072-how-to-use-powerpress/",
			want: []string{
				"https://media.blubrry.com/theaudacitytopodcast/op3.dev/e/traffic.libsyn.com/noodlemx/tap072.mp3",
				"https://media.blubrry.com/podcasting_video_tips_from/op3.dev/e/traffic.libsyn.com/noodlemx/tap072.mp4",
			},
			not: []string{"?_="}, // WordPress cache-buster
			check: func(t *testing.T, s pageScan) {
				c, ok := findCand(s.Cands, "https://www.youtube.com/embed/HdgrYtGQj34*")
				if !ok || !c.Embed {
					t.Errorf("YouTube player not handed to yt-dlp: %+v", c)
				}
				if a, _ := findCand(s.Cands, "https://media.blubrry.com/theaudacitytopodcast/op3.dev/e/traffic.libsyn.com/noodlemx/tap072.mp3"); a.Kind != "audio" {
					t.Errorf("podcast mp3 kind = %q", a.Kind)
				}
			},
		},
		{
			file: "archive.html", base: "https://archive.org/details/Popeye_forPresident",
			want: []string{
				"https://archive.org/download/Popeye_forPresident/Popeye_forPresident_512kb.mp4",
				"https://archive.org/download/Popeye_forPresident/Popeye_forPresident_archive.torrent",
			},
			not: []string{"athena.archive.org/0.gif"}, // hit counter
		},
		{
			file: "samplelib.html", base: "https://samplelib.com/sample-mp4.html",
			want: []string{"https://samplelib.com/mp4/sample-5s.mp4", "https://samplelib.com/mp4/sample-10s-2160p.mp4"},
			check: func(t *testing.T, s pageScan) {
				if c, _ := findCand(s.Cands, "https://samplelib.com/mp4/sample-5s.mp4"); c.Name != "Test video of a road in a city" || c.Kind != "video" {
					t.Errorf("JSON-LD name/kind lost: %+v", c)
				}
			},
		},
		{
			file: "librivox.html", base: "https://librivox.org/the-art-of-war-by-sun-tzu/",
			want: []string{
				"https://archive.org/download/art_of_war_librivox/ArtOfWar-64kb_librivox.m4b",
				"https://www.archive.org/download/art_of_war_librivox/art_of_war_01-02_sun_tzu.mp3",
				"https://archive.org/compress/art_of_war_librivox/*",
			},
		},
		{
			file: "clappr.html", base: "https://clappr.github.io/",
			want: []string{"http://clappr.io/highline.mp4"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "grab", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			s := extractCandidates(tc.base, string(b))
			for _, w := range tc.want {
				if _, ok := findCand(s.Cands, w); !ok {
					t.Errorf("missing %s", w)
				}
			}
			for _, c := range s.Cands {
				for _, n := range tc.not {
					if strings.Contains(c.URL, n) {
						t.Errorf("junk candidate %s (%s)", c.URL, c.Source)
					}
				}
			}
			if tc.check != nil {
				tc.check(t, s)
			}
			if t.Failed() {
				for _, c := range s.Cands {
					t.Logf("  %-13s kind=%-6s embed=%v frame=%v linked=%v %q %s", c.Source, c.Kind, c.Embed, c.Frame, c.Linked, c.Name, c.URL)
				}
			}
		})
	}
}

// TestExtractSynthetic covers the tag rules on one hand-written page.
func TestExtractSynthetic(t *testing.T) {
	doc := `<!doctype html><html><head>
<title>Episode 12 | ExampleTV</title>
<base href="https://cdn.example.com/media/">
<meta property="og:title" content="Episode 12">
<meta property="og:video" content="https://cdn.example.com/og/ep12.mp4">
<meta property="og:image" content="https://cdn.example.com/og/ep12.jpg">
<link rel="icon" href="/favicon.ico">
<script type="application/ld+json">{"@context":"https://schema.org","@type":"VideoObject","name":"Episode Twelve","contentUrl":"https://cdn.example.com/ld/ep12-1080.mp4","thumbnailUrl":"https://cdn.example.com/ld/thumb.jpg","duration":"PT21M30S"}</script>
</head><body>
<video poster="poster.jpg"><source src="clips/ep12.webm" type="video/webm"><track src="subs/ep12.en.vtt" label="English"></video>
<div class="player" data-setup='{"sources":[{"src":"https:\/\/stream.example.com\/hls\/ep12\/master.m3u8","type":"application/x-mpegURL"}]}'></div>
<img src="https://ads.example.net/p.gif" width="1" height="1">
<img srcset="https://img.example.com/w_400,c_fill/a.jpg 400w, https://img.example.com/w_1600,c_fill/a.jpg 1600w" alt="">
<a href="chrome_setup.exe">Chrome installer</a>
<a href="/collections/2024/extras.zip" download="Extras.zip">Extras</a>
<a href="https://www.google-analytics.com/collect?v=1">x</a>
<a href="/downloads">All downloads</a>
<iframe src="https://player.example.org/embed/77"></iframe>
<iframe src="https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ"></iframe>
<script>import {x} from "https://cdn.example.com/app/main.ts"; var cfg = {file: "/v/ep12-720.mp4", name: "ep12.webm"};</script>
</body></html>`
	s := extractCandidates("https://www.example.com/shows/ep12", doc)
	if s.Title != "Episode 12 | ExampleTV" || s.OGTitle != "Episode 12" {
		t.Errorf("titles = %q / %q", s.Title, s.OGTitle)
	}
	want := map[string]string{ // URL -> expected kind ("" = don't care)
		"https://cdn.example.com/og/ep12.mp4":                 "video",
		"https://cdn.example.com/ld/ep12-1080.mp4":            "video",
		"https://cdn.example.com/media/clips/ep12.webm":       "video",
		"https://cdn.example.com/media/subs/ep12.en.vtt":      "subtitle",
		"https://cdn.example.com/media/poster.jpg":            "image",
		"https://stream.example.com/hls/ep12/master.m3u8":     "stream",
		"https://img.example.com/w_1600,c_fill/a.jpg":         "image",
		"https://cdn.example.com/media/chrome_setup.exe":      "",
		"https://cdn.example.com/collections/2024/extras.zip": "",
		"https://player.example.org/embed/77":                 "",
		"https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ":  "",
		"https://cdn.example.com/v/ep12-720.mp4":              "video",
	}
	for u, kind := range want {
		c, ok := findCand(s.Cands, u)
		if !ok {
			t.Errorf("missing %s", u)
			continue
		}
		if kind != "" && c.Kind != kind {
			t.Errorf("%s: kind %q, want %q", u, c.Kind, kind)
		}
	}
	if c, _ := findCand(s.Cands, "https://cdn.example.com/ld/ep12-1080.mp4"); c.Name != "Episode Twelve" || c.Duration != 1290 {
		t.Errorf("JSON-LD name/duration: %+v", c)
	}
	if c, _ := findCand(s.Cands, "https://cdn.example.com/collections/2024/extras.zip"); c.Name != "Extras.zip" || !c.Linked {
		t.Errorf("download attribute: %+v", c)
	}
	if c, _ := findCand(s.Cands, "https://player.example.org/embed/77"); !c.Frame {
		t.Errorf("unknown iframe should be read one level deep: %+v", c)
	}
	if c, _ := findCand(s.Cands, "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ"); !c.Embed {
		t.Errorf("YouTube iframe should go to yt-dlp: %+v", c)
	}
	for _, c := range s.Cands {
		for _, bad := range []string{"p.gif", "favicon", "google-analytics", "main.ts", "w_400", "/downloads", "media/ep12.webm"} {
			if strings.Contains(c.URL, bad) {
				t.Errorf("junk candidate %s (%s)", c.URL, c.Source)
			}
		}
	}
}

func TestGrabLargestSrcset(t *testing.T) {
	cases := map[string]string{
		"a.jpg 480w, b.jpg 1080w": "b.jpg",
		"a.jpg 1x,b.jpg 2x":       "b.jpg",
		"only.jpg":                "only.jpg",
		"https://i.x/w_400,h_300/a.jpg 400w, https://i.x/w_1200,h_900/a.jpg 1200w": "https://i.x/w_1200,h_900/a.jpg",
		"https://i.x/a.jpg,https://i.x/b.jpg":                                      "https://i.x/a.jpg,https://i.x/b.jpg", // one URL with a comma
	}
	for in, want := range cases {
		if got := grabLargestSrcset(in); got != want {
			t.Errorf("grabLargestSrcset(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGrabUsefulText(t *testing.T) {
	for _, s := range []string{"Download (2.97MB)", "download 1 file", "175.31mb Download Now", "Download", "here", "↓"} {
		if grabUsefulText(s) {
			t.Errorf("%q should not name a file", s)
		}
	}
	for _, s := range []string{"01 - Laying Plans", "download UZDoom-5.0.3.4.zip", "2021 Annual Report", "Big Buck Bunny"} {
		if !grabUsefulText(s) {
			t.Errorf("%q should name a file", s)
		}
	}
}

func TestGrabKeyDedup(t *testing.T) {
	pairs := [][2]string{
		{"https://archive.org/download/x/My%20File.mp4", "https://archive.org/download/x/My File.mp4"},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "https://youtu.be/dQw4w9WgXcQ"},
		{"https://www.youtube.com/embed/dQw4w9WgXcQ?rel=0", "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ"},
	}
	for _, p := range pairs {
		if grabKey(p[0]) != grabKey(p[1]) {
			t.Errorf("grabKey(%q) != grabKey(%q)", p[0], p[1])
		}
	}
	if grabKey("https://x.test/a.mp4?q=1") == grabKey("https://x.test/a.mp4?q=2") {
		t.Error("different queries must stay different files")
	}
}

func TestExtractTextLinks(t *testing.T) {
	text := `Mirror: <a href="https://files.example.com/a.zip?x=1&amp;y=2">a.zip</a>
also https://files.example.com/b.rar, and (https://files.example.com/c.7z).
not a link: ftp://old.example.com/d.zip`
	got := extractTextLinks(text)
	want := []string{"https://files.example.com/a.zip?x=1&y=2", "https://files.example.com/b.rar", "https://files.example.com/c.7z"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("extractTextLinks = %q, want %q", got, want)
	}
}
