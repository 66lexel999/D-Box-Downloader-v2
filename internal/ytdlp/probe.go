package ytdlp

// Probe: what a page offers, as the quality list the UI shows.
//
// yt-dlp -J describes either one video or — for the pages its generic
// extractor handles, i.e. most uncommon sites — a playlist of every <video> and
// <audio> on the page in DOM order; --no-playlist does not apply there. The
// first entry is often a sound effect, an intro or an ad, so every entry is
// kept and ranked and the main media is recommended instead of whichever came
// first. Formats are classified by what is known about them: the generic
// extractor's <video src> files carry no codec, height, size or duration, so
// codecs alone would discard them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"myidm/internal/procutil"
)

// probeEntryCap bounds how many entries -J resolves: a channel or gallery page
// would otherwise resolve hundreds of videos and time out.
const probeEntryCap = 30

// retryBudget is the fresh deadline a probe retry (browser impersonation, an
// alternative URL) gets, so a slow first attempt can't starve it.
const retryBudget = 45 * time.Second

// jsonInt reads a yt-dlp number that is usually an int but can arrive as a
// float (computed estimates) or null, without failing the whole document.
type jsonInt int64

func (n *jsonInt) UnmarshalJSON(b []byte) error {
	var f float64
	if json.Unmarshal(b, &f) != nil {
		f = 0 // null, a string, anything odd: unknown
	}
	*n = jsonInt(f)
	return nil
}

// drmFlag reads has_drm, which is true, false or "maybe" (still attempted).
type drmFlag bool

func (d *drmFlag) UnmarshalJSON(b []byte) error {
	*d = string(b) == "true"
	return nil
}

// Format is the subset of yt-dlp's -J output we care about.
type Format struct {
	ID             string            `json:"format_id"`
	URL            string            `json:"url"`
	Protocol       string            `json:"protocol"`
	Ext            string            `json:"ext"`
	Width          jsonInt           `json:"width"`
	Height         jsonInt           `json:"height"`
	FPS            float64           `json:"fps"`
	VCodec         string            `json:"vcodec"`
	ACodec         string            `json:"acodec"`
	FileSize       jsonInt           `json:"filesize"`
	FileSizeApprox jsonInt           `json:"filesize_approx"`
	TBR            float64           `json:"tbr"` // total bitrate (kbps), for size estimation
	ABR            float64           `json:"abr"`
	VBR            float64           `json:"vbr"`
	FormatNote     string            `json:"format_note"`
	HasDRM         drmFlag           `json:"has_drm"`
	HTTPHeaders    map[string]string `json:"http_headers"`

	notMedia bool // the server answered this URL with a web page
}

// info is one -J document: a video, or a playlist whose entries are videos.
type info struct {
	// A result without a formats list (url_transparent, a bare direct link)
	// carries its one format's fields at the top level.
	Format
	Type       string   `json:"_type"`
	VideoID    string   `json:"id"`
	Title      string   `json:"title"`
	WebpageURL string   `json:"webpage_url"`
	Duration   float64  `json:"duration"` // seconds, for size estimation
	Thumbnail  string   `json:"thumbnail"`
	Extractor  string   `json:"extractor_key"`
	IsLive     bool     `json:"is_live"`
	Index      jsonInt  `json:"playlist_index"`
	Formats    []Format `json:"formats"`
	Entries    []*info  `json:"entries"`
}

// DownOption is one entry in the overlay's quality dropdown.
type DownOption struct {
	Label     string `json:"label"`    // "MP4 - 1080p 60fps (~335 MB)"
	Selector  string `json:"selector"` // yt-dlp -f selector
	Ext       string `json:"ext"`      // expected container
	Audio     bool   `json:"audio"`    // audio-only (extract to mp3)
	Size      int64  `json:"size"`
	Estimated bool   `json:"estimated,omitempty"` // Size is an estimate
	// PlaylistItem is the page entry (1-based) this option downloads; pass it
	// on as the task's PlaylistItem. 0 for a single video.
	PlaylistItem int `json:"playlistItem,omitempty"`
}

