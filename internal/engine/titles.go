package engine

// Title and stream naming heuristics. Page titles and CDN URLs from uncommon
// sites rarely make good file names as they are:
//
//   - "4172q6dt4qljs4kfrvr7 | Byse"        a file-host id plus the site's name
//   - "Watch My Show S01E02 Online Free - SiteName"
//   - "<page title> (1)"                   yt-dlp's generic-extractor entry number
//   - …/hls2/05/06876/qz1cmr88zuzv_x/master.m3u8   nothing but ids and placeholders
//
// cleanTitleFor drops the title segments that name the site or are opaque ids;
// streamNameTier walks title → the URL's own name → the page's slug → a
// meaningful URL folder → "<site> video <date>" (the engine inserts the
// referring page's own <title>/og:title before the slug when it can fetch it).

import (
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"myidm/internal/ytdlp"
)

// titleSeps separate a title from the site name (or a title's parts). Only
// spaced separators count: "AC/DC" and "Spider-Man" are single words.
var titleSeps = []string{" | ", " - ", " – ", " — ", " :: ", " / ", " _ ", " • ", " · ", " » ", " « ", " ~ ", " ｜ "}

type titleSeg struct {
	text string
	sep  string // the separator before this segment ("" for the first)
}

func splitTitle(t string) []titleSeg {
	var out []titleSeg
	sep := ""
	for {
		i, w := -1, ""
		for _, s := range titleSeps {
			if j := strings.Index(t, s); j >= 0 && (i < 0 || j < i) {
				i, w = j, s
			}
		}
		if i < 0 {
			return append(out, titleSeg{strings.TrimSpace(t), sep})
		}
		out = append(out, titleSeg{strings.TrimSpace(t[:i]), sep})
		sep, t = w, t[i+len(w):]
	}
}

// siteWords are words that only ever describe the site, never the content: a
// title segment made of nothing else ("Watch Online", "Free HD Movies",
// "مشاهدة اون لاين") is dropped.
var siteWords = map[string]bool{
	"watch": true, "online": true, "free": true, "hd": true, "fhd": true, "full": true, "download": true,
	"downloads": true, "stream": true, "streaming": true, "streams": true, "movies": true, "films": true,
	"series": true, "tv": true, "video": true, "videos": true, "player": true, "embed": true, "for": true,
	"and": true, "&": true, "in": true, "official": true, "site": true, "website": true, "home": true,
	"شاهد": true, "مشاهدة": true, "تحميل": true, "تنزيل": true, "اون": true, "أون": true, "لاين": true,
	"اونلاين": true, "أونلاين": true, "مباشر": true, "بث": true, "مجانا": true, "مجاناً": true,
	"فيديو": true, "افلام": true, "أفلام": true, "مسلسلات": true, "بجودة": true, "عالية": true, "و": true,
}

// trailSiteWords end a title only as site boilerplate ("… Online Free", "… HD").
var trailSiteWords = map[string]bool{
	"online": true, "free": true, "hd": true, "fhd": true, "لاين": true, "اونلاين": true, "أونلاين": true,
	"مجانا": true, "مجاناً": true, "مباشر": true, "عالية": true,
}

// trailJoiners are dropped together with the site word after them ("for free",
// "in HD", "اون لاين", "بجودة عالية").
var trailJoiners = map[string]bool{"for": true, "in": true, "full": true, "اون": true, "أون": true, "بجودة": true}

// leadSiteWords start a title only as site boilerplate ("شاهد …", "Watch … Online").
var leadSiteWords = map[string]bool{"شاهد": true, "مشاهدة": true, "تحميل": true, "تنزيل": true}
var leadSiteWordsEN = map[string]bool{"watch": true, "download": true}

// siteKeySuffixes may follow a site's name in its own branding ("ArabicToons TV").
var siteKeySuffixes = map[string]bool{"": true, "tv": true, "online": true, "hd": true, "com": true, "net": true,
	"org": true, "official": true, "app": true, "site": true, "live": true, "media": true, "video": true,
	"videos": true, "movies": true, "films": true, "stream": true, "player": true}

