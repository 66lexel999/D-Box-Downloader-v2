package engine

// File naming: turn whatever a server, a URL and the caller tell us into a
// clean, meaningful Windows file name. The common failure modes this fixes:
//
//   - CDN redirects to hash-named objects ("…/9f86d081884c7d65") even though the
//     original link was "MyApp-Setup.exe" — we weigh BOTH URLs, not just the last.
//   - Pre-signed S3/GCS/Azure links carry the real name in a query parameter
//     (response-content-disposition=, rscd=, filename=) rather than the path.
//   - Malformed Content-Disposition headers (unquoted names with spaces, raw
//     UTF-8, percent-encoding, RFC 2047 words) that mime.ParseMediaType rejects.
//   - Generic path names ("videoplayback", "index.m3u8", "download.php", "get")
//     and names with no extension at all — fixed up from the caller's title, the
//     page the link came from, the Content-Type, or the file's magic bytes.

import (
	"mime"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// nameHints carries every signal that can name a download.
type nameHints struct {
	Disposition string // Content-Disposition response header
	OrigURL     string // the URL the user/extension gave us
	FinalURL    string // after redirects
	ContentType string // response Content-Type
	Sniff       []byte // first bytes of the body, for magic-byte detection
	Title       string // caller-supplied title (page title, extension hint)
	Referer     string // page the download was started from
}

// maxNameUnits caps a file name's length in UTF-16 code units (what NTFS and
// MAX_PATH count). Leaves room for the download folder plus our ".<id>.part"
// working suffixes inside the classic 260-char path limit.
const maxNameUnits = 150

// resolveFileName picks the best file name for a plain (single-file) download.
func resolveFileName(h nameHints) string {
	detected := detectExt(h.Sniff, h.ContentType)

	name := dispositionName(h.Disposition)
	if name == "" {
		name = queryFileName(h.FinalURL)
	}
	if name == "" {
		name = queryFileName(h.OrigURL)
	}
	if name == "" {
		name = bestPathName(h.OrigURL, h.FinalURL)
	}

	stem, ext := splitExt(name)
	if ext != "" && !meaningfulExt(ext) {
		stem, ext = name, "" // "video.123456": the suffix is part of the name, not a type
	}
	if isGenericStem(stem) {
		if t := cleanTitleFor(h.Title, h.OrigURL, h.FinalURL, h.Referer); t != "" {
			stem = t
		} else if s := refererSlug(h.Referer); s != "" {
			stem = s
		}
	}
	if !meaningfulExt(ext) || scriptExts[ext] || ((ext == "htm" || ext == "html") && detected != "" && detected != "html") {
		if detected != "" {
			ext = detected
		} else if !meaningfulExt(ext) || scriptExts[ext] {
			ext = ""
		}
	}
	if stem == "" {
		stem = "download"
	}
	if ext != "" {
		return SanitizeFileName(stem + "." + ext)
	}
	return SanitizeFileName(stem)
}

// streamBaseName names a stream (HLS/DASH/page video) whose URL is usually
// meaningless ("index.m3u8", "master.m3u8?token=…"). Prefers the caller's
// title (minus site names and ids), then the URL's own name if it isn't
// generic, then the referring page's slug, then a meaningful folder of the URL
// ("img_bipbop_adv_example_fmp4"), then "<site> video <date>". Returns a stem
// WITHOUT extension. (Engine.streamStem also tries the referring page's own
// title before the slug.)
func streamBaseName(title, rawURL, referer string) string {
	name, _ := streamNameTier(title, rawURL, referer)
	return name
}

// ---- Content-Disposition --------------------------------------------------

var (
	cdFilenameStarRE = regexp.MustCompile(`(?i)filename\*\s*=\s*([^;]+)`)
	cdFilenameRE     = regexp.MustCompile(`(?i)(?:^|;)\s*filename\s*=\s*("(?:\\.|[^"\\])*"|[^;]*)`)
)

// dispositionName extracts a file name from a Content-Disposition header,
// tolerating the malformed variants real servers send.
func dispositionName(cd string) string {
	cd = strings.TrimSpace(cd)
	if cd == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(cd); err == nil {
		// mime decodes RFC 2231/5987 filename* into "filename" too.
		if n := pctDecodeName(decodeHeaderWord(params["filename"])); n != "" {
			return baseOf(n)
		}
	}
	// Lenient fallback: filename* (charset''pct-encoded) wins over filename.
	if m := cdFilenameStarRE.FindStringSubmatch(cd); m != nil {
		if n := decodeRFC5987(strings.Trim(strings.TrimSpace(m[1]), `"`)); n != "" {
			return baseOf(n)
		}
	}
	if m := cdFilenameRE.FindStringSubmatch(cd); m != nil {
		v := strings.TrimSpace(m[1])
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = strings.ReplaceAll(v[1:len(v)-1], `\"`, `"`)
		}
		v = pctDecodeName(decodeHeaderWord(v))
		if n := baseOf(v); n != "" {
			return n
		}
	}
	return ""
}