// ProbeEntry is one media item of a page that holds several.
type ProbeEntry struct {
	Index         int          `json:"index"` // 1-based, for --playlist-items
	Title         string       `json:"title"`
	URL           string       `json:"url,omitempty"`
	Duration      float64      `json:"duration,omitempty"`
	Thumbnail     string       `json:"thumbnail,omitempty"`
	Ext           string       `json:"ext,omitempty"`
	HasVideo      bool         `json:"hasVideo"`
	Size          int64        `json:"size"`
	SizeEstimated bool         `json:"sizeEstimated,omitempty"`
	Options       []DownOption `json:"options"`

	ad bool // served from an ad/tracker host
}

// ProbeResult summarises what's downloadable at a URL for the UI.
type ProbeResult struct {
	Title   string       `json:"title"`
	Options []DownOption `json:"options"` // the recommended entry's options
	// Entries lists every media item of a page that holds several;
	// Recommended is the Index of the main one (0 for a single video).
	Entries     []ProbeEntry `json:"entries,omitempty"`
	Recommended int          `json:"recommended"`
	Duration    float64      `json:"duration,omitempty"`
	Extractor   string       `json:"extractor,omitempty"`
	IsLive      bool         `json:"isLive,omitempty"`
	HasVideo    bool         `json:"hasVideo"`
	Thumbnail   string       `json:"thumbnail,omitempty"`
	// DirectFile is set when the "video" is really a plain file (a Dropbox
	// archive, a mirror page's .exe): download that URL natively instead.
	DirectFile string `json:"directFile,omitempty"`
}

// Probe runs `yt-dlp -J` and distils the result into quality options.
func Probe(ctx context.Context, rawURL string, o Opts) (*ProbeResult, error) {
	in, err := probeJSON(ctx, rawURL, o)
	if err != nil && !errors.Is(err, ErrNotInstalled) && !errors.Is(ctx.Err(), context.Canceled) {
		msg := err.Error()
		retry := true
		switch {
		case AccountRetry(rawURL, o, msg):
			o.UseAccount = true // age-restricted/members-only: once more, signed in
		case !o.Impersonate && NeedsImpersonation(msg):
			o.Impersonate = true // once more, looking like a real browser
		case AltURL(rawURL, msg) != "":
			rawURL = AltURL(rawURL, msg)
		default:
			retry = false
		}
		if retry {
			rctx, cancel := retryContext(ctx, retryBudget)
			in, err = probeJSON(rctx, rawURL, o)
			cancel()
		}
	}
	if err != nil {
		if errors.Is(err, ErrNotInstalled) {
			return nil, err
		}
		return nil, errors.New(FriendlyError(err.Error()))
	}
	normalize(in)
	sctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	fillSizes(sctx, in, o)
	cancel()
	res := buildResult(in)
	if len(res.Options) == 0 && res.DirectFile == "" {
		return nil, errors.New("no downloadable video or audio found on this page")
	}
	return res, nil
}

// probeJSON runs one `yt-dlp -J` and parses it. With --ignore-errors one broken
// embed doesn't sink the page: yt-dlp still prints the entries it could read
// (and exits non-zero), which are used.
func probeJSON(ctx context.Context, rawURL string, o Opts) (*info, error) {
	bin := Locate()
	if bin == "" {
		return nil, ErrNotInstalled
	}
	args := []string{"-J", "--no-warnings", "--no-playlist", "--ignore-errors",
		"--playlist-end", strconv.Itoa(probeEntryCap)}
	args = append(args, requestArgs(rawURL, o)...)
	args = append(args, rawURL)
	cmd := exec.CommandContext(ctx, bin, args...)
	procutil.Hidden(cmd) // no flashing cmd window under a -H windowsgui parent
	// UTF-8 mode so titles with non-ASCII characters survive the JSON.
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	out, err := cmd.Output()
	if body := bytes.TrimSpace(out); len(body) > 0 && string(body) != "null" {
		var in info
		jerr := json.Unmarshal(body, &in)
		if jerr == nil && (err == nil || len(in.Formats) > 0 || len(in.Entries) > 0 || in.URL != "") {
			return &in, nil
		}
		if err == nil {
			return nil, fmt.Errorf("parse yt-dlp output: %w", jerr)
		}
	}
	if err != nil {
		return nil, errors.New(ytdlpErr(err))
	}
	return nil, errors.New("yt-dlp returned nothing")
}