// domainRE matches a segment that is itself a domain name ("example.com", "Site.TV").
var domainRE = regexp.MustCompile(`(?i)^(www\.)?[a-z0-9][a-z0-9-]*(\.[a-z0-9-]+)*\.(com|net|org|tv|io|co|me|to|cc|sx|info|xyz|online|site|live|cam|top|ru|fr|de|uk|us|in|pk|eg|sa|ae|ma|dz|tn|iq|biz|club|pro|ws|la|li|lol|fun|vip|app|dev|watch|video|stream|movie|film|so|is|nl|pl|tr|ir|es|it|br|mx|ar|id|my|ph|vn|cn|jp|kr|sh|ly|gg|ai|ms|st|su|nu|re|sk|cz|ro|hu|ch|at|be|se|no|dk|fi|gr|pt|ua|kz|lk|bd|np|ng|ke|za|au|nz|ca|asia|today|world|space|website|store|news|media|link|click|icu|cyou|monster|rest|bar|buzz|work|mobi|name|pw|tk|ml|ga|cf|gq|one|plus|zone|lat|mov|homes|show|tube)$`)

// numberSuffixRE is yt-dlp's generic-extractor entry number: "Title (1)".
var numberSuffixRE = regexp.MustCompile(`\s+\(\d{1,3}\)$`)

