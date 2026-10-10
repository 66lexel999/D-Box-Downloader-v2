// Package ytdlp wraps the external yt-dlp binary so MyIDM can download
// streaming-site media (YouTube, etc.) that its single-file segmented engine
// can't handle. yt-dlp (and ffmpeg, for muxing) are optional: if absent, only
// direct-file downloads work and video probes return ErrNotInstalled.
package ytdlp

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"myidm/internal/procutil"
)

var ErrNotInstalled = errors.New(
	"yt-dlp not found - put yt-dlp.exe next to DBox.exe or on PATH")

func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

// searchDirs are extra roots to look in (set by the engine to the download dir),
// so dropping yt-dlp.exe / an extracted ffmpeg build into MyIDM's Programs
// folder just works.
var (
	searchMu   sync.RWMutex
	searchDirs []string
)

// SetSearchDirs registers extra directories to scan for the binaries. Safe to
// call while downloads are running (the download folder can change live).
func SetSearchDirs(dirs ...string) {
	searchMu.Lock()
	searchDirs = append([]string(nil), dirs...)
	searchMu.Unlock()
}

func statFile(p string) string {
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

func firstGlob(pattern string) string {
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		if p := statFile(m); p != "" {
			return p
		}
	}
	return ""
}

// find looks next to the MyIDM executable, then the configured download dir
// (incl. its Programs category and one level of subfolders, e.g. an extracted
// ffmpeg-*/bin/ffmpeg.exe), then PATH.
func find(name string) string {
	n := exeName(name)
	var roots []string
	if self, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(self))
	}
	if td := getToolsDir(); td != "" {
		roots = append(roots, td) // on-demand tools (ffmpeg) live here
	}
	searchMu.RLock()
	roots = append(roots, searchDirs...)
	searchMu.RUnlock()
	for _, root := range roots {
		if root == "" {
			continue
		}
		if p := statFile(filepath.Join(root, n)); p != "" {
			return p
		}
		for _, pat := range []string{
			filepath.Join(root, "*", n),
			filepath.Join(root, "*", "bin", n),
			filepath.Join(root, "Programs", n),
			filepath.Join(root, "Programs", "*", n),
			filepath.Join(root, "Programs", "*", "bin", n),
		} {
			if p := firstGlob(pat); p != "" {
				return p
			}
		}
	}
	if p, err := exec.LookPath(n); err == nil {
		return p
	}
	return ""
}

func Locate() string     { return find("yt-dlp") }
func ffmpegPath() string { return find("ffmpeg") }
func Available() bool    { return Locate() != "" }

// ---- browser cookies (for login-gated sites: Instagram stories/reels, etc.) --
//
// yt-dlp's own --cookies-from-browser can't read a Chromium browser's cookie DB
// while that browser is RUNNING (the file is exclusively locked on Windows —
// yt-dlp #7271), which is the common case. So instead the browser EXTENSION,
// which already holds the live session, reads the cookies via chrome.cookies and
// posts them here; we write a Netscape cookies.txt per registrable domain and
// hand yt-dlp --cookies <file> for matching URLs. No browser cooperation needed.

var (
	cookiesMu  sync.RWMutex
	cookiesDir string
)

// SetCookiesDir sets where per-domain cookies.txt files live (created on demand).
func SetCookiesDir(dir string) {
	cookiesMu.Lock()
	cookiesDir = dir
	cookiesMu.Unlock()
}

// Cookie mirrors the fields chrome.cookies.getAll returns that matter for a
// Netscape cookie jar.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
	HostOnly bool    `json:"hostOnly"`
	Expiry   float64 `json:"expiry"` // epoch seconds; <=0 means a session cookie
}

// multipartTLDs are second-level labels that are effectively part of the TLD, so
// the registrable domain is the last THREE labels (e.g. bbc.co.uk), not two.
var multipartTLDs = map[string]bool{
	"co": true, "com": true, "org": true, "net": true, "gov": true, "edu": true, "ac": true,
}

// baseDomain reduces a host to its registrable domain (instagram.com,
// bbc.co.uk) so one cookie jar covers www./m./i. subdomains alike. Leading dots
// (from cookie domains like ".instagram.com") are trimmed first.
func baseDomain(host string) string {
	host = strings.Trim(strings.ToLower(host), ".")
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	n := 2
	if multipartTLDs[labels[len(labels)-2]] {
		n = 3
	}
	return strings.Join(labels[len(labels)-n:], ".")
}