// retryContext gives a retry a fresh deadline while still honouring an
// explicit cancel (the user closed the dialog, the task was paused).
func retryContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), d)
	stop := context.AfterFunc(parent, func() {
		if errors.Is(parent.Err(), context.Canceled) {
			cancel()
		}
	})
	return ctx, func() { stop(); cancel() }
}

// normalize gives every document a formats list: a result yt-dlp resolved to
// one URL (url_transparent, a direct link) carries it at the top level.
func normalize(in *info) {
	if in == nil {
		return
	}
	if len(in.Formats) == 0 && strings.HasPrefix(in.URL, "http") {
		in.Formats = []Format{in.Format}
	}
	for _, e := range in.Entries {
		normalize(e)
	}
}

// ---- classification -----------------------------------------------------------

type fkind int

const (
	fkSkip      fkind = iota // not media (storyboard, image, archive, web page)
	fkAudio                  // sound only
	fkVideoOnly              // picture only: yt-dlp muxes an audio stream in
	fkMuxed                  // picture and sound (or picture with sound unknown)
)

var (
	audioExts = setOf("mp3", "m4a", "aac", "opus", "oga", "ogg", "flac", "wav", "weba", "mka", "wma", "alac", "aiff", "ape")
	videoExts = setOf("mp4", "m4v", "mov", "webm", "mkv", "flv", "avi", "wmv", "ogv", "3gp", "3g2", "ts", "m2ts", "mts",
		"mpg", "mpeg", "f4v", "asf", "rm", "rmvb", "vob", "divx")
	imageExts = setOf("jpg", "jpeg", "png", "gif", "webp", "bmp", "svg", "avif", "ico", "mhtml")
	pageExts  = setOf("html", "htm", "php", "asp", "aspx", "jsp", "cgi")
)

func setOf(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// codec states: yt-dlp says "none" for an absent stream and null when it
// simply doesn't know (every generic <video src> format).
const (
	cNone = iota - 1
	cUnknown
	cPresent
)

func codecState(c string) int {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "none":
		return cNone
	case "", "null", "unknown":
		return cUnknown
	}
	return cPresent
}

// urlExt is the extension of a URL's path ("" when it has none).
func urlExt(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), ".")
}

// fileExt is the extension a format really has: yt-dlp's "unknown_video"
// falls back to the URL's own.
func fileExt(f Format) string {
	ext := strings.ToLower(f.Ext)
	if ext == "" || ext == "unknown_video" {
		return urlExt(f.URL)
	}
	return ext
}

// fileLike reports an extension that is a plain file, not media or a page.
func fileLike(ext string) bool {
	return ext != "" && !audioExts[ext] && !videoExts[ext] && !imageExts[ext] && !pageExts[ext] &&
		ext != "m3u8" && ext != "mpd" && ext != "unknown_video"
}

func manifestProto(p string) bool {
	p = strings.ToLower(p)
	return strings.Contains(p, "m3u8") || strings.Contains(p, "dash") || strings.Contains(p, "ism") || strings.Contains(p, "f4m")
}

var drmNoteRE = regexp.MustCompile(`(?i)fairplay|widevine|playready|sample-aes`)

