package engine

// Link Grabber, part 1: what on this page could be downloaded?
//
// extractCandidates reads a page's HTML (no JavaScript runs) and lists every
// address that looks like a file, a media stream or an embedded video, with
// whatever name the page gives it. It is deliberately greedy: the grabber
// probes each candidate afterwards and drops the ones that turn out to be web
// pages, so a false positive costs one small request while a miss costs the
// user the file. Where candidates come from, roughly in order of trust:
//
//   - tags that exist to carry media: <video>/<audio>/<source>/<track>, <img>
//     (srcset and the lazy-loading data-src/data-original/… attributes),
//     <embed>/<object>, and <iframe>s (known video sites go to yt-dlp, other
//     frames are read one level deep);
//   - links: <a href> with a file extension, a download attribute or the shape
//     of a download endpoint (/download, ?dl=1, export=download …), GET forms
//     that lead to a download (Google Drive's "can't scan for viruses" page),
//     <meta http-equiv=refresh> countdown pages;
//   - metadata: og:/twitter: meta tags, itemprop, JSON-LD Video/Audio/Image
//     objects (their "name" is the best title a page gives a file);
//   - player configs and inline scripts: data-* attributes, Video.js
//     data-setup, "file:"/"src:"/"hls:" keys and any absolute or JSON-escaped
//     URL ending in a media/file extension;
//   - host hints: a Mux thumbnail implies its stream.mux.com playlist, a
//     lite-youtube id its YouTube video, a JW Player id its manifest, …
//
// Junk (tracking pixels, icons and logos, analytics beacons, fonts, UI sounds)
// is dropped here so it never costs a probe.
//
// This is a small purpose-built tag scanner rather than an HTML5 parser: it
// only needs start tags with their attributes, a few end tags, link text and
// the raw bodies of <script>/<title>, and it must never fail on broken markup.

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Candidate sources — the item's "source" tag in the grabber's list.
const (
	grabSrcDirect = "direct"
	grabSrcYtdlp  = "ytdlp"
	grabSrcVideo  = "video-tag"
	grabSrcAudio  = "audio-tag"
	grabSrcSource = "source-tag"
	grabSrcImg    = "img"
	grabSrcLink   = "link"
	grabSrcMeta   = "meta"
	grabSrcJSONLD = "jsonld"
	grabSrcScript = "script"
	grabSrcIframe = "iframe"
	grabSrcPlayer = "player-config"
	grabSrcText   = "text"
)

// grabSourceRank orders sources by how sure they are that a URL is a real,
// intended download; merging a duplicate keeps the stronger label.
var grabSourceRank = map[string]int{
	grabSrcVideo: 7, grabSrcAudio: 7, grabSrcSource: 7, grabSrcLink: 6, grabSrcJSONLD: 6, grabSrcIframe: 5,
	grabSrcMeta: 5, grabSrcImg: 4, grabSrcPlayer: 3, grabSrcScript: 2, grabSrcText: 1,
}

// grabCand is one address found on a page.
type grabCand struct {
	URL      string
	Name     string // name hint: <a download>, JSON-LD name, link text, track label
	Source   string
	Kind     string // what the page says it is: video, audio, image, subtitle, stream ("" = by extension)
	Width    int    // from width/height attributes (0 = unknown)
	Height   int
	Thumb    string // poster / thumbnailUrl
	Duration float64
	Embed    bool // a known video site's player or page: yt-dlp extracts it
	Frame    bool // an unknown sub-document (iframe, include-fragment, og:video page): read it once
	Linked   bool // offered to the user as a link or form: any file type counts
	Follow   bool // a "Download" button to a landing page: if it answers with HTML, read that page once
	nameQ    int  // how trustworthy Name is (3 download attr … 1 link text)
}

// pageScan is extractCandidates' answer.
type pageScan struct {
	Title   string // <title>
	OGTitle string // og:title / twitter:title
	Cands   []grabCand
}

// extractCandidates lists the downloadable-looking addresses in doc, a page
// fetched from baseURL, in page order and without duplicates.
func extractCandidates(baseURL, doc string) pageScan {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return pageScan{}
	}
	x := &grabExtractor{base: base, page: base, index: map[string]int{}}
	x.queryMedia(base.String(), grabSrcPlayer)
	x.hostHints()
	x.scan(doc, 0)
	x.finishAnchor()
	x.finishForm()
	// Whole-document passes: URLs in text and attributes no tag rule covers
	// (code samples, inline styles, templates), then player hints.
	x.scanScript(html.UnescapeString(doc), grabSrcText)
	x.playerHints(doc)
	return pageScan{Title: x.title, OGTitle: x.ogTitle, Cands: x.out}
}