func cookieFilePath(domain string) string {
	cookiesMu.RLock()
	dir := cookiesDir
	cookiesMu.RUnlock()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, baseDomain(domain)+".txt")
}

// WriteCookies persists a Netscape cookie jar for the URL's registrable domain,
// replacing any prior jar for it. A nil/empty list removes the jar.
func WriteCookies(rawURL string, cookies []Cookie) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid url")
	}
	path := cookieFilePath(u.Hostname())
	if path == "" {
		return fmt.Errorf("cookies dir not configured")
	}
	if len(cookies) == 0 {
		os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n# Written by D BOX from the browser extension.\n\n")
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		dom := c.Domain
		if dom == "" {
			dom = u.Hostname()
		}
		flag := "TRUE" // include-subdomains: true for a .domain cookie
		if c.HostOnly {
			flag = "FALSE"
		}
		// httpOnly cookies (e.g. Instagram's sessionid) are marked with yt-dlp's
		// #HttpOnly_ prefix — without it the parser drops the auth cookie.
		if c.HTTPOnly {
			dom = "#HttpOnly_" + dom
		}
		secure := "FALSE"
		if c.Secure {
			secure = "TRUE"
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		exp := int64(0)
		if c.Expiry > 0 {
			exp = int64(c.Expiry)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", dom, flag, path, secure, exp, c.Name, c.Value)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// cookieArgsFor returns --cookies <file> when a cookie jar exists for the URL's
// domain, else nil (yt-dlp runs anonymously, as before).
func cookieArgsFor(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil
	}
	path := cookieFilePath(u.Hostname())
	if path == "" || statFile(path) == "" {
		return nil
	}
	return []string{"--cookies", path}
}

// HasCookiesFor reports whether a cookie jar is on disk for the URL's domain.
func HasCookiesFor(rawURL string) bool { return len(cookieArgsFor(rawURL)) > 0 }

// CookieHeader builds a "Cookie:" header value for rawURL from the browser-
// supplied jar of its registrable domain, applying the usual domain / path /
// secure / expiry matching. "" when there is no jar or nothing matches. Lets
// plain HTTP downloads (not just yt-dlp) reach login-gated files.
func CookieHeader(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	path := cookieFilePath(u.Hostname())
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return matchCookies(string(b), u, time.Now())
}

// matchCookies parses a Netscape cookie jar and returns the cookies that apply
// to u, as "a=1; b=2".
func matchCookies(jar string, u *url.URL, now time.Time) string {
	host := strings.ToLower(u.Hostname())
	reqPath := u.EscapedPath()
	if reqPath == "" {
		reqPath = "/"
	}
	var parts []string
	seen := map[string]bool{}
	for _, line := range strings.Split(jar, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "#HttpOnly_") {
			line = strings.TrimPrefix(line, "#HttpOnly_")
		} else if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		domain, sub, cpath, secure, exp, name, value := strings.ToLower(f[0]), f[1], f[2], f[3], f[4], f[5], f[6]
		bare := strings.TrimPrefix(domain, ".")
		switch {
		case host == bare:
		case (sub == "TRUE" || strings.HasPrefix(domain, ".")) && strings.HasSuffix(host, "."+bare):
		default:
			continue
		}
		if cpath != "" && cpath != "/" && !strings.HasPrefix(reqPath, cpath) {
			continue
		}
		if secure == "TRUE" && u.Scheme != "https" {
			continue
		}
		if e, err := strconv.ParseInt(exp, 10, 64); err == nil && e > 0 && time.Unix(e, 0).Before(now) {
			continue
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// RegistrableDomain returns a URL's cookie-jar domain (instagram.com for
// www.instagram.com), or "" if it can't be parsed. Exported so callers can match
// cookie scope — e.g. invalidating cached probes for the same site.
func RegistrableDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return baseDomain(u.Hostname())
}

// aria2Win64URL is aria2's official Windows 64-bit release. aria2 releases
// rarely (1.37.0 is the current stable), so a pinned URL is stable enough; a
// 404 later just logs and leaves yt-dlp on its single-connection downloader.
const aria2Win64URL = "https://github.com/aria2/aria2/releases/download/release-1.37.0/aria2-1.37.0-win-64bit-build1.zip"

// EnsureAria2c makes aria2c available so yt-dlp can use it as a multi-connection
// external downloader (YouTube throttles each connection, so a single stream
// crawls; aria2c opens many and multiplies the throughput). If aria2c isn't
// already found, it downloads the official Windows build into dir and extracts
// aria2c.exe there. Returns the path, or "" if it can't be provided.
func EnsureAria2c(ctx context.Context, dir string) (string, error) {
	if p := find("aria2c"); p != "" {
		return p, nil // already installed somewhere we look
	}
	if runtime.GOOS != "windows" {
		return "", nil // only the Windows build is auto-provisioned
	}
	dest := filepath.Join(dir, "aria2c.exe")
	if statFile(dest) != "" {
		return dest, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aria2Win64URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download aria2: HTTP %d", resp.StatusCode)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return "", fmt.Errorf("open aria2 archive: %w", err)
	}
	for _, f := range zr.File {
		if !strings.EqualFold(filepath.Base(f.Name), "aria2c.exe") {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		tmp := dest + ".tmp"
		out, err := os.Create(tmp)
		if err != nil {
			rc.Close()
			return "", err
		}
		_, copyErr := io.Copy(out, rc) // archive/zip verifies the CRC as it reads
		rc.Close()
		out.Close()
		if copyErr != nil {
			os.Remove(tmp)
			return "", copyErr
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			return "", err
		}
		return dest, nil
	}
	return "", fmt.Errorf("aria2c.exe not found in archive")
}

// SelfUpdate upgrades yt-dlp to the latest release (`yt-dlp -U`). YouTube changes
// its player/signature ("n"/nsig) scheme constantly; an out-of-date yt-dlp can't
// solve it and is handed a THROTTLED media URL (~hundreds of KB/s per connection
// — the reason even aria2c tops out at a few MB/s), while a current one gets the
// full-speed URL. Keeping yt-dlp current is the single biggest speed factor.
// Returns yt-dlp's output; a package-managed install just prints a notice.
func SelfUpdate(ctx context.Context) (string, error) {
	bin := Locate()
	if bin == "" {
		return "", ErrNotInstalled
	}
	cmd := exec.CommandContext(ctx, bin, "-U")
	procutil.Hidden(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Opts is the browser context yt-dlp should present: many embedded players
// (uqload and friends) only serve their stream to requests carrying the page
// they are embedded in as Referer.
type Opts struct {
	UserAgent string
	Referer   string
	Headers   map[string]string // extra headers (Cookie, Origin, Authorization, …)
	// Impersonate makes yt-dlp present a real browser's network fingerprint
	// (its bundled curl_cffi) — what it asks for when a site's Cloudflare bot
	// check turns its plain Python requests away.
	Impersonate bool

	// PlaylistItem (DownloadCmd, ResolveURLs only) downloads that one entry
	// (1-based) of a page with several media. yt-dlp's generic extractor turns
	// every <video>/<audio> on a page into a playlist entry and ignores
	// --no-playlist, so without it all of them land in the same file name.
	PlaylistItem int
	// NativeDownloader (DownloadCmd only) keeps aria2c out of the transfer —
	// the retry when an aria2c attempt failed on a host that caps connections.
	NativeDownloader bool
	// UseAccount lets a YouTube run carry the browser's cookies (as a jar).
	// Off by default: YouTube refuses them as a plain header, and public videos
	// don't need them — see account.go.
	UseAccount bool
}

// UsesAria2c reports whether DownloadCmd hands the transfer to aria2c.
func UsesAria2c(o Opts) bool {
	// aria2c can't impersonate a browser, and NativeDownloader opts out.
	return !o.Impersonate && !o.NativeDownloader && find("aria2c") != ""
}

// playlistArgs selects one entry of a multi-media page, or the single video.
func (o Opts) playlistArgs() []string {
	if o.PlaylistItem > 0 {
		return []string{"--playlist-items", strconv.Itoa(o.PlaylistItem)}
	}
	return []string{"--no-playlist"}
}

// args renders the options as yt-dlp flags.
func (o Opts) args() []string {
	var a []string
	if o.Impersonate {
		a = append(a, "--impersonate", "chrome", "--extractor-args", "generic:impersonate")
		// The impersonated client sends its own browser User-Agent; overriding it
		// would contradict the fingerprint.
		o.UserAgent = ""
	}
	if o.UserAgent != "" {
		a = append(a, "--user-agent", o.UserAgent)
	}
	if o.Referer != "" {
		a = append(a, "--referer", o.Referer)
	}
	keys := make([]string, 0, len(o.Headers))
	for k := range o.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable command lines (and tests)
	for _, k := range keys {
		// Cookies travel as a jar (requestArgs), never as a header: yt-dlp would
		// send a Cookie header to every host, and YouTube rejects it outright.
		if v := o.Headers[k]; k != "" && v != "" && !strings.EqualFold(k, "Referer") && !strings.EqualFold(k, "User-Agent") &&
			!strings.EqualFold(k, "Cookie") {
			a = append(a, "--add-headers", k+":"+v)
		}
	}
	return a
}

// DownloadCmd builds the yt-dlp command for a task. outTemplate should be an
// -o template like "<dir>/<title>.%(ext)s". Resumable via --continue.
func DownloadCmd(ctx context.Context, url, selector, outTemplate string, o Opts, audio bool, concurrency int) *exec.Cmd {
	selector = SelectorWithoutFFmpeg(selector, audio)
	args := downloadArgs(url, selector, outTemplate, o, audio, concurrency, ffmpegPath(), UsesAria2c(o))
	cmd := exec.CommandContext(ctx, Locate(), args...)
	procutil.Hidden(cmd) // no flashing cmd window under a -H windowsgui parent
	// yt-dlp is a Python program: by default its stdout uses the Windows console
	// codepage, which silently drops characters it can't represent (ş, Arabic,
	// …) from the "[download] Destination:" / "[Merger]" lines we parse — even
	// though the file lands on disk with the correct Unicode name. Force Python
	// UTF-8 mode so the paths we capture match what's actually written.
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	return cmd
}

// downloadArgs renders DownloadCmd's flags (split out so tests can check them
// without a yt-dlp binary).
func downloadArgs(url, selector, outTemplate string, o Opts, audio bool, concurrency int, ffmpeg string, aria2 bool) []string {
	if concurrency < 1 {
		concurrency = 4
	}
	args := []string{
		"--no-warnings", "--newline", "--continue",
		// Stamp files with the download time (like a browser) instead of the
		// upload date, so new downloads sort as new in Explorer.
		"--no-mtime",
		// Obscure hosts drop connections and fragments; retry them rather than
		// failing a long download near the end.
		"--retries", "10", "--fragment-retries", "10", "--extractor-retries", "3",
		// Default player-client set (matches Probe/ResolveURLs) so the chosen format
		// — e.g. 1080p — is honored; the android-only override capped quality at 360p.
		"-f", selector, "-o", outTemplate,
	}
	args = append(args, o.playlistArgs()...)
	if ffmpeg != "" {
		args = append(args, "--ffmpeg-location", ffmpeg)
	}
	// YouTube throttles each media connection to roughly playback speed, so a
	// single stream crawls at a few hundred KB/s no matter how fast the link is.
	// Pull many pieces at once to multiply throughput: aria2c (multi-connection
	// per fragment) when it's available, else yt-dlp's own concurrent-fragment
	// downloader plus chunked HTTP (which also resets the throttle per chunk).
	if aria2 {
		// YouTube throttles each connection, so use aria2c's max (16 connections
		// per server, 16 splits) to multiply throughput past the throttle.
		args = append(args,
			"--downloader", "aria2c",
			// -x16/-s16: aria2c's max connections+splits per server. -k1M: small
			// pieces so all 16 connections stay busy. --file-allocation=none skips
			// the pre-allocation stall on big files; --disk-cache=64M smooths
			// high-speed writes. --summary-interval=1 emits a progress line every
			// second (default 60s) so the row shows live speed.
			"--downloader-args",
			"aria2c:-x16 -s16 -k1M --file-allocation=none --disk-cache=64M --summary-interval=1")
	} else {
		args = append(args,
			"--concurrent-fragments", strconv.Itoa(concurrency),
			"--http-chunk-size", "10M")
	}
	switch {
	case audio && ffmpeg != "":
		// An mp3 source is kept as is; anything else is converted.
		args = append(args, "-x", "--audio-format", "mp3")
	case audio:
		// No ffmpeg to convert: keep the best audio stream as-is (m4a/webm).
	default:
		args = append(args, "--merge-output-format", "mp4")
	}
	args = append(args, requestArgs(url, o)...)
	return append(args, url)
}

// SelectorWithoutFFmpeg adapts a format selector when ffmpeg is missing, so
// yt-dlp picks a stream it can save as ONE playable file. Otherwise yt-dlp
// downloads video and audio separately, can't merge them, and leaves a silent
// video. With ffmpeg present the selector is returned unchanged.
func SelectorWithoutFFmpeg(selector string, audio bool) string {
	if ffmpegPath() != "" {
		return selector
	}
	if audio {
		return "ba[ext=m4a]/ba/b"
	}
	if strings.Contains(selector, "+") {
		return "b[vcodec!=none][acodec!=none]/b/" + selector
	}
	return selector
}

// HasFFmpeg reports whether ffmpeg is available (needed to mux the separate
// video+audio streams the burst downloader fetches).
func HasFFmpeg() bool { return ffmpegPath() != "" }

// ResolveURLs runs `yt-dlp -g` to turn a page URL + format selector into the
// direct media URL(s): one element for a progressive/combined format, two
// (video then audio) for an adaptive selection that needs muxing. These
// googlevideo URLs are what MyIDM's burst downloader then fetches itself, far
// faster than letting yt-dlp+aria2c stream them. The URLs are short-lived.
func ResolveURLs(ctx context.Context, pageURL, selector string, o Opts) ([]string, error) {
	bin := Locate()
	if bin == "" {
		return nil, ErrNotInstalled
	}
	args := []string{
		"--no-warnings", "--no-plugin-dirs",
		// Use yt-dlp's DEFAULT player-client set — the SAME one Probe uses — so the
		// format the user picked actually resolves. Forcing player_client=android was
		// ~2x faster but the android client only exposes a low progressive ladder
		// (360p max), so a "1080p" pick silently fell back to 360p (size 800MB→83MB).
		// The default auto-selects whichever client currently works as YouTube shifts.
		"-g", "-f", selector,
	}
	args = append(args, o.playlistArgs()...)
	args = append(args, requestArgs(pageURL, o)...)
	args = append(args, pageURL)
	cmd := exec.CommandContext(ctx, bin, args...)
	procutil.Hidden(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp -g failed: %s", ytdlpErr(err))
	}
	var urls []string
	for _, ln := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		if ln = strings.TrimSpace(ln); strings.HasPrefix(ln, "http") {
			urls = append(urls, ln)
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("yt-dlp -g returned no URLs")
	}
	return urls, nil
}

// MuxCmd builds an ffmpeg command that remuxes a separate video and audio file
// into one container (stream copy, no re-encode) with the moov atom up front.
func MuxCmd(ctx context.Context, videoPath, audioPath, outPath string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, ffmpegPath(),
		"-y", "-loglevel", "error",
		"-i", videoPath, "-i", audioPath,
		"-c", "copy", "-map", "0:v:0", "-map", "1:a:0",
		"-movflags", "+faststart", outPath)
	procutil.Hidden(cmd)
	return cmd
}

var progressRE = regexp.MustCompile(
	`\[download\]\s+([\d.]+)%\s+of\s+(~)?\s*([\d.]+)\s*([KMGT]i)?B(?:\s+at\s+([\d.]+)\s*([KMGT]i)?B/s)?(?:.*?\(frag (\d+)/(\d+)\))?`)

// progressBytesRE matches yt-dlp's progress when the total is unknown (live
// streams, chunked origins): "[download]   12.34MiB at  1.23MiB/s (00:00:10)".
var progressBytesRE = regexp.MustCompile(
	`\[download\]\s+([\d.]+)\s*([KMGT]i)?B\s+at\s+([\d.]+)\s*([KMGT]i)?B/s`)

// ffmpegSizeRE matches ffmpeg's own progress ("size=   10240kB time=…"), shown
// when yt-dlp hands a stream to ffmpeg.
var ffmpegSizeRE = regexp.MustCompile(`size=\s*(\d+)\s*(kB|KiB|MB|MiB|mB|B)\b`)

var destRE = regexp.MustCompile(
	`(?:\[download\] Destination:|\[Merger\] Merging formats into|\[ExtractAudio\] Destination:)\s+"?(.+?)"?\s*$`)

// Progress is a parsed yt-dlp progress line.
type Progress struct {
	Percent    float64 // -1 when only a byte count is known
	Total      int64   // bytes, 0 if unknown
	Estimated  bool    // Total is yt-dlp's "~" estimate (fragmented streams)
	Downloaded int64   // bytes so far, when reported directly (unknown total)
	SpeedBPS   float64
	Frag       int // current fragment (HLS/DASH), 0 if not reported
	Frags      int // total fragments
}

func unitMul(u string) float64 {
	switch strings.TrimSuffix(u, "i") {
	case "K":
		return 1 << 10
	case "M":
		return 1 << 20
	case "G":
		return 1 << 30
	case "T":
		return 1 << 40
	}
	return 1
}

// ParseProgress extracts percent/total/speed from a `[download] ... %` line, or
// the downloaded byte count when yt-dlp doesn't know the total.
func ParseProgress(line string) (Progress, bool) {
	if m := progressRE.FindStringSubmatch(line); m != nil {
		var p Progress
		p.Percent, _ = strconv.ParseFloat(m[1], 64)
		p.Estimated = m[2] == "~"
		if v, err := strconv.ParseFloat(m[3], 64); err == nil {
			p.Total = int64(v * unitMul(m[4]))
		}
		if m[5] != "" {
			if v, err := strconv.ParseFloat(m[5], 64); err == nil {
				p.SpeedBPS = v * unitMul(m[6])
			}
		}
		if m[7] != "" {
			p.Frag, _ = strconv.Atoi(m[7])
			p.Frags, _ = strconv.Atoi(m[8])
		}
		return p, true
	}
	if m := progressBytesRE.FindStringSubmatch(line); m != nil {
		p := Progress{Percent: -1}
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			p.Downloaded = int64(v * unitMul(m[2]))
		}
		if v, err := strconv.ParseFloat(m[3], 64); err == nil {
			p.SpeedBPS = v * unitMul(m[4])
		}
		return p, true
	}
	return Progress{}, false
}

// ParseFFmpegProgress reads the byte count from an ffmpeg "size=" status line.
func ParseFFmpegProgress(line string) (Progress, bool) {
	m := ffmpegSizeRE.FindStringSubmatch(line)
	if m == nil || !strings.Contains(line, "time=") {
		return Progress{}, false
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return Progress{}, false
	}
	mul := int64(1)
	switch m[2] {
	case "kB", "KiB":
		mul = 1 << 10
	case "MB", "MiB", "mB":
		mul = 1 << 20
	}
	return Progress{Percent: -1, Downloaded: v * mul}, true
}

// ParseDestination returns the output path yt-dlp reports for a stream/merge.
func ParseDestination(line string) (string, bool) {
	m := destRE.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// aria2RE matches aria2c's periodic summary line (emitted when yt-dlp uses it as
// the external downloader), e.g. "[#a1b2c3 4.5MiB/100MiB(4%) CN:16 DL:5.2MiB ETA:18s]".
var aria2RE = regexp.MustCompile(
	`([\d.]+)((?:Ki|Mi|Gi)?)B/([\d.]+)((?:Ki|Mi|Gi)?)B\((\d+)%\).*?DL:\s*([\d.]+)((?:Ki|Mi|Gi)?)B`)

// ParseAria2cProgress reads aria2c's progress so video downloads still show a
// live percentage and speed when aria2c (not yt-dlp's own downloader) is doing
// the transfer.
func ParseAria2cProgress(line string) (Progress, bool) {
	m := aria2RE.FindStringSubmatch(line)
	if m == nil {
		return Progress{}, false
	}
	pct, _ := strconv.ParseFloat(m[5], 64)
	return Progress{
		Percent:  pct,
		Total:    int64(aria2Bytes(m[3], m[4])),
		SpeedBPS: aria2Bytes(m[6], m[7]),
	}, true
}

func aria2Bytes(val, unit string) float64 {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0
	}
	switch unit {
	case "Ki":
		f *= 1 << 10
	case "Mi":
		f *= 1 << 20
	case "Gi":
		f *= 1 << 30
	}
	return f
}

func human(n int64) string {
	f := float64(n)
	u := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, u[0])
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}