func classify(f Format) fkind {
	ext := fileExt(f)
	if bool(f.HasDRM) || f.notMedia || drmNoteRE.MatchString(f.ID+" "+f.FormatNote) ||
		strings.EqualFold(f.Protocol, "mhtml") || imageExts[ext] {
		return fkSkip
	}
	v, a := codecState(f.VCodec), codecState(f.ACodec)
	sized := f.Height > 0 || f.Width > 0
	switch {
	case v == cNone && a == cNone:
		return fkSkip
	case v == cPresent && a == cNone:
		return fkVideoOnly
	case v == cPresent:
		return fkMuxed // sound unknown: most such formats have it
	case v == cNone:
		return fkAudio
	case a == cPresent: // picture unknown
		if sized || (videoExts[ext] && !audioExts[ext]) {
			return fkMuxed
		}
		return fkAudio
	}
	// Nothing known about the codecs: go by extension, size and protocol.
	switch {
	case audioExts[ext] && !sized, strings.Contains(strings.ToLower(f.FormatNote), "audio only"):
		return fkAudio
	case sized, videoExts[ext], manifestProto(f.Protocol):
		return fkMuxed
	case ext == "" && strings.HasPrefix(f.URL, "http"):
		return fkMuxed // an extension-less stream URL: let yt-dlp have it
	}
	return fkSkip // an archive, an installer, a document …
}

// formatSize returns a format's byte size — reported, else estimated from its
// bitrate × duration — and whether it is an estimate.
func formatSize(f Format, durationSec float64) (int64, bool) {
	if f.FileSize > 0 {
		return int64(f.FileSize), false
	}
	if f.FileSizeApprox > 0 {
		return int64(f.FileSizeApprox), true
	}
	br := bitrate(f)
	if br > 0 && durationSec > 0 {
		return int64(br * 1000 / 8 * durationSec), true // kbps -> bytes
	}
	return 0, false
}

func bitrate(f Format) float64 {
	if f.TBR > 0 {
		return f.TBR
	}
	return f.VBR + f.ABR
}

// quality is a video's short side ("1080p" for 1920×1080 and for a portrait
// 1080×1920 alike), or its height when only that is known.
func quality(f Format) int {
	w, h := int(f.Width), int(f.Height)
	if w > 0 && h > 0 && w < h {
		return w
	}
	return h
}

// ---- options -----------------------------------------------------------------------

type cand struct {
	f    Format
	kind fkind
	size int64
	est  bool
	q    int
	br   float64
}

func isMP4(c cand) bool { e := fileExt(c.f); return e == "mp4" || e == "m4v" }

// pref ranks formats of the same quality: an MP4 plays everywhere, and H.264
// most of all.
func pref(c cand) int {
	p := 0
	if isMP4(c) {
		p += 2
	}
	if v := strings.ToLower(c.f.VCodec); strings.HasPrefix(v, "avc") || strings.HasPrefix(v, "h264") {
		p++
	}
	return p
}

// directFormat reports a format served as one plain HTTP(S) file. Those are
// fetched in parallel ranges (D BOX's burst path reaches ~100 MB/s on
// googlevideo); a manifest format (HLS/DASH fragments) crawls through hundreds
// of small pieces one after another at a tenth of that. YouTube offers both at
// every height, and its HLS ones carry bitrate-based size estimates far above
// the real files, so without this they won on "size".
func directFormat(c cand) bool { return !manifestProto(strings.ToLower(c.f.Protocol)) }

func outranks(a, b cand) bool {
	if a.q != b.q {
		return a.q > b.q
	}
	if da, db := directFormat(a), directFormat(b); da != db {
		return da
	}
	if pa, pb := pref(a), pref(b); pa != pb {
		return pa > pb
	}
	if a.est != b.est {
		return !a.est // a known size beats a guess
	}
	if a.size != b.size {
		return a.size > b.size
	}
	return a.br > b.br
}

// bestVideo picks what "Best quality" downloads: the highest quality, but an
// MP4 at nearly the same quality wins (archive.org's 240p MP4 over its 304p
// OGV — the same picture to a viewer, and it plays everywhere).
func bestVideo(vids []cand) cand {
	maxQ := 0
	for _, c := range vids {
		if c.q > maxQ {
			maxQ = c.q
		}
	}
	pool := vids
	if maxQ > 0 {
		var mp4s []cand
		for _, c := range vids {
			if isMP4(c) && c.q*4 >= maxQ*3 {
				mp4s = append(mp4s, c)
			}
		}
		if len(mp4s) > 0 {
			pool = mp4s
		}
	}
	best := pool[0]
	for _, c := range pool[1:] {
		if outranks(c, best) {
			best = c
		}
	}
	return best
}