// extractTextLinks pulls http(s) links out of pasted text or HTML the way the
// old "Grab links" box did: href attributes first (copied HTML often shows
// only file NAMES while the address lives in the anchor), then bare URLs.
func extractTextLinks(text string) []string {
	seen := map[string]bool{}
	var out []string
	push := func(u string) {
		u = strings.ReplaceAll(u, "&amp;", "&")
		u = strings.TrimRight(u, `.,;:!?'")]`)
		if !grabHTTPRE.MatchString(u) || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, m := range grabHrefRE.FindAllStringSubmatch(text, -1) {
		push(m[1])
	}
	for _, m := range grabBareURLRE.FindAllString(text, -1) {
		push(m)
	}
	return out
}

var (
	grabHTTPRE    = regexp.MustCompile(`(?i)^https?://[^\s/]+`)
	grabHrefRE    = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
	grabBareURLRE = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
)

// ---- the scanner --------------------------------------------------------------

type grabExtractor struct {
	base    *url.URL // resolves relative links (<base href> may change it)
	page    *url.URL // the page itself
	baseSet bool
	out     []grabCand
	index   map[string]int // dedup key -> position in out

	title, ogTitle string
	svgDepth       int    // <title> inside inline SVG is not the page title
	media          string // "video"/"audio" while inside that element (for <source>)
	inPicture      bool
	anchor         *grabAnchor
	form           *grabForm
}

type grabAnchor struct {
	href, dlName string
	hasDownload  bool   // download attribute present
	buttonish    bool   // id/class/aria-label says "download" (a landing-page button)
	text         []byte // visible link text
}

type grabForm struct {
	action string
	params url.Values
}

// grabRawTags hold text, not markup, up to their end tag.
var grabRawTags = map[string]bool{"script": true, "style": true, "title": true, "textarea": true, "noscript": true, "xmp": true}

func (x *grabExtractor) scan(doc string, depth int) {
	lower := grabLower(doc) // same length as doc: indexes line up
	n := len(doc)
	for i := 0; i < n; {
		lt := strings.IndexByte(doc[i:], '<')
		if lt < 0 {
			x.text(doc[i:])
			return
		}
		if lt > 0 {
			x.text(doc[i : i+lt])
		}
		i += lt
		switch {
		case strings.HasPrefix(doc[i:], "<!--"):
			end := strings.Index(doc[i+4:], "-->")
			if end < 0 {
				return
			}
			i += 4 + end + 3
		case i+1 < n && (doc[i+1] == '!' || doc[i+1] == '?'):
			end := strings.IndexByte(doc[i:], '>')
			if end < 0 {
				return
			}
			i += end + 1
		case i+1 < n && doc[i+1] == '/':
			name, j := grabTagName(doc, i+2)
			end := strings.IndexByte(doc[j:], '>')
			if end < 0 {
				return
			}
			i = j + end + 1
			if name != "" {
				x.endTag(name)
			}
		case i+1 < n && grabIsLetter(doc[i+1]):
			name, attrs, next, selfClose := grabStartTag(doc, i+1)
			i = next
			x.startTag(name, attrs)
			if grabRawTags[name] && !selfClose {
				body := doc[i:]
				if end := strings.Index(lower[i:], "</"+name); end >= 0 {
					body = doc[i : i+end]
				}
				i += len(body) // the loop then reads the end tag
				x.rawText(name, attrs, body, depth)
			}
		default:
			x.text("<")
			i++
		}
	}
}

// grabTagName reads a tag name at doc[i:], lowercased.
func grabTagName(doc string, i int) (string, int) {
	j := i
	for j < len(doc) && (grabIsLetter(doc[j]) || doc[j] >= '0' && doc[j] <= '9' || doc[j] == '-' || doc[j] == ':' || doc[j] == '_') {
		j++
	}
	return grabLower(doc[i:j]), j
}

// grabStartTag parses "<name attr=… …>" starting after the '<'. Attribute
// values are entity-decoded; the first of duplicate attributes wins.
func grabStartTag(doc string, i int) (name string, attrs map[string]string, next int, selfClose bool) {
	name, j := grabTagName(doc, i)
	attrs = map[string]string{}
	n := len(doc)
	for j < n {
		for j < n && grabIsSpace(doc[j]) {
			j++
		}
		if j >= n {
			break
		}
		switch doc[j] {
		case '>':
			return name, attrs, j + 1, false
		case '/':
			if j+1 < n && doc[j+1] == '>' {
				return name, attrs, j + 2, true
			}
			j++
			continue
		}
		start := j
		for j < n && !grabIsSpace(doc[j]) && doc[j] != '=' && doc[j] != '>' && !(doc[j] == '/' && j+1 < n && doc[j+1] == '>') {
			j++
		}
		if j == start {
			j++
			continue
		}
		aname := grabLower(doc[start:j])
		for j < n && grabIsSpace(doc[j]) {
			j++
		}
		val := ""
		if j < n && doc[j] == '=' {
			j++
			for j < n && grabIsSpace(doc[j]) {
				j++
			}
			if j < n && (doc[j] == '"' || doc[j] == '\'') {
				q := doc[j]
				end := strings.IndexByte(doc[j+1:], q)
				if end < 0 {
					val, j = doc[j+1:], n
				} else {
					val, j = doc[j+1:j+1+end], j+1+end+1
				}
			} else {
				s := j
				for j < n && !grabIsSpace(doc[j]) && doc[j] != '>' {
					j++
				}
				val = doc[s:j]
			}
		}
		if _, dup := attrs[aname]; !dup {
			attrs[aname] = html.UnescapeString(val)
		}
	}
	return name, attrs, n, false
}

func (x *grabExtractor) text(s string) {
	if x.anchor != nil && len(x.anchor.text) < 400 {
		x.anchor.text = append(x.anchor.text, s...)
		x.anchor.text = append(x.anchor.text, ' ')
	}
}

func (x *grabExtractor) startTag(name string, a map[string]string) {
	switch name {
	case "base":
		if !x.baseSet {
			if u, err := x.page.Parse(strings.TrimSpace(a["href"])); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
				x.base, x.baseSet = u, true
			}
		}
	case "a", "area":
		x.finishAnchor()
		_, hasDL := a["download"]
		label := strings.ToLower(a["id"] + " " + a["class"] + " " + a["aria-label"] + " " + a["title"])
		x.anchor = &grabAnchor{href: a["href"], dlName: strings.TrimSpace(a["download"]), hasDownload: hasDL,
			buttonish: strings.Contains(label, "download") || strings.Contains(label, "mirror")}
	case "video", "audio":
		x.media = name
		src := grabSrcVideo
		if name == "audio" {
			src = grabSrcAudio
		}
		for _, k := range []string{"src", "data-src"} {
			x.add(grabCand{URL: a[k], Source: src, Kind: name, Name: a["title"], nameQ: 1})
		}
		if p := grabFirst(a["poster"], a["data-poster"]); p != "" {
			x.add(grabCand{URL: p, Source: src, Kind: "image"})
		}
	case "source":
		if x.inPicture {
			x.add(grabCand{URL: grabLargestSrcset(grabFirst(a["srcset"], a["data-srcset"])), Source: grabSrcImg, Kind: "image"})
			break
		}
		kind := x.media
		if t := strings.ToLower(a["type"]); strings.Contains(t, "mpegurl") || strings.Contains(t, "dash+xml") {
			kind = "stream"
		} else if kind == "" && strings.HasPrefix(t, "video/") {
			kind = "video"
		} else if kind == "" && strings.HasPrefix(t, "audio/") {
			kind = "audio"
		}
		for _, k := range []string{"src", "data-src"} {
			x.add(grabCand{URL: a[k], Source: grabSrcSource, Kind: kind})
		}
	case "track":
		label := strings.TrimSpace(grabFirst(a["label"], a["srclang"]))
		x.add(grabCand{URL: a["src"], Source: grabSrcSource, Kind: "subtitle", Name: label, nameQ: 1})
	case "picture":
		x.inPicture = true
	case "img":
		w, h := grabAtoi(a["width"]), grabAtoi(a["height"])
		srcset := grabLargestSrcset(grabFirst(a["srcset"], a["data-srcset"], a["data-lazy-srcset"]))
		lazy := grabFirst(a["data-src"], a["data-original"], a["data-lazy-src"], a["data-lazy"], a["data-url"], a["data-full"], a["data-large-file"], a["data-orig-file"])
		for _, u := range []string{srcset, lazy} {
			x.add(grabCand{URL: u, Source: grabSrcImg, Kind: "image", Width: w, Height: h})
		}
		if srcset == "" && lazy == "" {
			x.add(grabCand{URL: a["src"], Source: grabSrcImg, Kind: "image", Width: w, Height: h})
		}
	case "iframe", "frame":
		src := strings.TrimSpace(a["src"])
		if src == "" || strings.HasPrefix(strings.ToLower(src), "about:") || strings.HasPrefix(strings.ToLower(src), "javascript:") {
			src = grabFirst(a["data-src"], a["data-lazy-src"], a["data-url"])
		}
		x.addFrame(src, grabSrcIframe, a["title"])
	case "embed":
		x.addFrame(a["src"], grabSrcIframe, a["title"])
	case "object":
		x.addFrame(a["data"], grabSrcIframe, a["title"])
	case "param":
		switch strings.ToLower(a["name"]) {
		case "movie", "src", "url", "file", "filename":
			x.addFrame(a["value"], grabSrcIframe, "")
		}
	case "link":
		x.link(a)
	case "meta":
		x.meta(a)
	case "form":
		x.finishForm()
		if m := strings.ToLower(strings.TrimSpace(a["method"])); m == "" || m == "get" {
			x.form = &grabForm{action: a["action"], params: url.Values{}}
		}
	case "input":
		if x.form != nil && a["name"] != "" {
			switch strings.ToLower(a["type"]) {
			case "hidden", "text", "":
				x.form.params.Add(a["name"], a["value"])
			}
		}
	case "include-fragment": // GitHub loads a release's asset list this way
		x.addFrame(a["src"], grabSrcLink, "")
	case "lite-youtube", "lite-vimeo":
		if id := strings.TrimSpace(a["videoid"]); id != "" {
			u := "https://www.youtube.com/watch?v=" + url.QueryEscape(id)
			if name == "lite-vimeo" {
				u = "https://vimeo.com/" + url.PathEscape(id)
			}
			x.add(grabCand{URL: u, Source: grabSrcPlayer, Embed: true, Name: a["videotitle"], nameQ: 1})
		}
	case "video-js": // Brightcove's player tag
		if acct, vid := a["data-account"], a["data-video-id"]; acct != "" && vid != "" {
			player := grabFirst(a["data-player"], "default")
			x.add(grabCand{URL: "https://players.brightcove.net/" + url.PathEscape(acct) + "/" + url.PathEscape(player) +
				"_default/index.html?videoId=" + url.QueryEscape(vid), Source: grabSrcPlayer, Embed: true})
		}
	case "svg":
		x.svgDepth++
	}
	x.dataAttrs(name, a)
}