// pctDecodeName decodes a percent-encoded legacy filename ("Setup%20v2.exe"),
// as browsers do, when it contains escapes and no literal spaces.
func pctDecodeName(v string) string {
	if strings.Contains(v, "%") && !strings.Contains(v, " ") {
		if dec, err := url.PathUnescape(v); err == nil {
			return fixEncoding(dec)
		}
	}
	return v
}

// decodeRFC5987 decodes the RFC 5987 ext-value form: charset'lang'%XX-encoded.
func decodeRFC5987(v string) string {
	parts := strings.SplitN(v, "'", 3)
	if len(parts) != 3 {
		if dec, err := url.PathUnescape(v); err == nil {
			return fixEncoding(dec)
		}
		return fixEncoding(v)
	}
	dec, err := url.PathUnescape(parts[2])
	if err != nil {
		return ""
	}
	if strings.EqualFold(parts[0], "iso-8859-1") || strings.EqualFold(parts[0], "latin1") {
		return latin1ToUTF8(dec)
	}
	return fixEncoding(dec)
}

// decodeHeaderWord decodes RFC 2047 "=?UTF-8?B?…?=" words and repairs raw
// Latin-1 bytes some servers put in headers.
func decodeHeaderWord(v string) string {
	v = strings.TrimSpace(v)
	if strings.Contains(v, "=?") {
		if dec, err := new(mime.WordDecoder).DecodeHeader(v); err == nil {
			v = dec
		}
	}
	return fixEncoding(v)
}

// fixEncoding returns s unchanged when it is valid UTF-8, else treats it as
// Latin-1 (what browsers do for non-UTF-8 header bytes).
func fixEncoding(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return latin1ToUTF8(s)
}

func latin1ToUTF8(s string) string {
	r := make([]rune, 0, len(s))
	for i := 0; i < len(s); i++ {
		r = append(r, rune(s[i]))
	}
	return string(r)
}

// ---- URLs -------------------------------------------------------------------

// Query keys that carry a whole Content-Disposition (S3/GCS use the first,
// Azure SAS the second).
var queryDispositionKeys = []string{"response-content-disposition", "rscd", "content-disposition"}

// Query keys that name the file outright.
var queryNameKeys = []string{"filename", "file_name", "fname", "fn", "download", "dl", "attachment", "file", "name", "title"}

