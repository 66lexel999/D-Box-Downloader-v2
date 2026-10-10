package engine

// Request context: many media hosts only serve a file to a request that looks
// like the browser page that embedded it — the right Referer (hotlink
// protection on video CDNs, embedded players like uqload), the site's cookies
// (login-gated files), sometimes an Origin or a token header. Every HTTP request
// the engine makes goes through newRequest so the probe, the segment workers,
// the HLS fetcher and key requests all present the same identity.

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"myidm/internal/ytdlp"
)

// defaultUserAgent is used when no config is wired (tests) — a current desktop
// Chrome, since some CDNs refuse unknown agents.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// reqInfo is the browser context a download replays.
type reqInfo struct {
	Referer string
	Headers map[string]string // extra headers (Cookie, Authorization, Origin, …)
}

func (t *Task) reqInfo() reqInfo { return reqInfo{Referer: t.Referer, Headers: t.Headers} }

// blockedHeaders are headers a caller may not override: they belong to the
// transport or to our own range/resume logic.
var blockedHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
	"keep-alive": true, "upgrade": true, "te": true, "trailer": true, "range": true,
	"if-range": true, "accept-encoding": true, "expect": true, "proxy-authorization": true,
	"proxy-connection": true,
}

// CleanHeaders normalizes caller-supplied headers: canonical names, no blocked
// or empty entries, no CR/LF injection. Returns nil when nothing is left.
func CleanHeaders(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" || strings.ContainsAny(k+v, "\r\n") || blockedHeaders[strings.ToLower(k)] {
			continue
		}
		out[http.CanonicalHeaderKey(k)] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (e *Engine) userAgent() string {
	if e.cfg != nil && e.cfg.UserAgent != "" {
		return e.cfg.UserAgent
	}
	return defaultUserAgent
}

// newRequest builds a request carrying the download's browser context.
func (e *Engine) newRequest(ctx context.Context, method, rawURL string, ri reqInfo) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("User-Agent", e.userAgent())
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Accept-Encoding", "identity") // byte offsets must match the file on disk
	if ri.Referer != "" {
		h.Set("Referer", ri.Referer)
	}
	for k, v := range ri.Headers {
		if blockedHeaders[strings.ToLower(k)] {
			continue
		}
		h.Set(k, v)
	}
	// Login cookies the browser extension shared for this site, merged under
	// any cookies the caller passed explicitly (those win on a name clash).
	if jar := ytdlp.CookieHeader(rawURL); jar != "" {
		h.Set("Cookie", mergeCookies(h.Get("Cookie"), jar))
	}
	return req, nil
}

// mergeCookies appends the jar's cookies whose names aren't already present.
func mergeCookies(explicit, jar string) string {
	if explicit == "" {
		return jar
	}
	have := map[string]bool{}
	for _, p := range strings.Split(explicit, ";") {
		if name, _, ok := strings.Cut(strings.TrimSpace(p), "="); ok {
			have[name] = true
		}
	}
	out := explicit
	for _, p := range strings.Split(jar, ";") {
		p = strings.TrimSpace(p)
		if name, _, ok := strings.Cut(p, "="); ok && !have[name] {
			out += "; " + p
		}
	}
	return out
}

// refererCandidates lists the Referer values worth trying when a server turns
// the probe away: the caller's own first, then none (some hosts reject foreign
// referers), then the media host's origin, the file's own folder and the
// registrable domain (hotlink protection usually accepts "same site"; a few
// sites want the listing page the file sits in).
func refererCandidates(rawURL, given string) []string {
	cands := []string{given}
	if given != "" {
		cands = append(cands, "")
	}
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		cands = append(cands, u.Scheme+"://"+u.Host+"/")
		if i := strings.LastIndexByte(u.EscapedPath(), '/'); i > 0 {
			cands = append(cands, u.Scheme+"://"+u.Host+u.EscapedPath()[:i+1])
		}
		if d := ytdlp.RegistrableDomain(rawURL); d != "" && d != strings.ToLower(u.Hostname()) {
			cands = append(cands, u.Scheme+"://"+d+"/")
		}
	}
	seen := map[string]bool{}
	out := cands[:0]
	for _, c := range cands {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// strongETag returns etag only if it is a strong validator. A weak ETag
// (W/"…") must never go in If-Range: the server is required to treat it as a
// mismatch and resend the whole file, which used to fail every resumed segment.
func strongETag(etag string) string {
	if strings.HasPrefix(etag, "W/") || strings.HasPrefix(etag, "w/") {
		return ""
	}
	return etag
}

// siteSuffixes gives the engine's cookie jar the registrable-domain rules the
// rest of D BOX uses, so a site can't set cookies for a whole public suffix
// ("co.uk") while cookies for "example.co.uk" reach its subdomains.
type siteSuffixes struct{}

func (siteSuffixes) PublicSuffix(domain string) string {
	d := ytdlp.RegistrableDomain("http://" + domain + "/")
	if i := strings.IndexByte(d, '.'); i >= 0 {
		return d[i+1:]
	}
	if i := strings.LastIndexByte(domain, '.'); i >= 0 {
		return domain[i+1:]
	}
	return domain
}

func (siteSuffixes) String() string { return "dbox-registrable-domains" }

// newCookieJar is the engine client's jar. Cookies the caller sends explicitly
// (the extension's, the site's shared login jar) go in the request's own
// Cookie header first; the jar's are appended after them, and servers take
// the first value of a repeated name, so the explicit ones win.
func newCookieJar() http.CookieJar {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: siteSuffixes{}})
	if err != nil {
		return nil
	}
	return jar
}