func (x *grabExtractor) endTag(name string) {
	switch name {
	case "a", "area":
		x.finishAnchor()
	case "video", "audio":
		x.media = ""
	case "picture":
		x.inPicture = false
	case "svg":
		if x.svgDepth > 0 {
			x.svgDepth--
		}
	case "form":
		x.finishForm()
	}
}

func (x *grabExtractor) rawText(name string, a map[string]string, body string, depth int) {
	switch name {
	case "title":
		if x.svgDepth == 0 && x.title == "" {
			x.title = grabCollapse(html.UnescapeString(body))
		}
	case "script":
		t := strings.ToLower(strings.TrimSpace(a["type"]))
		if strings.Contains(t, "ld+json") {
			x.jsonLD(body)
		}
		if t == "" || strings.Contains(t, "javascript") || strings.Contains(t, "json") || t == "module" {
			x.scanScript(body, grabSrcScript)
		}
	case "noscript":
		// Lazy-loading libraries keep the real <img>/<video> in <noscript>.
		if depth < 2 {
			x.scan(body, depth+1)
		}
	}
}

// finishAnchor decides whether the link that just closed is a download.
func (x *grabExtractor) finishAnchor() {
	a := x.anchor
	if a == nil {
		return
	}
	x.anchor = nil
	u := x.resolve(a.href)
	if u == "" {
		return
	}
	pu, err := url.Parse(u)
	if err != nil {
		return
	}
	text := grabCollapse(string(a.text))
	name, q := a.dlName, 3
	if name == "" && grabUsefulText(text) {
		name, q = text, 1
	}
	_, ext := splitExt(pathName(u))
	switch {
	case grabFileExt[ext] || a.hasDownload:
		x.add(grabCand{URL: u, Source: grabSrcLink, Name: name, nameQ: q, Linked: true})
	case isGrabVideoHost(pu.Hostname()):
		// A plain link to a video site's page is navigation, not this page's
		// content (the iframe rule catches embedded players).
	case grabDownloadEndpoint(pu):
		follow := a.buttonish || strings.Contains(strings.ToLower(text), "download") || strings.Contains(strings.ToLower(text), "mirror")
		x.add(grabCand{URL: u, Source: grabSrcLink, Name: name, nameQ: q, Linked: true, Follow: follow && grabSameSite(pu, x.page)})
	}
	x.queryMedia(u, grabSrcLink)
}

// finishForm turns a GET form that leads to a download into a link (Google
// Drive's "Download anyway" form carries id/export/confirm as hidden inputs).
func (x *grabExtractor) finishForm() {
	f := x.form
	if f == nil {
		return
	}
	x.form = nil
	u := x.resolve(f.action)
	if u == "" {
		return
	}
	pu, err := url.Parse(u)
	if err != nil || !grabDownloadEndpoint(pu) {
		return
	}
	q := pu.Query()
	for k, vs := range f.params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	pu.RawQuery = q.Encode()
	x.add(grabCand{URL: pu.String(), Source: grabSrcLink, Linked: true})
}

func (x *grabExtractor) link(a map[string]string) {
	rel := " " + strings.ToLower(a["rel"]) + " "
	href := a["href"]
	switch {
	case strings.Contains(rel, " icon ") || strings.Contains(rel, "apple-touch") || strings.Contains(rel, " stylesheet "):
	case strings.Contains(rel, " preload ") || strings.Contains(rel, " prefetch "):
		switch as := strings.ToLower(a["as"]); as {
		case "video", "audio", "image":
			x.add(grabCand{URL: href, Source: grabSrcMeta, Kind: as})
		case "fetch":
			if k := grabMediaKind(href); k != "" {
				x.add(grabCand{URL: href, Source: grabSrcMeta, Kind: k})
			}
		}
	case strings.Contains(rel, " image_src "):
		x.add(grabCand{URL: href, Source: grabSrcMeta, Kind: "image"})
	case strings.Contains(rel, " enclosure "):
		x.add(grabCand{URL: href, Source: grabSrcLink, Linked: true})
	}
	switch strings.ToLower(a["itemprop"]) {
	case "contenturl":
		x.add(grabCand{URL: href, Source: grabSrcMeta})
	case "embedurl":
		x.addFrame(href, grabSrcMeta, "")
	case "thumbnailurl":
		x.add(grabCand{URL: href, Source: grabSrcMeta, Kind: "image"})
	}
}

