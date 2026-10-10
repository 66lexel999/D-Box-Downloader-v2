package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeYtdlp installs a stand-in yt-dlp on PATH that behaves like the real one
// against a Cloudflare-protected stream: plain requests fail with yt-dlp's
// "anti-bot challenge" error; with --impersonate it "downloads" the video and
// audio as separate format files (what yt-dlp leaves when it can't merge).
// Every download invocation's arguments are appended to the returned log file.
func fakeYtdlp(t *testing.T, withFFmpeg bool) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-ins need a POSIX shell")
	}
	bin := t.TempDir()
	logPath = filepath.Join(bin, "calls.log")
	script := `#!/bin/sh
case "$*" in *--newline*) ;; *) exit 1 ;; esac   # only downloads (not -U / -g)
echo "$*" >> "` + logPath + `"
case "$*" in
  *--impersonate*) ;;
  *) echo 'ERROR: [generic] Got HTTP Error 403 caused by Cloudflare anti-bot challenge; try again with --extractor-args "generic:impersonate"' >&2; exit 1 ;;
esac
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
base=$(printf '%s' "$out" | sed 's/\.%(ext)s$//')
printf 'VIDEO-VIDEO-VIDEO-VIDEO' > "$base.f137.mp4"
printf 'AUDIO' > "$base.f140.m4a"
echo "[download] Destination: $base.f140.m4a"
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "yt-dlp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if withFFmpeg {
		// Stand-in ffmpeg: concatenates its -i inputs into the last argument.
		ff := `#!/bin/sh
out=""; ins=""; prev=""
for a in "$@"; do [ "$prev" = "-i" ] && ins="$ins $a"; prev="$a"; out="$a"; done
{ printf 'MERGED:'; for f in $ins; do cat "$f"; done; } > "$out"
`
		if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(ff), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestYtdlpImpersonationAndSeparateParts: a Cloudflare-blocked stream is
// retried with browser impersonation (and the page Referer), and when yt-dlp
// leaves video and audio unmerged the task keeps the VIDEO — it used to point
// at whichever part finished last, the audio.
func TestYtdlpImpersonationAndSeparateParts(t *testing.T) {
	logPath := fakeYtdlp(t, false)
	e := newRunningEngine(t)
	v, err := e.AddVideoWithOptions("https://pegasus.example/api/hls/abc/1080p/playlist.m3u8", VideoOptions{
		Title: "Avatar Episode 1", Referer: "https://cartoony.example/watch/1118/15184",
		Headers: map[string]string{"User-Agent": "BrowserUA/1.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)

	calls, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || strings.Contains(lines[0], "--impersonate") || !strings.Contains(lines[1], "--impersonate chrome") {
		t.Fatalf("expected a plain attempt then an impersonated retry, got:\n%s", calls)
	}
	if !strings.Contains(lines[0], "--referer https://cartoony.example/watch/1118/15184") || !strings.Contains(lines[0], "--user-agent BrowserUA/1.0") {
		t.Errorf("page context not passed to yt-dlp: %s", lines[0])
	}
	if strings.Contains(lines[1], "--user-agent") {
		t.Error("impersonated retry must not override the browser's own User-Agent")
	}
	if base := filepath.Base(v.FilePath); base != "Avatar Episode 1.mp4" {
		t.Errorf("file = %q, want the clean video name", base)
	}
	if got := readFile(t, v.FilePath); string(got) != "VIDEO-VIDEO-VIDEO-VIDEO" {
		t.Errorf("kept %q, want the video part", got)
	}
	if _, err := os.Stat(filepath.Join(v.Dir, "Avatar Episode 1 (audio).m4a")); err != nil {
		t.Errorf("audio part was not kept beside the video: %v", err)
	}
}

// TestYtdlpMergesSeparateParts: with ffmpeg available, leftover video+audio
// parts are merged into one file and the parts removed.
func TestYtdlpMergesSeparateParts(t *testing.T) {
	fakeYtdlp(t, true)
	e := newRunningEngine(t)
	v, err := e.AddVideoWithOptions("https://videos.example/watch/clip.m3u8", VideoOptions{Title: "Clip"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	if got := readFile(t, v.FilePath); !strings.HasPrefix(string(got), "MERGED:") ||
		!strings.Contains(string(got), "VIDEO") || !strings.Contains(string(got), "AUDIO") {
		t.Fatalf("output = %q, want a merge of both parts", got)
	}
	if filepath.Base(v.FilePath) != "Clip.mp4" {
		t.Errorf("file = %s", filepath.Base(v.FilePath))
	}
	ents, _ := os.ReadDir(v.Dir)
	for _, en := range ents {
		if strings.Contains(en.Name(), ".f137.") || strings.Contains(en.Name(), ".f140.") {
			t.Errorf("part left behind: %s", en.Name())
		}
	}
}

// TestChallengedStreamHandsToYtdlp: when the stream server answers D BOX's own
// requests with a Cloudflare challenge, the task is handed to yt-dlp (which
// retries as a browser) instead of failing.
func TestChallengedStreamHandsToYtdlp(t *testing.T) {
	logPath := fakeYtdlp(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		http.Error(w, "Just a moment...", http.StatusForbidden)
	}))
	defer srv.Close()

	e := newRunningEngine(t)
	v, err := e.AddWithOptions(srv.URL+"/api/hls/xyz/1080p/playlist.m3u8", AddOptions{Title: "Episode 2"})
	if err != nil {
		t.Fatal(err)
	}
	v = mustComplete(t, e, v.ID)
	if v.Kind != "ytdlp" || filepath.Base(v.FilePath) != "Episode 2.mp4" {
		t.Fatalf("kind=%s file=%s", v.Kind, filepath.Base(v.FilePath))
	}
	if calls, _ := os.ReadFile(logPath); !strings.Contains(string(calls), "--impersonate") {
		t.Fatalf("yt-dlp was not retried as a browser: %s", calls)
	}
}