// queryFileName finds a file name carried in a URL's query string.
func queryFileName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return ""
	}
	q := u.Query()
	get := func(key string) string {
		for k, vs := range q {
			if strings.EqualFold(k, key) && len(vs) > 0 {
				return strings.TrimSpace(vs[0])
			}
		}
		return ""
	}
	for _, k := range queryDispositionKeys {
		if v := get(k); v != "" {
			if n := dispositionName(v); n != "" {
				return n
			}
		}
	}
	for i, k := range queryNameKeys {
		v := get(k)
		if v == "" || strings.Contains(v, "://") {
			continue
		}
		n := baseOf(fixEncoding(v))
		_, ext := splitExt(n)
		// The first four keys are explicit file-name parameters; the looser ones
		// (file=, name=, title=…) often carry IDs, so require a real extension.
		if n != "" && (i < 4 || meaningfulExt(ext) && knownExt(ext)) {
			return n
		}
	}
	return ""
}

// bestPathName chooses between the original and the post-redirect URL's last
// path segment: whichever carries a known file extension, else the original's
// non-generic name, else the final one.
func bestPathName(orig, final string) string {
	o, f := pathName(orig), pathName(final)
	_, oe := splitExt(o)
	_, fe := splitExt(f)
	switch {
	case knownExt(oe) && !scriptExts[oe]:
		return o
	case knownExt(fe) && !scriptExts[fe]:
		return f
	}
	ostem, _ := splitExt(o)
	fstem, _ := splitExt(f)
	switch {
	case o != "" && !isGenericStem(ostem):
		return o
	case f != "" && !isGenericStem(fstem):
		return f
	case o != "":
		return o
	}
	return f
}

// pathName returns the decoded last path segment of a URL ("" if none).
func pathName(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := u.EscapedPath()
	p = strings.TrimRight(p, "/")
	i := strings.LastIndexByte(p, '/')
	seg := p[i+1:]
	if dec, err := url.PathUnescape(seg); err == nil {
		seg = dec
	}
	seg = fixEncoding(seg)
	if seg == "." || seg == ".." {
		return ""
	}
	return strings.TrimSpace(seg)
}

// refererSlug derives a readable name from the page a download came from:
// "https://site/videos/my-cool-clip-123.html" -> "my-cool-clip-123".
func refererSlug(ref string) string {
	n := pathName(ref)
	if n == "" {
		return ""
	}
	stem, ext := splitExt(n)
	if !scriptExts[ext] && ext != "html" && ext != "htm" && ext != "" {
		stem = n // not a page extension — keep the whole segment
	}
	if isGenericStem(stem) || isNumeric(stem) || slugSkip[strings.ToLower(stem)] {
		return ""
	}
	return strings.TrimSpace(stem)
}

// slugSkip are page paths that name a section of a site, not a video.
var slugSkip = map[string]bool{
	"demo": true, "demos": true, "home": true, "iframe": true, "frame": true, "movie": true, "movies": true,
	"videos": true, "tv": true, "channel": true, "post": true, "page": true, "article": true, "news": true,
	"episode": true, "episodes": true, "series": true, "shows": true, "show": true, "clips": true,
}

// hostLabel returns a URL's host without "www." ("" if unparsable).
func hostLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// ---- heuristics -------------------------------------------------------------

// genericStems are names that say nothing about the content.
var genericStems = map[string]bool{
	"": true, "download": true, "downloads": true, "file": true, "files": true, "get": true,
	"getfile": true, "index": true, "view": true, "stream": true, "video": true, "videos": true,
	"media": true, "content": true, "fetch": true, "dl": true, "attachment": true, "raw": true,
	"blob": true, "master": true, "playlist": true, "chunklist": true, "manifest": true,
	"embed": true, "watch": true, "play": true, "player": true, "api": true, "videoplayback": true,
	"default": true, "main": true, "output": true, "out": true, "data": true, "load": true,
	"serve": true, "redirect": true, "go": true, "link": true, "open": true, "source": true,
	"src": true, "hls": true, "dash": true, "live": true, "audio": true, "track": true,
	"resource": true, "item": true, "object": true, "mp4": true, "m3u8": true, "mpd": true,
	"prog_index": true, "mono": true, "stereo": true, "init": true, "v": true, "d": true,
	// image placeholders (search thumbnails are all "images.jpg") and stream parts
	"images": true, "image": true, "img": true, "photo": true, "photos": true, "picture": true,
	"pic": true, "thumb": true, "thumbs": true, "thumbnail": true, "thumbnails": true, "poster": true,
	"preview": true, "chunk": true, "segment": true, "seg": true, "fragment": true, "frag": true,
	"iframe_index": true, "iframes": true, "rendition": true, "variant": true, "vod": true,
	"unknown_video": true, "untitled": true, "noname": true, "unnamed": true, "null": true,
	"undefined": true, "blank": true, "temp": true, "tmp": true, "e": true, "p": true, "f": true,
}