func (x *grabExtractor) meta(a map[string]string) {
	key := strings.ToLower(strings.TrimSpace(grabFirst(a["property"], a["name"], a["itemprop"])))
	content := strings.TrimSpace(a["content"])
	if strings.EqualFold(a["http-equiv"], "refresh") {
		// "5;URL='https://mirror/…/file.exe'" — a download countdown page.
		if i := strings.Index(strings.ToLower(content), "url="); i >= 0 {
			x.add(grabCand{URL: strings.Trim(strings.TrimSpace(content[i+4:]), `'"`), Source: grabSrcMeta, Linked: true})
		}
		return
	}
	if content == "" {
		return
	}
	switch key {
	case "og:title", "twitter:title":
		if x.ogTitle == "" {
			x.ogTitle = grabCollapse(content)
		}
	case "og:video", "og:video:url", "og:video:secure_url", "twitter:player:stream":
		if grabMediaKind(content) != "" {
			x.add(grabCand{URL: content, Source: grabSrcMeta, Kind: "video"})
		} else {
			x.addFrame(content, grabSrcMeta, "")
		}
	case "twitter:player", "embedurl":
		x.addFrame(content, grabSrcMeta, "")
	case "og:audio", "og:audio:url", "og:audio:secure_url":
		x.add(grabCand{URL: content, Source: grabSrcMeta, Kind: "audio"})
	case "og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src", "thumbnailurl":
		x.add(grabCand{URL: content, Source: grabSrcMeta, Kind: "image"})
	case "contenturl":
		x.add(grabCand{URL: content, Source: grabSrcMeta})
	}
}

// addFrame files an embedded document: a known video site's player goes to
// yt-dlp, a direct media URL is a candidate, anything else is read once.
func (x *grabExtractor) addFrame(ref, source, title string) {
	u := x.resolve(ref)
	if u == "" {
		return
	}
	pu, err := url.Parse(u)
	if err != nil {
		return
	}
	switch {
	case grabMediaKind(u) != "":
		x.add(grabCand{URL: u, Source: source})
	case isGrabVideoHost(pu.Hostname()):
		x.add(grabCand{URL: u, Source: source, Embed: true, Name: title, nameQ: 1})
	case grabJunkFrame(pu):
	default:
		x.add(grabCand{URL: u, Source: source, Frame: true})
	}
}

// dataAttrs mines data-* attributes: player configs (Video.js data-setup and
// other JSON blobs) and any attribute holding a media/file address.
func (x *grabExtractor) dataAttrs(tag string, a map[string]string) {
	keys := make([]string, 0, len(a))
	for k := range a {
		if strings.HasPrefix(k, "data-") && a[k] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // map order would shuffle the grabber's list between runs
	for _, k := range keys {
		v := a[k]
		v = strings.TrimSpace(v)
		switch {
		case strings.HasPrefix(v, "{") || strings.HasPrefix(v, "["):
			x.scanScript(v, grabSrcPlayer)
		case k == "data-youtube-id" || k == "data-ytid" || k == "data-yt-id" || k == "data-youtube":
			if grabYTIDRE.MatchString(v) {
				x.add(grabCand{URL: "https://www.youtube.com/watch?v=" + v, Source: grabSrcPlayer, Embed: true})
			}
		case tag == "img" || tag == "source" || tag == "iframe":
			// handled by the tag rules (lazy-loading attributes)
		case strings.ContainsAny(v, " \t\n") || !strings.Contains(v, "/"):
			// Not an address: a bare "Clip.webm" is a title (MediaWiki's
			// data-mwtitle) and would resolve to a page on this site.
		default:
			kind := grabMediaKind(v)
			if kind == "" && (strings.Contains(k, "hls") || strings.Contains(k, "m3u8") || strings.Contains(k, "dash") || strings.Contains(k, "stream")) &&
				(strings.HasPrefix(v, "http") || strings.HasPrefix(v, "/")) {
				kind = "stream"
			}
			if kind == "" {
				_, ext := splitExt(pathName(v))
				if !grabFileExt[ext] {
					continue
				}
			}
			x.add(grabCand{URL: v, Source: grabSrcPlayer, Kind: kind})
		}
	}
}

// ---- JSON-LD ------------------------------------------------------------------

func (x *grabExtractor) jsonLD(body string) {
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &v); err != nil {
		return // broken JSON-LD is common; the script scan still sees its URLs
	}
	x.walkLD(v, 0)
}

func (x *grabExtractor) walkLD(v any, depth int) {
	if depth > 12 {
		return
	}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			x.walkLD(e, depth+1)
		}
	case map[string]any:
		kind, media := "", false
		for _, typ := range grabLDStrings(t["@type"]) {
			switch strings.ToLower(typ) {
			case "videoobject", "movie", "clip", "episode", "tvepisode", "movieclip", "videogame trailer":
				kind, media = "video", true
			case "audioobject", "musicrecording", "podcastepisode", "audiobook":
				kind, media = "audio", true
			case "imageobject", "photograph":
				kind, media = "image", true
			case "mediaobject", "datadownload":
				media = true
			}
		}
		if media {
			name := grabCollapse(grabFirst(grabLDString(t["name"]), grabLDString(t["headline"])))
			dur := grabISODuration(grabLDString(t["duration"]))
			thumbs := grabLDURLs(t["thumbnailUrl"])
			if len(thumbs) == 0 {
				thumbs = grabLDURLs(t["thumbnail"])
			}
			thumb := ""
			if len(thumbs) > 0 {
				thumb = x.resolve(thumbs[0])
			}
			content := grabLDURLs(t["contentUrl"])
			if len(content) == 0 && (kind == "image" || grabMediaKind(grabLDString(t["url"])) != "") {
				content = grabLDURLs(t["url"])
			}
			for _, u := range content {
				x.add(grabCand{URL: u, Source: grabSrcJSONLD, Kind: kind, Name: name, nameQ: 2, Duration: dur, Thumb: thumb})
			}
			for _, u := range grabLDURLs(t["embedUrl"]) {
				x.addFrame(u, grabSrcJSONLD, name)
			}
			for _, u := range thumbs {
				x.add(grabCand{URL: u, Source: grabSrcJSONLD, Kind: "image"})
			}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic order
		for _, k := range keys {
			if k != "@context" {
				x.walkLD(t[k], depth+1)
			}
		}
	}
}