// bestAudio picks the richest audio stream; preferMP3 takes an MP3 at nearly
// the same bitrate (SoundCloud's 128k MP3 over its 96k AAC HLS).
func bestAudio(auds []cand, preferMP3 bool) cand {
	better := func(a, b cand) bool {
		if a.br > 0 && b.br > 0 && a.br != b.br {
			return a.br > b.br
		}
		return a.size > b.size
	}
	best := auds[0]
	for _, c := range auds[1:] {
		if better(c, best) {
			best = c
		}
	}
	if preferMP3 && fileExt(best.f) != "mp3" {
		var mp3 *cand
		for i, c := range auds {
			if fileExt(c.f) == "mp3" && (best.br == 0 || c.br*4 >= best.br*3) && (mp3 == nil || better(c, *mp3)) {
				mp3 = &auds[i]
			}
		}
		if mp3 != nil {
			best = *mp3
		}
	}
	return best
}

// idSafe reports a format id usable verbatim in a -f selector.
func idSafe(id string) bool { return id != "" && !strings.ContainsAny(id, "+/,[]()*? ") }

// videoSelector pins the format that was sized, falling back to yt-dlp's own
// choice if the id is gone at download time.
func videoSelector(c cand, audioAvail bool) string {
	if !idSafe(c.f.ID) {
		return "bv*+ba/b"
	}
	if c.kind == fkVideoOnly && audioAvail {
		return c.f.ID + "+ba/" + c.f.ID + "/bv*+ba/b"
	}
	return c.f.ID + "/bv*+ba/b"
}

// outExt is the container a video candidate ends up in.
func outExt(c cand, audioAvail bool) string {
	if c.kind == fkVideoOnly && audioAvail {
		return "mp4" // --merge-output-format mp4
	}
	if e := fileExt(c.f); videoExts[e] {
		return e
	}
	return "mp4"
}

func sizeSuffix(n int64, est bool) string {
	if n <= 0 {
		return ""
	}
	if est {
		return " (~" + human(n) + ")"
	}
	return " (" + human(n) + ")"
}

func describe(c cand, ext string) string {
	var parts []string
	if c.q > 0 {
		parts = append(parts, fmt.Sprintf("%dp", c.q))
		if c.f.FPS >= 50 {
			parts = append(parts, fmt.Sprintf("%dfps", int(c.f.FPS+0.5)))
		}
	}
	return strings.Join(append(parts, strings.ToUpper(ext)), " ")
}

// distilEntry turns one video's formats into options. direct is the URL of a
// plain file when that is all yt-dlp found.
func distilEntry(in *info) (pe ProbeEntry, direct string) {
	normalize(in)
	dur := in.Duration
	var vids, auds []cand
	for _, f := range in.Formats {
		k := classify(f)
		if k == fkSkip {
			continue
		}
		s, est := formatSize(f, dur)
		c := cand{f: f, kind: k, size: s, est: est, q: quality(f), br: bitrate(f)}
		if k == fkAudio {
			auds = append(auds, c)
		} else {
			vids = append(vids, c)
		}
	}
	pe = ProbeEntry{Title: strings.TrimSpace(in.Title), Duration: dur, Thumbnail: in.Thumbnail, HasVideo: len(vids) > 0}
	switch {
	case len(vids) > 0:
		pe.Options, pe.ad = videoOptions(vids, auds, dur)
	case len(auds) > 0:
		best := bestAudio(auds, true)
		pe.Options = audioOptions(best)
		pe.ad = adURL(best.f.URL)
	default:
		direct = directFile(in)
	}
	if len(pe.Options) > 0 {
		pe.Size, pe.SizeEstimated, pe.Ext = pe.Options[0].Size, pe.Options[0].Estimated, pe.Options[0].Ext
	}
	return pe, direct
}