var (
	hexIDRE  = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	uuidRE   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	tokenRE  = regexp.MustCompile(`^[A-Za-z0-9=]{24,}$`) // no separators: real slugs have hyphens/underscores
	numberRE = regexp.MustCompile(`^[0-9]+$`)
	// Placeholder names with qualifiers: "index-v1-a1", "master_1080p",
	// "chunklist_w905519204", "playlist_b2000000", "video_0", "stream1".
	genericPartsRE = regexp.MustCompile(`^(index|master|playlist|chunklist|manifest|media|stream|video|audio|main|prog_index|seg|segment|hls|dash|mono|stereo|track|rendition|variant|chunk|iframes?|init|source|output|out|file|default|live|vod)([_.-][a-z0-9]{1,16})+$`)
	genericNumRE   = regexp.MustCompile(`^(index|master|playlist|chunklist|stream|video|audio|track|media|main|seg|segment|source|output|out|file|clip|v|p|part)\d+p?$`)
)

// isGenericStem reports whether a name stem is meaningless: empty, a known
// placeholder (also with quality/number qualifiers), or an opaque
// hash/UUID/token/id.
func isGenericStem(stem string) bool {
	stem = strings.TrimSpace(stem)
	s := strings.ToLower(strings.TrimLeft(stem, ".")) // ".m3u8" from Unified Streaming's "x.ism/.m3u8"
	if genericStems[s] {
		return true
	}
	return hexIDRE.MatchString(s) || uuidRE.MatchString(s) ||
		(tokenRE.MatchString(stem) && hasMixedDigits(stem)) ||
		genericPartsRE.MatchString(s) || genericNumRE.MatchString(s) || idLike(stem)
}

func hasMixedDigits(s string) bool {
	var d, l bool
	for _, r := range s {
		d = d || unicode.IsDigit(r)
		l = l || unicode.IsLetter(r)
	}
	return d && l
}

func isNumeric(s string) bool { return numberRE.MatchString(s) }

// scriptExts are server-side script names that are never the real file type.
var scriptExts = map[string]bool{
	"php": true, "asp": true, "aspx": true, "jsp": true, "jspx": true, "cgi": true, "pl": true,
	"do": true, "action": true, "ashx": true, "axd": true, "cfm": true,
}