func grabLDString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		if len(t) > 0 {
			return grabLDString(t[0])
		}
	}
	return ""
}

func grabLDStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// grabLDURLs reads a URL-valued property: a string, a list, or an object with url.
func grabLDURLs(v any) []string {
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, grabLDURLs(e)...)
		}
		return out
	case map[string]any:
		for _, k := range []string{"contentUrl", "url", "@id"} {
			if s := grabLDString(t[k]); s != "" {
				return []string{s}
			}
		}
	}
	return nil
}

var grabISODurRE = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// grabISODuration reads schema.org durations like "PT1H2M3S" (0 = unknown).
func grabISODuration(s string) float64 {
	m := grabISODurRE.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0
	}
	f := func(i int) float64 { v, _ := strconv.ParseFloat(m[i], 64); return v }
	return f(1)*86400 + f(2)*3600 + f(3)*60 + f(4)
}

// ---- scripts and player configs -------------------------------------------------

var (
	// grabAbsURLRE finds absolute (or protocol-relative) URLs in script text.
	grabAbsURLRE = regexp.MustCompile(`(?i)(?:https?:)?//[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)+(?::\d+)?/[^\s"'<>\\` + "`" + `{}|^]*`)
	// grabPlayerKeyRE finds player-config values: file: "…", "hlsUrl":"…", src='…'.
	grabPlayerKeyRE = regexp.MustCompile(`(?i)["']?\b(file|src|source|url|hls|hlsurl|hls_url|hlsmanifesturl|dash|dashurl|dash_url|videourl|video_url|videosrc|video_src|mp4|mp4url|mp4_url|stream|streamurl|stream_url|contenturl|playlist|manifest|manifesturl|manifest_url|m3u8|m3u8url|m3u8_url|audiourl|audio_url|downloadurl|download_url|fileurl|file_url)["']?\s*[:=]\s*["']([^"'\s<>]{4,1000})["']`)
	// grabRelMediaRE finds quoted site-relative media paths: "/videos/a.mp4".
	grabRelMediaRE = regexp.MustCompile(`(?i)["']((?:\.{1,2})?/[^"'\s<>]+?\.(?:m3u8|mpd|mp4|m4v|webm|mkv|mov|ogv|mp3|m4a|aac|flac|ogg|opus|wav|zip|rar|7z|pdf|exe|msi|apk|dmg|iso)(?:\?[^"'\s<>]*)?)["']`)
	grabJSEscRE    = regexp.MustCompile(`\\u00([0-9a-fA-F]{2})|\\x([0-9a-fA-F]{2})|\\/`)
	grabYTIDRE     = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	grabMuxRE      = regexp.MustCompile(`(?:image|stream)\.mux\.com/([A-Za-z0-9]{20,64})`)
	grabJWRE       = regexp.MustCompile(`(?:cdn\.jwplayer\.com|content\.jwplatform\.com)/(?:v2/media|manifests|previews|players|videos)/([A-Za-z0-9]{8})\b`)
	grabWistiaRE   = regexp.MustCompile(`(?:wistia_async_|fast\.wistia\.(?:net|com)/embed/(?:iframe|medias)/)([a-z0-9]{10})\b`)
)

// grabScriptExts are the extensions an address found in script or free text must
// end with to count: media and the file types people download. (Images are
// left out — scripts are full of thumbnails and sprites — and so is ".ts",
// which in a script is far more often TypeScript than MPEG-TS.)
var grabScriptExts = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields("m3u8 mpd mp4 m4v webm mkv mov flv ogv avi wmv 3gp mp3 m4a m4b aac flac ogg oga opus wav " +
		"zip rar 7z pdf exe msi apk xapk iso dmg torrent") {
		m[e] = true
	}
	return m
}()

// grabUnescapeJS decodes the escapes JSON and JS put in URLs: \/ / \x2F.
func grabUnescapeJS(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return grabJSEscRE.ReplaceAllStringFunc(s, func(m string) string {
		if m == `\/` {
			return "/"
		}
		b, err := strconv.ParseUint(m[len(m)-2:], 16, 8)
		if err != nil || b < 0x20 || b > 0x7e {
			return m
		}
		return string(rune(b))
	})
}

// scanScript finds media addresses in script (or other free) text.
func (x *grabExtractor) scanScript(body, source string) {
	s := grabUnescapeJS(body)
	for _, m := range grabAbsURLRE.FindAllString(s, -1) {
		m = strings.TrimRight(m, `.,;:)]`)
		if grabURLExt(m) == "ts" {
			continue // see grabScriptExts
		}
		if k := grabMediaKind(m); k != "" || grabScriptExts[grabURLExt(m)] {
			x.add(grabCand{URL: m, Source: source, Kind: k})
		}
	}
	for _, m := range grabPlayerKeyRE.FindAllStringSubmatch(s, -1) {
		v := m[2]
		if !strings.Contains(v, "/") || grabURLExt(v) == "ts" {
			// A bare "name.webm" is a file name in some config (MediaWiki's
			// page title), not an address relative to this page.
			continue
		}
		kind := grabMediaKind(v)
		if kind == "" && !grabScriptExts[grabURLExt(v)] {
			continue
		}
		src := grabSrcPlayer
		if source == grabSrcText {
			src = grabSrcText
		}
		x.add(grabCand{URL: v, Source: src, Kind: kind})
	}
	for _, m := range grabRelMediaRE.FindAllStringSubmatch(s, -1) {
		x.add(grabCand{URL: m[1], Source: source, Kind: grabMediaKind(m[1])})
	}
}

// playerHints turns hosted-player fingerprints into their media: a Mux
// playback id (thumbnails live on image.mux.com/<id>/) into its HLS playlist,
// a JW Player media id into its manifest, a Wistia id into its embed page.
func (x *grabExtractor) playerHints(doc string) {
	for _, m := range grabMuxRE.FindAllStringSubmatch(doc, 20) {
		x.add(grabCand{URL: "https://stream.mux.com/" + m[1] + ".m3u8", Source: grabSrcPlayer, Kind: "stream"})
	}
	for _, m := range grabJWRE.FindAllStringSubmatch(doc, 20) {
		x.add(grabCand{URL: "https://cdn.jwplayer.com/manifests/" + m[1] + ".m3u8", Source: grabSrcPlayer, Kind: "stream"})
	}
	for _, m := range grabWistiaRE.FindAllStringSubmatch(doc, 10) {
		x.add(grabCand{URL: "https://fast.wistia.net/embed/iframe/" + m[1], Source: grabSrcPlayer, Embed: true})
	}
}