func videoOptions(vids, auds []cand, dur float64) ([]DownOption, bool) {
	var bestA *cand
	if len(auds) > 0 {
		b := bestAudio(auds, false)
		bestA = &b
	}
	hasSound := bestA != nil
	for i := range vids {
		if vids[i].kind == fkMuxed {
			hasSound = true
		}
		// A video-only stream gets the audio yt-dlp muxes in.
		if vids[i].kind == fkVideoOnly && bestA != nil && vids[i].size > 0 && bestA.size > 0 {
			vids[i].size += bestA.size
			vids[i].est = vids[i].est || bestA.est
		}
	}
	option := func(c cand, label string) DownOption {
		ext := outExt(c, bestA != nil)
		return DownOption{Label: label + sizeSuffix(c.size, c.est), Selector: videoSelector(c, bestA != nil),
			Ext: ext, Size: c.size, Estimated: c.est}
	}
	best := bestVideo(vids)
	opts := []DownOption{option(best, "Best quality – "+describe(best, outExt(best, bestA != nil)))}

	// One option per quality when there is a choice …
	byQ := map[int]cand{}
	var unknown []cand
	for _, c := range vids {
		if c.q <= 0 {
			if c.f.ID != best.f.ID {
				unknown = append(unknown, c)
			}
			continue
		}
		if cur, ok := byQ[c.q]; !ok || outranks(c, cur) {
			byQ[c.q] = c
		}
	}
	if len(byQ) > 1 {
		qs := make([]int, 0, len(byQ))
		for q := range byQ {
			qs = append(qs, q)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(qs)))
		for _, q := range qs {
			c := byQ[q]
			ext := outExt(c, bestA != nil)
			label := fmt.Sprintf("%s - %dp", strings.ToUpper(ext), q)
			if c.f.FPS >= 50 {
				label += fmt.Sprintf(" %dfps", int(c.f.FPS+0.5))
			}
			opts = append(opts, option(c, label))
		}
	}
	// … and each other file of unknown resolution (NASA's MP4 vs its 4 GB ProRes).
	sort.SliceStable(unknown, func(i, j int) bool { return outranks(unknown[i], unknown[j]) })
	for _, c := range unknown {
		opts = append(opts, option(c, strings.ToUpper(outExt(c, bestA != nil))))
	}

	// Audio only: from the audio stream, or extracted from the video when the
	// site has no separate audio (ba/b); hidden when there is no sound at all.
	if hasSound {
		a := DownOption{Label: "Audio only (MP3)", Selector: "ba/b", Ext: "mp3", Audio: true}
		switch {
		case bestA != nil && bestA.size > 0:
			a.Size, a.Estimated = bestA.size, bestA.est
		case dur > 0:
			a.Size, a.Estimated = int64(dur*128000/8), true // ~128 kbps MP3
		}
		a.Label += sizeSuffix(a.Size, a.Estimated)
		opts = append(opts, a)
	}
	return opts, adURL(best.f.URL)
}

// audioOptions serves sound-only media (SoundCloud, Bandcamp, a podcast's mp3):
// no "video" option, the best stream first — as is when it is already MP3.
func audioOptions(best cand) []DownOption {
	ext := fileExt(best.f)
	if !audioExts[ext] {
		ext = "m4a"
	}
	sel := "ba/b"
	if idSafe(best.f.ID) {
		sel = best.f.ID + "/ba/b"
	}
	kind := strings.ToUpper(ext)
	if best.br > 0 {
		kind += fmt.Sprintf(" %dk", int(best.br+0.5))
	}
	if ext == "mp3" {
		// -x keeps an MP3 source as is.
		return []DownOption{{Label: "Audio – " + kind + sizeSuffix(best.size, best.est), Selector: sel, Ext: "mp3",
			Audio: true, Size: best.size, Estimated: best.est}}
	}
	return []DownOption{
		{Label: "Audio – " + kind + sizeSuffix(best.size, best.est), Selector: sel, Ext: ext,
			Size: best.size, Estimated: best.est},
		{Label: "Audio – MP3 (converted)", Selector: "ba/b", Ext: "mp3", Audio: true, Size: best.size, Estimated: true},
	}
}

// directFile finds the plain file a result points at when it has no media at
// all (Dropbox's "video" of a .tar.gz, a generic page whose only link is .exe).
func directFile(in *info) string {
	for _, f := range in.Formats {
		if strings.HasPrefix(f.URL, "http") && !f.notMedia && fileLike(fileExt(f)) {
			return f.URL
		}
	}
	return ""
}

