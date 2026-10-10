package engine

// Landing pages: file hosts and mirrors rarely hand out the file itself for the
// link people copy. They answer with a web page that points at it — a
// countdown with <meta http-equiv="refresh">, MediaFire's "Download" button,
// ModDB's mirror link, Google Drive's "can't scan this file for viruses" form —
// or, when the request lacks the Referer/cookie the site wants, with the site's
// home page. Saving that HTML under the file's name (or asking yt-dlp to find a
// video on it) is wrong either way; this file recognizes those answers, follows
// the one link that is clearly the download, and otherwise fails with a message
// that says what happened.

import (
	"errors"
	"net/url"
	"path"
	"strings"
)

// errRetryAsFile is returned by convertToVideo when yt-dlp found that a "page"
// is really a plain file (a Dropbox preview, a mirror page whose only media is
// an .exe/.zip): it has pointed t.URL at the file and cleared t.Probed, and
// runHTTP downloads it natively — once, never handing it back to yt-dlp.
var errRetryAsFile = errors.New("resolved to a plain file")

// landingDepth caps how many landing pages in a row the probe follows
// (mirror page → countdown page → file is the longest chain seen in practice).
const landingDepth = 2

// landingError: a file was asked for, and the server answered with a web page
// (or a JSON/text error, or a placeholder picture) that led nowhere.
type landingError struct {
	Host     string
	FinalURL string
	image    bool // a picture where a video/audio file was expected
}

func (e *landingError) Error() string {
	if e.image {
		return "The server sent an image instead of the video (" + e.Host + ") — possibly region-blocked or hotlink-protected"
	}
	return "The server sent a web page instead of the file (" + e.Host + ") — the link may have expired or need to be started from its page in your browser"
}

func newLandingError(finalURL string, image bool) *landingError {
	host := finalURL
	if u, err := url.Parse(finalURL); err == nil && u.Host != "" {
		host = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	}
	return &landingError{Host: host, FinalURL: finalURL, image: image}
}

// expectation is what a link promises to deliver, judged from its names.
type expectation struct {
	ext   string   // extension of the promised file ("" = nothing in particular)
	names []string // lowercase file names a landing page's real link may carry
}

// expectFor reads the promise from the user's own name (the dialog, or the
// browser's suggestion the extension passed on) and the link's file name.
// Other path segments and query values that look like file names
// ("/file/<id>/Game.rar/file", "wait.php?f=Game.rar") only help recognize
// the right link on a landing page.
//
// A media name on a link that names nothing itself is not a promise: the New
// Download dialog names a pasted video page "Title.mp4", and that page is for
// yt-dlp to read, not a refusal.
func expectFor(rawURL, userName string) expectation {
	var ex expectation
	_, urlExt := splitExt(pathName(rawURL))
	if _, ext := splitExt(userName); extFamily(ext) != "" {
		if f := extFamily(ext); extFamily(urlExt) != "" || (f != famVideo && f != famAudio) {
			ex.ext = strings.ToLower(ext)
		}
	}
	if ex.ext == "" && extFamily(urlExt) != "" {
		ex.ext = strings.ToLower(urlExt)
	}
	ex.addName(userName)
	ex.addURLNames(rawURL)
	return ex
}

// addURLNames adds the file-like names in a URL's path and query.
func (ex *expectation) addURLNames(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if s, err := url.PathUnescape(seg); err == nil {
			ex.addName(s)
		}
	}
	for _, vs := range u.Query() {
		for _, v := range vs {
			ex.addName(path.Base(v))
		}
	}
}

// addName records name when it carries a binary file extension.
func (ex *expectation) addName(name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if _, ext := splitExt(name); !binaryExt(ext) {
		return
	}
	for _, n := range ex.names {
		if n == name {
			return
		}
	}
	ex.names = append(ex.names, name)
}

// matches reports whether rawURL's file name is one of the expected names.
func (ex *expectation) matches(rawURL string) bool {
	n := strings.ToLower(pathName(rawURL))
	for _, want := range ex.names {
		if n == want {
			return true
		}
	}
	return false
}

// Why an answer to a file request is not the file.
const (
	refusalNone  = iota
	refusalPage  // HTML/JSON/plain text, or a redirect to the site's home page
	refusalImage // a picture (a "not available in your region" poster) where video/audio was expected
)