// queryMedia finds media addresses carried in another URL's query string —
// player demo pages (hls.js ?src=<m3u8>), redirectors (?url=<file>).
func (x *grabExtractor) queryMedia(raw, source string) {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return
	}
	keys := make([]string, 0)
	q := u.Query()
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range q[k] {
			if !grabHTTPRE.MatchString(v) {
				continue
			}
			if kind := grabMediaKind(v); kind != "" || grabFileExt[grabURLExt(v)] {
				x.add(grabCand{URL: v, Source: source, Kind: kind, Linked: source == grabSrcLink})
			}
		}
	}
}

// hostHints adds the real download behind a few well-known landing pages
// that need no HTML to work out.
func (x *grabExtractor) hostHints() {
	p := x.page
	host := strings.TrimPrefix(strings.ToLower(p.Hostname()), "www.")
	switch {
	case host == "dropbox.com" && (strings.HasPrefix(p.Path, "/s/") || strings.HasPrefix(p.Path, "/scl/")):
		u := *p
		q := u.Query()
		q.Set("dl", "1")
		u.RawQuery = q.Encode()
		x.add(grabCand{URL: u.String(), Source: grabSrcLink, Linked: true})
	case host == "drive.google.com" && strings.HasPrefix(p.Path, "/file/d/"):
		if id, _, _ := strings.Cut(strings.TrimPrefix(p.Path, "/file/d/"), "/"); id != "" {
			x.add(grabCand{URL: "https://drive.usercontent.google.com/download?id=" + url.QueryEscape(id) + "&export=download",
				Source: grabSrcLink, Linked: true, Follow: true})
		}
	}
}

// ---- adding & filtering -----------------------------------------------------------

// add resolves, filters and dedups a candidate.
func (x *grabExtractor) add(c grabCand) {
	u := x.resolve(c.URL)
	if u == "" {
		return
	}
	pu, err := url.Parse(u)
	if err != nil || grabJunkURL(pu, c) {
		return
	}
	c.URL = u
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		c.nameQ = 0
	}
	key := grabKey(u)
	if i, ok := x.index[key]; ok {
		x.out[i] = grabMergeCand(x.out[i], c)
		return
	}
	x.index[key] = len(x.out)
	x.out = append(x.out, c)
}

// grabMergeCand folds a duplicate sighting into the first one: the stronger
// source label, the better name, and whatever details either knew.
func grabMergeCand(a, b grabCand) grabCand {
	if grabSourceRank[b.Source] > grabSourceRank[a.Source] {
		a.Source = b.Source
	}
	if b.nameQ > a.nameQ {
		a.Name, a.nameQ = b.Name, b.nameQ
	}
	if a.Kind == "" || (a.Kind == "image" && b.Kind != "" && b.Kind != "image") {
		a.Kind = grabFirst(b.Kind, a.Kind)
	}
	if a.Width == 0 && a.Height == 0 {
		a.Width, a.Height = b.Width, b.Height
	}
	a.Thumb = grabFirst(a.Thumb, b.Thumb)
	if a.Duration == 0 {
		a.Duration = b.Duration
	}
	a.Linked = a.Linked || b.Linked
	a.Follow = a.Follow || b.Follow
	a.Embed = a.Embed || b.Embed
	a.Frame = a.Frame && b.Frame // a real sighting beats "read it to find out"
	return a
}

// resolve makes ref absolute against the page, or "" when it isn't an
// http(s) address worth anything (data:, javascript:, templates, anchors).
func (x *grabExtractor) resolve(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > 4096 {
		return ""
	}
	low := strings.ToLower(ref)
	for _, p := range []string{"#", "data:", "blob:", "javascript:", "mailto:", "tel:", "sms:", "about:", "file:", "intent:", "chrome:", "chrome-extension:", "moz-extension:"} {
		if strings.HasPrefix(low, p) {
			return ""
		}
	}
	if strings.ContainsAny(ref, "<>{}\"`\n\r\t") || strings.Contains(ref, "${") || strings.Contains(ref, "+'") || strings.Contains(ref, "' +") {
		return "" // a template or string concatenation, not an address
	}
	if strings.HasPrefix(ref, "//") {
		ref = "https:" + ref
	}
	u, err := x.base.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(u.Host, "_ ") {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	u.Host = strings.ToLower(u.Host)
	if port := u.Port(); (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		u.Host = u.Hostname()
	}
	u.RawQuery = grabStripTracking(u.RawQuery)
	return u.String()
}

// grabStripTracking drops campaign tags and WordPress media cache-busters
// (tap072.mp3?_=2) that make one file look like several.
func grabStripTracking(raw string) string {
	if raw == "" {
		return raw
	}
	parts := strings.Split(raw, "&")
	out := parts[:0]
	for _, p := range parts {
		k, v, _ := strings.Cut(p, "=")
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || lk == "fbclid" || lk == "gclid" || (lk == "_" && isNumeric(v)) || p == "" {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "&")
}

// grabKey is the dedup identity of an address: the cleaned URL, or for a
// known video site, the video's id (watch/embed/short links are one video).
func grabKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	host = strings.TrimPrefix(host, "m.")
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case host == "youtube.com" || host == "youtube-nocookie.com" || host == "music.youtube.com":
		if v := u.Query().Get("v"); grabYTIDRE.MatchString(v) {
			return "yt:" + v
		}
		if len(seg) >= 2 && (seg[0] == "embed" || seg[0] == "shorts" || seg[0] == "v" || seg[0] == "live") && grabYTIDRE.MatchString(seg[1]) {
			return "yt:" + seg[1]
		}
	case host == "youtu.be":
		if len(seg) >= 1 && grabYTIDRE.MatchString(seg[0]) {
			return "yt:" + seg[0]
		}
	case host == "vimeo.com" || host == "player.vimeo.com":
		for _, s := range seg {
			if isNumeric(s) && len(s) >= 5 {
				return "vimeo:" + s
			}
		}
	case host == "dailymotion.com" || host == "dai.ly" || host == "geo.dailymotion.com":
		if v := u.Query().Get("video"); v != "" {
			return "dm:" + v
		}
		if len(seg) >= 1 && host == "dai.ly" {
			return "dm:" + seg[0]
		}
		for i, s := range seg {
			if s == "video" && i+1 < len(seg) {
				id, _, _ := strings.Cut(seg[i+1], "_")
				return "dm:" + id
			}
		}
	}
	// The same file is often linked with and without %-escapes
	// (archive.org's metadata vs its HTML): compare decoded paths.
	u.Host = strings.ToLower(u.Host)
	u.RawPath = ""
	u.Fragment, u.RawFragment = "", ""
	return u.Scheme + "://" + u.Host + u.Path + "?" + u.RawQuery
}