var adRE = regexp.MustCompile(`(?i)(^|[./_-])(doubleclick|googlesyndication|googleads|imasdk|adservice|adsystem|adnxs|` +
	`moatads|springserve|spotx|pubmatic|adserver|adform|vpaid|preroll|pre-roll|advert[a-z]*)([./_-]|$)|/ads?/|[?&]ad_?(id|unit)=`)

func adURL(raw string) bool { return raw != "" && adRE.MatchString(raw) }

// ---- the page as a whole ----------------------------------------------------------

var entryNumberRE = regexp.MustCompile(`\s*\(\d+\)$`)

func buildResult(in *info) *ProbeResult {
	normalize(in)
	res := &ProbeResult{Title: strings.TrimSpace(in.Title), Extractor: in.Extractor, Thumbnail: in.Thumbnail,
		Duration: in.Duration, IsLive: in.IsLive}
	if in.Type != "playlist" && in.Type != "multi_video" && len(in.Entries) == 0 {
		pe, direct := distilEntry(in)
		res.Options, res.HasVideo, res.DirectFile = pe.Options, pe.HasVideo, direct
		return res
	}

	var directs []string
	entries := flatEntries(in.Entries)
	for i, e := range entries {
		pe, direct := distilEntry(e)
		pe.Index = int(e.Index)
		if pe.Index <= 0 {
			pe.Index = i + 1
		}
		// The generic extractor numbers its entries "Page title (1)", "(2)" …
		if strings.EqualFold(e.Extractor, "HTML5MediaEmbed") || strings.EqualFold(e.Extractor, "Generic") {
			pe.Title = strings.TrimSpace(entryNumberRE.ReplaceAllString(pe.Title, ""))
		}
		if pe.Title == "" {
			pe.Title = res.Title
		}
		pe.URL = e.WebpageURL
		if strings.HasPrefix(e.URL, "http") && (pe.URL == "" || pe.URL == in.WebpageURL) {
			pe.URL = e.URL // an embedded file: its own address tells entries apart
		}
		pe.ad = pe.ad || adURL(pe.URL)
		if pe.Thumbnail == "" {
			pe.Thumbnail = in.Thumbnail
		}
		for j := range pe.Options {
			pe.Options[j].PlaylistItem = pe.Index
		}
		res.Entries = append(res.Entries, pe)
		directs = append(directs, direct)
	}
	rec := recommend(res.Entries)
	if rec < 0 {
		// Nothing playable: maybe the page's one link is a plain file.
		for _, d := range directs {
			if d != "" {
				res.DirectFile = d
				break
			}
		}
		return res
	}
	r := res.Entries[rec]
	res.Recommended, res.Options, res.HasVideo = r.Index, r.Options, r.HasVideo
	res.Duration, res.IsLive = r.Duration, res.IsLive || entries[rec].IsLive
	if r.Thumbnail != "" {
		res.Thumbnail = r.Thumbnail
	}
	if res.Title == "" {
		res.Title = r.Title
	}
	return res
}

// flatEntries drops failed (null) entries and lifts a nested playlist's
// entries up one level.
func flatEntries(es []*info) []*info {
	var out []*info
	for _, e := range es {
		switch {
		case e == nil:
		case len(e.Entries) > 0:
			out = append(out, flatEntries(e.Entries)...)
		default:
			out = append(out, e)
		}
	}
	return out
}

// recommend picks the page's main media: one with a picture, not from an ad
// host, not a tiny clip, then the longest, then the largest. Ties keep page
// order. -1 when no entry is playable.
func recommend(es []ProbeEntry) int {
	best := -1
	for i := range es {
		if len(es[i].Options) == 0 {
			continue
		}
		if best < 0 || outscores(es[i], es[best]) {
			best = i
		}
	}
	return best
}

func tinyEntry(e ProbeEntry) bool {
	return (e.Duration > 0 && e.Duration < 10) || (e.Size > 0 && e.Size < 1<<20)
}