// meaningfulExt reports whether ext looks like a real file extension: 1-5
// characters, alphanumeric, containing a letter ("mp4", "7z"; not "123456").
func meaningfulExt(ext string) bool {
	if ext == "" || len(ext) > 5 {
		return false
	}
	letter := false
	for _, r := range ext {
		switch {
		case r >= 'a' && r <= 'z':
			letter = true
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return letter
}

// splitExt splits "name.ext" into ("name", "ext"), lowercasing the extension.
func splitExt(name string) (stem, ext string) {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return name, ""
	}
	return name[:i], strings.ToLower(name[i+1:])
}

// baseOf strips any directory components (either slash) from a name.
func baseOf(n string) string {
	n = strings.TrimSpace(strings.ReplaceAll(n, "\\", "/"))
	if i := strings.LastIndexByte(n, '/'); i >= 0 {
		n = n[i+1:]
	}
	if n == "." || n == ".." {
		return ""
	}
	return strings.TrimSpace(n)
}

// cleanTitle normalizes a caller-supplied title for use as a stem; titles that
// are themselves generic ("video", "index"), opaque ids or only site words are
// dropped, and yt-dlp's " (1)" entry numbers are removed. Callers that know the
// download's URLs should use cleanTitleFor, which also drops the site's own
// name ("… | Byse").
func cleanTitle(t string) string {
	return cleanTitleFor(t)
}

var mediaExts = map[string]bool{
	"mp4": true, "mkv": true, "webm": true, "mov": true, "avi": true, "flv": true, "m4v": true,
	"ts": true, "m3u8": true, "mpd": true, "mp3": true, "m4a": true, "aac": true, "wav": true,
	"flac": true, "ogg": true, "opus": true,
}

// ---- sanitizing -------------------------------------------------------------

// windowsReserved are device names Windows refuses as file stems.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// invisibleRune reports format characters that can disguise a name — most
// importantly the right-to-left override used to make "gnp.exe" read as
// "exe.png" — plus zero-width characters and the BOM.
func invisibleRune(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, // zero-width space/joiners, LRM/RLM
		r >= 0x202A && r <= 0x202E, // bidi embeddings/overrides
		r >= 0x2066 && r <= 0x2069, // bidi isolates
		r == 0xFEFF, r == 0x00AD:
		return true
	}
	return false
}

// SanitizeFileName makes a name safe for NTFS: strips the directory part of a
// real path, replaces characters Windows rejects, removes invisible/bidi-
// override characters, collapses whitespace, avoids reserved device names, and
// caps the length (keeping the extension). A title's "/" or "\" is text, not a
// path ("AC/DC", bandcamp's `a "/\ b`) — it becomes "_" and nothing is cut
// away; a separator between words reads as a dash ("Title: Sub" and "A | B"
// become "Title - Sub" and "A - B").
func SanitizeFileName(name string) string {
	name = strings.ToValidUTF8(name, "")
	if looksLikePath(name) {
		name = baseOf(name)
	}
	rs := []rune(name)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		case strings.ContainsRune(`:/\|`, r) && i+1 < len(rs) && rs[i+1] == ' ':
			b.WriteString(" -")
		case strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteByte('_')
		case invisibleRune(r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	out = strings.Trim(out, " .")
	if out == "" {
		return "download"
	}
	stem, ext := out, ""
	if i := strings.LastIndexByte(out, '.'); i > 0 && len(out)-i <= 11 {
		stem, ext = out[:i], out[i:]
	}
	if windowsReserved[strings.ToUpper(strings.TrimSpace(stem))] {
		stem = "_" + stem
	}
	stem = truncateUTF16(stem, maxNameUnits-utf16Len(ext))
	stem = strings.TrimRight(stem, " .")
	if stem == "" {
		stem = "download"
	}
	return stem + ext
}

// looksLikePath reports a name that is really a file path — a drive or UNC
// root, or "."/".." segments (`..\..\evil.exe`) — rather than a title that
// merely contains slashes.
func looksLikePath(n string) bool {
	n = strings.TrimSpace(n)
	if len(n) >= 3 && n[1] == ':' && (n[2] == '\\' || n[2] == '/') &&
		((n[0] >= 'a' && n[0] <= 'z') || (n[0] >= 'A' && n[0] <= 'Z')) {
		return true
	}
	if strings.HasPrefix(n, `\\`) || strings.HasPrefix(n, "//") {
		return true
	}
	segs := strings.Split(strings.ReplaceAll(n, `\`, "/"), "/")
	for _, s := range segs[:len(segs)-1] {
		if s == "." || s == ".." {
			return true
		}
	}
	return false
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// truncateUTF16 cuts s to at most max UTF-16 code units without splitting a rune.
func truncateUTF16(s string, max int) string {
	if max < 1 {
		max = 1
	}
	n := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if n+w > max {
			return s[:i]
		}
		n += w
	}
	return s
}