// grabJunkURL drops addresses that are never what a user wants to download:
// analytics beacons, tracking pixels, icons/logos/sprites, fonts and web
// resources, and UI sound effects. An explicit link is trusted more.
func grabJunkURL(u *url.URL, c grabCand) bool {
	host := strings.ToLower(u.Hostname())
	for _, d := range grabJunkHosts {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	p := strings.ToLower(u.Path)
	if (host == "facebook.com" || strings.HasSuffix(host, ".facebook.com")) && strings.HasPrefix(p, "/tr") {
		return true
	}
	if (host == "youtube.com" || strings.HasSuffix(host, ".youtube.com")) && strings.HasPrefix(p, "/s/") {
		return true // player/search UI sounds and assets
	}
	if grabWikiPage(u) {
		return true // /wiki/File:Clip.webm is the page about the file, never the file
	}
	if (c.Width > 0 && c.Width <= 2) || (c.Height > 0 && c.Height <= 2) {
		return true // tracking pixel
	}
	_, ext := splitExt(pathName(u.String()))
	if !c.Linked && (grabFontExts[ext] || grabWebExts[ext]) {
		return true
	}
	if c.Linked {
		return false
	}
	last := p[strings.LastIndexByte(p, '/')+1:]
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "pixel", "beacon", "collect", "tracking", "track", "pixel.gif", "beacon.gif":
			return true // match whole segments: "/collections/clip.mp4" is not a beacon
		}
	}
	for _, s := range []string{"1x1.", "spacer.", "blank.gif", "transparent.gif", "clear.gif"} {
		if strings.Contains(last, s) {
			return true
		}
	}
	// "0.gif?kind=track_js…", "b.gif?…": a one- or two-letter GIF with a query
	// string is a hit counter.
	if stem, _ := splitExt(last); ext == "gif" && len(stem) <= 2 && u.RawQuery != "" {
		return true
	}
	if c.Kind == "image" || grabImageExts[ext] {
		for _, s := range []string{"favicon", "apple-touch-icon", "/icons/", "/icon/", "/emoji/", "/emojis/", "/flags/"} {
			if strings.Contains(p, s) {
				return true
			}
		}
		for _, s := range []string{"sprite", "logo", "icon-", "-icon", "_icon", "icon_", "badge", "spinner", "loader", "placeholder"} {
			if strings.Contains(last, s) {
				return true
			}
		}
	}
	return false
}