func outscores(a, b ProbeEntry) bool {
	if a.HasVideo != b.HasVideo {
		return a.HasVideo
	}
	if a.ad != b.ad {
		return !a.ad
	}
	if ta, tb := tinyEntry(a), tinyEntry(b); ta != tb {
		return !ta
	}
	if a.Duration > 0 && b.Duration > 0 && a.Duration != b.Duration {
		return a.Duration > b.Duration
	}
	return a.Size > b.Size
}

// ---- sizes the extractor didn't report ------------------------------------------

// sizeClient asks servers for sizes; a variable so tests can swap it.
var sizeClient = &http.Client{Timeout: 8 * time.Second}

const (
	sizeProbeMax  = 24 // URLs per probe
	sizeProbeConc = 6
)

// fillSizes asks the servers for the byte size of plain-HTTP formats yt-dlp
// couldn't size (the generic extractor's <video src> files carry no size,
// bitrate or duration), so options show sizes and the main media can be told
// from a 1 MB intro. A URL answered with a web page is marked as not media.
func fillSizes(ctx context.Context, in *info, o Opts) {
	byURL := map[string][]*Format{}
	var order []string
	var visit func(x *info)
	visit = func(x *info) {
		if x == nil {
			return
		}
		for i := range x.Formats {
			f := &x.Formats[i]
			proto := strings.ToLower(f.Protocol)
			if !strings.HasPrefix(f.URL, "http") || (proto != "" && proto != "http" && proto != "https") ||
				f.FileSize > 0 || f.FileSizeApprox > 0 || (bitrate(*f) > 0 && x.Duration > 0) || classify(*f) == fkSkip {
				continue
			}
			if _, seen := byURL[f.URL]; !seen {
				order = append(order, f.URL)
			}
			byURL[f.URL] = append(byURL[f.URL], f)
		}
		for _, e := range x.Entries {
			visit(e)
		}
	}
	visit(in)
	if len(order) > sizeProbeMax {
		order = order[:sizeProbeMax]
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, sizeProbeConc)
	for _, u := range order {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fs := byURL[u]
			size, page := remoteSize(ctx, u, fs[0].HTTPHeaders, o)
			mu.Lock()
			for _, f := range fs {
				if size > 0 {
					f.FileSize = jsonInt(size)
				}
				f.notMedia = page
			}
			mu.Unlock()
		}(u)
	}
	wg.Wait()
}

// remoteSize learns a URL's size from a HEAD, else from a one-byte range GET.
// page reports a successful answer that is a web page, not a file.
func remoteSize(ctx context.Context, rawURL string, hdr map[string]string, o Opts) (size int64, page bool) {
	do := func(method string, ranged bool) *http.Response {
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if req.Header.Get("User-Agent") == "" && o.UserAgent != "" {
			req.Header.Set("User-Agent", o.UserAgent)
		}
		if req.Header.Get("Referer") == "" && o.Referer != "" {
			req.Header.Set("Referer", o.Referer)
		}
		if req.Header.Get("Cookie") == "" {
			if c := CookieHeader(rawURL); c != "" {
				req.Header.Set("Cookie", c)
			}
		}
		if ranged {
			req.Header.Set("Range", "bytes=0-0")
		}
		resp, err := sizeClient.Do(req)
		if err != nil {
			return nil
		}
		resp.Body.Close() // headers are all we need
		return resp
	}
	isPage := func(r *http.Response) bool {
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		return r.StatusCode < 300 && (strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml"))
	}
	if r := do(http.MethodHead, false); r != nil && r.StatusCode < 300 {
		if isPage(r) {
			return 0, true
		}
		if r.ContentLength > 0 {
			return r.ContentLength, false
		}
	}
	r := do(http.MethodGet, true)
	if r == nil {
		return 0, false
	}
	if isPage(r) {
		return 0, true
	}
	if r.StatusCode == http.StatusPartialContent {
		cr := r.Header.Get("Content-Range")
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil {
				return n, false
			}
		}
	}
	if r.StatusCode == http.StatusOK && r.ContentLength > 1 {
		return r.ContentLength, false
	}
	return 0, false
}