// cleanTitleFor normalizes a caller-supplied title for use as a file stem, with
// the download's URLs (media, page) as context: segments naming the site
// ("… | Byse", "EgyDead - …", "… - example.com"), made only of site words
// ("Watch Online Free"), or that are opaque ids are dropped, as are yt-dlp's
// " (1)" entry numbers. Titles with nothing meaningful left give "".
func cleanTitleFor(t string, urls ...string) string {
	t = strings.Join(strings.Fields(t), " ")
	t = strings.Map(func(r rune) rune {
		if invisibleRune(r) {
			return -1
		}
		return r
	}, t)
	t = numberSuffixRE.ReplaceAllString(t, "")
	// A title that is itself a media file name ("clip.mp4") keeps only the stem
	// (but "Learn Node.js" stays whole).
	if stem, ext := splitExt(t); mediaExts[ext] {
		t = stem
	}
	if t == "" || isGenericStem(t) {
		return ""
	}
	keys := siteKeys(urls)
	segs := splitTitle(t)
	var kept []titleSeg
	droppedID, droppedSite := false, false
	for _, s := range segs {
		switch {
		case s.text == "":
		case idLike(s.text):
			droppedID = true
		case isGenericStem(s.text):
		case isSiteSegment(s.text, keys):
			droppedSite = true
		default:
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	// "4172q6dt4qljs4kfrvr7 | Byse": with the id gone, a lone one-word segment
	// is the site's name, not a title.
	if droppedID && len(kept) == 1 && !strings.Contains(kept[0].text, " ") && utf8.RuneCountInString(kept[0].text) <= 20 {
		return ""
	}
	var b strings.Builder
	for i, s := range kept {
		if i > 0 {
			b.WriteString(s.sep)
		}
		b.WriteString(s.text)
	}
	out := trimSiteWords(b.String(), droppedSite)
	if out == "" || isGenericStem(out) || idLike(out) {
		return ""
	}
	return out
}

// trimSiteWords strips site boilerplate from a title's ends: trailing
// "Online", "Free", "HD", "اون لاين"…, leading "شاهد"/"مشاهدة" and — only when
// the end was boilerplate too, so "Watch Dogs 2" survives — a leading "Watch".
func trimSiteWords(t string, droppedSite bool) string {
	words := strings.Fields(t)
	trailed := false
	for len(words) > 1 && trailSiteWords[strings.ToLower(words[len(words)-1])] {
		words = words[:len(words)-1]
		trailed = true
		for len(words) > 1 && trailJoiners[strings.ToLower(words[len(words)-1])] {
			words = words[:len(words)-1]
		}
	}
	for len(words) > 1 && (leadSiteWords[words[0]] || ((trailed || droppedSite) && leadSiteWordsEN[strings.ToLower(words[0])])) {
		words = words[1:]
	}
	// A dangling separator left behind ("Show Name - Watch Online" → "Show Name -").
	for len(words) > 1 && strings.Trim(words[len(words)-1], "-–—|:·•") == "" {
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
}

// siteKeys lists normalized names of the download's sites: each host, its
// registrable domain and that domain's label ("byse" for byse.sx,
// "arabictoons" for www.arabic-toons.com).
func siteKeys(urls []string) map[string]bool {
	keys := map[string]bool{}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || net.ParseIP(u.Hostname()) != nil {
			continue
		}
		host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		keys[normKey(host)] = true
		if d := ytdlp.RegistrableDomain(raw); d != "" {
			keys[normKey(d)] = true
			if label, _, ok := strings.Cut(d, "."); ok && len(label) >= 2 {
				keys[normKey(label)] = true
			}
		}
	}
	return keys
}

// normKey lowercases s and keeps only letters and digits.
func normKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isSiteSegment reports a title segment that names the site rather than the
// content: its host/domain (optionally branded "… TV"), any domain name, or
// only site words.
func isSiteSegment(seg string, keys map[string]bool) bool {
	if domainRE.MatchString(seg) {
		return true
	}
	n := normKey(seg)
	if n == "" || keys[n] {
		return true
	}
	for k := range keys {
		if len(k) >= 4 && strings.HasPrefix(n, k) && siteKeySuffixes[n[len(k):]] {
			return true
		}
	}
	words := strings.Fields(strings.ToLower(seg))
	for _, w := range words {
		if !siteWords[strings.Trim(w, ".,!:")] {
			return false
		}
	}
	return len(words) > 0
}

// idLike reports an opaque identifier used as a name — "4172q6dt4qljs4kfrvr7",
// "pZxWPRg4", "Xk3pQ9zLmN2vB7wR", "qz1cmr88zuzv_x", "1407413685" — as opposed
// to words that merely contain digits ("iPhone15", "Windows11",
// "Tutorial2024Part3", "x264"). Judged on the longest run between '_'/'-';
// the other parts must be short or id-like too.
func idLike(s string) bool {
	if n := len(s); n < 8 || n > 64 || strings.ContainsAny(s, " \t") {
		return false
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' })
	core := ""
	for _, p := range parts {
		if len(p) > len(core) {
			core = p
		}
	}
	if !idCore(core) {
		return false
	}
	for _, p := range parts {
		if p != core && len(p) > 4 && !idCore(p) {
			return false
		}
	}
	return true
}

// idCore scores one separator-free token: 8+ ASCII letters/digits whose
// letters and digits interleave in short runs, or whose letters have almost no
// vowels / flip case constantly — the texture of random ids, not of words.
func idCore(s string) bool {
	if len(s) < 8 {
		return false
	}
	var letters, digits, vowels, flips, caseFlips, runs, runLen int
	prevKind, prevCase := 0, 0 // kind: 1 letter, 2 digit; case: 1 lower, 2 upper
	for _, r := range s {
		kind := 0
		switch {
		case r >= '0' && r <= '9':
			digits++
			kind = 2
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letters++
			kind = 1
			if strings.ContainsRune("aeiouAEIOU", r) {
				vowels++
			}
			c := 1
			if r <= 'Z' {
				c = 2
			}
			if prevKind == 1 && prevCase != 0 && c != prevCase {
				caseFlips++
			}
			prevCase = c
		default:
			return false // not ASCII alphanumeric: a word in some language
		}
		if prevKind != 0 && kind != prevKind {
			flips++
		}
		if kind == 1 && prevKind != 1 {
			runs++
		}
		if kind == 1 {
			runLen++
		}
		prevKind = kind
	}
	switch {
	case letters == 0:
		return digits >= 8 // a long number (1407413685)
	case digits > 0 && flips >= 3 && runs > 0 && float64(runLen)/float64(runs) <= 4.5:
		return true // 4172q6dt4qljs4kfrvr7, dQw4w9WgXcQ, BV1bK411W797
	case letters >= 6 && float64(vowels)/float64(letters) < 0.2 && (digits > 0 || caseFlips >= 3):
		return true // pZxWPRg4
	case digits == 0 && caseFlips >= 5 && float64(vowels)/float64(letters) < 0.35:
		return true // aTCxXKxO
	case digits == 0 && letters >= 10 && float64(vowels)/float64(letters) <= 0.1:
		return true // qljskfrvrzxw
	}
	return false
}

// Stream name tiers, strongest first: where streamNameTier's answer came from.
const (
	nameTierTitle  = iota // the caller's title
	nameTierURL           // the stream URL's own (non-generic) file name
	nameTierSlug          // the referring page's URL slug
	nameTierFolder        // a meaningful folder in the stream URL
	nameTierSite          // "<site> video <date>"
)

// streamNameTier is streamBaseName's chain, also reporting which tier named it
// (the engine may try the referring page's own title before the weak ones).
func streamNameTier(title, rawURL, referer string) (string, int) {
	if t := cleanTitleFor(title, rawURL, referer); t != "" {
		return SanitizeFileName(t), nameTierTitle
	}
	if n := pathName(rawURL); n != "" {
		if stem, _ := splitExt(n); !isGenericStem(stem) && !idLike(stem) {
			return SanitizeFileName(stem), nameTierURL
		}
	}
	if s := refererSlug(referer); s != "" {
		return SanitizeFileName(s), nameTierSlug
	}
	if d := meaningfulFolder(rawURL); d != "" {
		return SanitizeFileName(d), nameTierFolder
	}
	return siteVideoName(referer, rawURL, time.Now()), nameTierSite
}

// siteVideoName is the last-resort name: "<site> video <yyyy-mm-dd hh.mm>"
// (the time keeps unrelated captures from colliding as "(1)", "(2)").
func siteVideoName(referer, rawURL string, now time.Time) string {
	host := hostLabel(referer)
	if host == "" {
		host = hostLabel(rawURL)
	}
	stamp := now.Format("2006-01-02 15.04")
	if host == "" {
		return "video " + stamp
	}
	return SanitizeFileName(host + " video " + stamp)
}

// isSiteVideoName reports whether stem is a siteVideoName for these URLs.
func isSiteVideoName(stem, referer, rawURL string) bool {
	host := hostLabel(referer)
	if host == "" {
		host = hostLabel(rawURL)
	}
	return strings.HasPrefix(stem, SanitizeFileName(host+" video")+" ")
}

// folderSkip are URL folders that say nothing about the content.
var folderSkip = map[string]bool{
	"videos": true, "video": true, "hls": true, "hls2": true, "hls3": true, "dash": true, "vod": true,
	"live": true, "media": true, "content": true, "streams": true, "stream": true, "static": true,
	"assets": true, "files": true, "file": true, "cdn": true, "_definst_": true, "manifests": true,
	"manifest": true, "playlists": true, "playlist": true, "master": true, "variants": true,
	"segments": true, "public": true, "uploads": true, "upload": true, "storage": true, "data": true,
	"out": true, "output": true, "encoded": true, "transcoded": true, "renditions": true, "hd": true,
	"sd": true, "high": true, "low": true, "mid": true, "audio": true, "examples": true, "example": true,
	"streaming": true, "k8s": true, "features": true, "stable": true, "test": true, "tests": true,
	"sample": true, "samples": true, "demo": true, "demos": true, "embed": true, "player": true,
	"mp4": true, "m3u8": true, "hlsv3": true, "hlsv4": true, "vod2": true, "tracks": true, "index": true,
	"api": true, "v1": true, "v2": true, "v3": true, "prod": true, "production": true, "default": true,
	"cmaf": true, "fmp4": true, "ts": true, "adaptive": true, "abr": true, "movies": true, "films": true,
}

// meaningfulFolder returns the innermost folder of rawURL's path that reads as
// a name ("img_bipbop_adv_example_fmp4", "tears-of-steel.ism" →
// "tears-of-steel", "mp4:clip.mp4" → "clip"), skipping placeholders, ids and
// numbers.
func meaningfulFolder(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segs) < 2 {
		return ""
	}
	for i := len(segs) - 2; i >= 0; i-- {
		s := segs[i]
		if dec, err := url.PathUnescape(s); err == nil {
			s = fixEncoding(dec)
		}
		// Wowza/Unified-style prefixes and container suffixes: "smil:x.smil",
		// "mp4:clip.mp4", "movie.ism", "movie.mp4" (nginx-vod /movie.mp4/index.m3u8).
		if _, rest, ok := strings.Cut(s, ":"); ok {
			s = rest
		}
		if stem, ext := splitExt(s); ext != "" && (mediaExts[ext] || ext == "ism" || ext == "isml" || ext == "smil" || ext == "urlset" || ext == "csmil") {
			s = stem
		}
		s = strings.TrimSpace(s)
		low := strings.ToLower(s)
		if folderSkip[low] || isGenericStem(s) || idLike(s) || isNumeric(s) || versionishRE.MatchString(low) {
			continue
		}
		letters := 0
		for _, r := range s {
			if unicode.IsLetter(r) {
				letters++
			}
		}
		if letters >= 3 {
			return s
		}
	}
	return ""
}

// versionishRE matches folder names like "v2", "720p", "1080", "h264", "2024-05".
var versionishRE = regexp.MustCompile(`^(v\d+|\d+p|\d+k|h26[45]|x26[45]|[a-z]?\d+[a-z]?|\d+[-_]\d+([-_]\d+)?)$`)