// grabJunkFrame drops iframes that are ads, widgets and consent/captcha boxes.
func grabJunkFrame(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	p := strings.ToLower(u.Path)
	for _, s := range []string{"recaptcha", "/plugins/like", "/plugins/share", "/plugins/page", "widgets", "/ads/", "/ad/", "consent", "cookie", "disqus", "/sw_iframe", "/ns.html", "/chat"} {
		if strings.Contains(host+p, s) {
			return true
		}
	}
	for _, d := range []string{"platform.twitter.com", "disqus.com", "googleads.g.doubleclick.net", "accounts.google.com", "challenges.cloudflare.com", "hcaptcha.com", "trustarc.com", "onetrust.com"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// grabJunkHosts serve analytics, ads and tracking — nothing downloadable.
var grabJunkHosts = []string{
	"google-analytics.com", "googletagmanager.com", "googlesyndication.com", "googleadservices.com",
	"doubleclick.net", "adservice.google.com", "connect.facebook.net", "scorecardresearch.com",
	"quantserve.com", "quantcount.com", "hotjar.com", "criteo.com", "criteo.net", "taboola.com", "outbrain.com",
	"adnxs.com", "pubmatic.com", "rubiconproject.com", "amazon-adsystem.com", "adsrvr.org", "bat.bing.com",
	"clarity.ms", "cloudflareinsights.com", "nr-data.net", "chartbeat.com", "chartbeat.net", "segment.io",
	"mixpanel.com", "stats.wp.com", "pixel.wp.com", "ads.linkedin.com", "analytics.twitter.com",
	"ads-twitter.com", "analytics.tiktok.com", "mc.yandex.ru", "top-fwz1.mail.ru", "counter.yadro.ru",
	"sentry.io", "addthis.com", "sharethis.com", "gravatar.com", "moatads.com", "adform.net", "casalemedia.com",
	"openx.net", "smartadserver.com", "teads.tv", "yieldmo.com", "3lift.com", "sharethrough.com", "onesignal.com",
}

// grabVideoHosts are sites whose pages and players yt-dlp extracts.
var grabVideoHosts = []string{
	"youtube.com", "youtube-nocookie.com", "youtu.be", "vimeo.com", "dailymotion.com", "dai.ly",
	"streamable.com", "rumble.com", "odysee.com", "ok.ru", "bilibili.com", "bilibili.tv", "twitch.tv",
	"facebook.com", "fb.watch", "instagram.com", "tiktok.com", "twitter.com", "x.com", "soundcloud.com",
	"bandcamp.com", "mixcloud.com", "wistia.com", "wistia.net", "jwplayer.com", "jwplatform.com",
	"brightcove.net", "kaltura.com", "vk.com", "vkvideo.ru", "rutube.ru", "loom.com", "vidyard.com",
	"ted.com", "bitchute.com", "nicovideo.jp", "archive.org", "videopress.com", "streamja.com", "coub.com",
	"imgur.com", "reddit.com", "redd.it", "spotify.com", "podbean.com", "libsyn.com", "megaphone.fm",
	"simplecast.com", "anchor.fm", "buzzsprout.com", "sproutvideo.com", "vidio.com", "tubitv.com",
}

// isGrabVideoHost reports a host (or its parent domain) yt-dlp handles.
func isGrabVideoHost(host string) bool {
	host = strings.ToLower(host)
	for _, d := range grabVideoHosts {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// grabWikiPage reports a MediaWiki page link: /wiki/<Namespace>:<title> or
// index.php?title=… — pages whose names end in the file's extension.
func grabWikiPage(u *url.URL) bool {
	if strings.HasPrefix(u.Path, "/wiki/") {
		return strings.Contains(u.Path[len("/wiki/"):], ":")
	}
	return strings.HasSuffix(u.Path, "/index.php") && u.Query().Get("title") != ""
}

// grabDownloadEndpoint reports the shape of a download link without a file
// extension: /download, download.php, ?dl=1, export=download, get_file …
func grabDownloadEndpoint(u *url.URL) bool {
	return grabEndpointRE.MatchString(strings.ToLower(u.EscapedPath() + "?" + u.RawQuery))
}

// "/downloads" alone is usually a listing page (ModDB's files/top/latest
// tabs); the plural only counts when an id follows (/downloads/start/317674).
var grabEndpointRE = regexp.MustCompile(`(/download(/|\?|\.|$)|/downloads/[^?]*\d|download\.(php|aspx?|cgi)|/dl/|/dl\?|[?&](dl|download|raw)=(1|true)\b|get_?file|/attachments?/|export=download|/uc\?|/mirror/|/getfile|/fetch/)`)

// ---- small helpers ------------------------------------------------------------------

var (
	grabImageExts = grabExtSet("jpg jpeg png gif webp bmp svg tiff tif ico heic avif psd jfif")
	grabFontExts  = grabExtSet("woff woff2 ttf otf eot")
	grabWebExts   = grabExtSet("html htm xhtml shtml php asp aspx jsp cgi js mjs css map json xml rss atom")
	// grabFileExt: extensions that make a link a download (knownExt minus web pages/resources).
	grabFileExt = func() map[string]bool {
		m := map[string]bool{}
		for e := range knownExtSet {
			if !grabWebExts[e] && !grabFontExts[e] {
				m[e] = true
			}
		}
		for e := range grabExtSet("ogv oga m4b 7z lz lzma tgz xapk apks aab torrent srt vtt ass ssa sub epub mobi cbz cbr") {
			m[e] = true
		}
		return m
	}()
)

func grabExtSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(s) {
		m[e] = true
	}
	return m
}

// grabURLExt returns the lowercase extension of a URL's last path segment.
func grabURLExt(raw string) string {
	_, ext := splitExt(pathName(raw))
	return ext
}

// grabMediaKind classifies an address by extension: "stream" for HLS/DASH
// manifests, "video"/"audio" for media files, "" otherwise.
func grabMediaKind(raw string) string {
	ext := grabURLExt(raw)
	switch {
	case ext == "m3u8" || ext == "mpd":
		return "stream"
	case grabVideoExts[ext]:
		return "video"
	case grabAudioExts[ext]:
		return "audio"
	}
	low := strings.ToLower(raw)
	if strings.Contains(low, ".m3u8?") || strings.Contains(low, "/manifest.m3u8") || strings.Contains(low, ".mpd?") {
		return "stream"
	}
	return ""
}

var (
	grabVideoExts = grabExtSet("mp4 m4v webm mkv mov avi flv wmv mpg mpeg ts 3gp ogv f4v")
	grabAudioExts = grabExtSet("mp3 m4a m4b aac flac wav ogg oga opus wma weba alac aiff")
)

// grabSameSite reports whether two URLs share a registrable domain.
func grabSameSite(a, b *url.URL) bool {
	return grabSite(a.Hostname()) == grabSite(b.Hostname())
}

// grabSite is a cheap registrable-domain guess (last two labels, three for
// "co.uk"-style suffixes) — enough to tell a page's own links from others.
func grabSite(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	n := 2
	if l := labels[len(labels)-2]; len(l) <= 3 && (l == "co" || l == "com" || l == "net" || l == "org" || l == "ac" || l == "gov" || l == "edu") {
		n = 3
	}
	return strings.Join(labels[len(labels)-n:], ".")
}

// grabLargestSrcset picks the biggest image in a srcset ("a.jpg 480w, b.jpg 1080w").
// Image CDNs put commas inside the URL itself (Cloudinary's /w_400,c_fill/),
// so it is read the way browsers do: a URL runs to whitespace, and only then
// does a comma end the candidate.
func grabLargestSrcset(s string) string {
	best, bestW := "", -1.0
	for s != "" {
		s = strings.TrimLeftFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
		end := strings.IndexFunc(s, unicode.IsSpace)
		if end < 0 {
			end = len(s)
		}
		u, desc := s[:end], ""
		s = s[end:]
		if strings.HasSuffix(u, ",") {
			u = strings.TrimRight(u, ",")
		} else if c := strings.IndexByte(s, ','); c >= 0 {
			desc, s = s[:c], s[c+1:]
		} else {
			desc, s = s, ""
		}
		if u == "" {
			continue
		}
		f := append([]string{u}, strings.Fields(desc)...)
		w := 1.0
		if len(f) > 1 {
			d := strings.ToLower(f[1])
			if v, err := strconv.ParseFloat(strings.TrimRight(d, "wx"), 64); err == nil {
				w = v
				if strings.HasSuffix(d, "x") {
					w *= 1000 // density descriptors: rank above unknown widths
				}
			}
		}
		if w > bestW {
			best, bestW = f[0], w
		}
	}
	return best
}

// grabUsefulText keeps link text that can name a file (not "Download", "here").
func grabUsefulText(t string) bool {
	if n := utf8.RuneCountInString(t); n < 3 || n > 150 || grabButtonTextRE.MatchString(t) {
		return false
	}
	switch strings.ToLower(strings.Trim(t, " .:!»›→")) {
	case "download", "download now", "here", "click here", "link", "get", "mirror", "direct link", "download file",
		"mp4", "webm", "ogg", "mp3", "zip", "pdf", "source", "file":
		return false
	}
	return true
}

// grabButtonTextRE matches button captions made only of "download"-ish words,
// counts and sizes: "Download (2.97MB)", "download 1 file", "175.31mb Download Now".
var grabButtonTextRE = regexp.MustCompile(`(?i)^[\s(\[]*(?:(?:download|downloads|get|mirror|direct|link|now|free|here|click|this|file|files|the|start|[\d.,]+\s*(?:[kmgt]i?b|bytes)?|[kmgt]i?b)(?:[\s().,:!\]»›→↓|-]+|$))+$`)

func grabAtoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(s), "px"))
	return n
}

func grabFirst(vs ...string) string {
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func grabCollapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func grabIsLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func grabIsSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// grabLower lowercases ASCII letters only, so byte offsets stay aligned.
func grabLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if c := b[j]; c >= 'A' && c <= 'Z' {
					b[j] = c + 32
				}
			}
			return string(b)
		}
	}
	return s
}