// answerRefusal judges one 2xx answer against what the link promised. The
// server's own Content-Disposition name counts as a promise too: an
// "attachment; filename=x.zip" header over an HTML body is an error page.
func answerRefusal(ex expectation, disposition, orig, final string, sniff []byte, magic string, size int64) int {
	want := ex.ext
	if _, dext := splitExt(dispositionName(disposition)); dext != "" {
		if extFamily(dext) == famText {
			return refusalNone // the server says it is sending text
		}
		if want == "" {
			want = dext
		}
	}
	if !binaryExt(want) {
		return refusalNone
	}
	fam := extFamily(want)
	switch {
	case errorBody(sniff, size):
		return refusalPage
	case lostPath(orig, final) && !binaryMedia(magic):
		return refusalPage
	case (fam == famVideo || fam == famAudio) && extFamily(magic) == famImage:
		return refusalImage
	}
	return refusalNone
}

// errorBody reports a body that reads like an error or a web page rather than
// a file: text, and either markup/JSON or small. (A multi-megabyte plain-text
// body under a binary name is odd, but it is not an error message.)
func errorBody(head []byte, size int64) bool {
	if !textualSample(head) {
		return false
	}
	if size < 0 || size <= 1<<20 {
		return true
	}
	t := leadingText(head)
	return sniffExt(head) == "html" || strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") || strings.HasPrefix(t, "<")
}

// lostPath reports a redirect from a file's address to the site's front page —
// how many sites turn away a request without the right Referer or cookie.
func lostPath(orig, final string) bool {
	o, err1 := url.Parse(orig)
	f, err2 := url.Parse(final)
	if err1 != nil || err2 != nil || orig == final {
		return false
	}
	if _, ext := splitExt(pathName(orig)); ext == "" {
		return false
	}
	switch strings.ToLower(f.Path) {
	case "", "/", "/index.html", "/index.htm", "/index.php", "/home", "/default.aspx":
		return o.Path != f.Path
	}
	return false
}

// isHTMLAnswer reports an answer that is a web page (worth reading for links).
func isHTMLAnswer(pr probeResult) bool {
	if pr.Magic == "html" {
		return true
	}
	mt := mediaType(pr.ContentType)
	return pr.Magic == "" && (mt == "text/html" || mt == "application/xhtml+xml")
}

// landingTarget picks the one link on a landing page that is clearly the
// download; "" when there is none or the choice is ambiguous. Without an
// expected file (a page URL someone pasted) only unmistakable signs count —
// a countdown's meta refresh, Drive's confirm form, a single mirror link, a
// lone "Download" button to an archive/program/document on a page with no
// video of its own — so a video page still goes to yt-dlp.
func landingTarget(pageURL string, doc []byte, ex expectation) string {
	page, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	scan := extractCandidates(pageURL, string(doc))
	var byName, drive, mirror, button []string
	hasMedia := false
	for _, c := range scan.Cands {
		if c.Embed || c.Kind == "video" || c.Kind == "audio" || c.Kind == "stream" {
			hasMedia = true
		}
		if !c.Linked || c.URL == pageURL {
			continue
		}
		u, err := url.Parse(c.URL)
		if err != nil {
			continue
		}
		ext := grabURLExt(c.URL)
		if c.Source == grabSrcMeta { // only <meta http-equiv=refresh> is a Linked meta candidate
			if grabFileExt[ext] || grabDownloadEndpoint(u) || ex.matches(c.URL) {
				return c.URL
			}
			continue
		}
		switch {
		case ex.matches(c.URL):
			byName = appendNew(byName, c.URL)
		case isDriveConfirm(u):
			drive = appendNew(drive, c.URL)
		case strings.Contains(strings.ToLower(u.Path), "/mirror/") && grabSameSite(u, page):
			mirror = appendNew(mirror, c.URL)
		case grabFileExt[ext] && (strictExt(ext) || binaryExt(ex.ext)) &&
			(c.Follow || strings.Contains(strings.ToLower(c.Name), "download")):
			button = appendNew(button, c.URL)
		}
	}
	switch {
	case len(byName) > 0:
		return byName[0] // several links named like the file are mirrors of it
	case len(drive) == 1:
		return drive[0]
	case len(mirror) == 1:
		return mirror[0]
	case len(button) == 1 && (ex.ext != "" || !hasMedia):
		return button[0]
	}
	return ""
}

// isDriveConfirm matches Google Drive's "Download anyway" form target.
func isDriveConfirm(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(h, "google.com") && !strings.HasSuffix(h, "googleusercontent.com") {
		return false
	}
	q := u.Query()
	return q.Get("id") != "" && q.Get("confirm") != ""
}

func appendNew(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// sameURL compares two addresses ignoring a fragment.
func sameURL(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	ua.Fragment, ub.Fragment = "", ""
	return ua.String() == ub.String()
}
