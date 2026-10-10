package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// probeResult describes what the server told us about the resource.
type probeResult struct {
	Size         int64 // -1 unknown
	Ranged       bool
	FileName     string // best file name for a plain file download
	ETag         string
	LastModified string
	ContentType  string
	Kind         string // kindFile / kindHLS / kindDASH / kindPage
	FinalURL     string // after redirects
	Referer      string // the Referer that got the server to answer
	URL          string // the address that served it: the link itself, or where its landing page pointed
	Landing      string // the landing page that led to URL ("" = none)
	Magic        string // what the first bytes are (sniffExt), "" = unrecognized
	Encoding     string // a Content-Encoding the server applied although we asked for identity
	Expected     string // the extension the link promised ("" = nothing in particular)

	page    []byte // an HTML/text answer's body, for the landing-page scan
	partial bool   // page holds only the first bytes of a longer body
}

// sniffLen is how many leading bytes the probe asks for: enough for every magic
// signature and to recognize an HLS/DASH manifest or an HTML page, small
// enough to cost nothing.
const sniffLen = 4096

// landingMaxBytes caps how much of a web page the probe reads looking for the
// file's real link (MediaFire's button sits ~100 KB into its page).
const landingMaxBytes = 2 << 20

// probeOpts are the probe's inputs beyond the request itself.
type probeOpts struct {
	title      string        // page title (names streams and generic links)
	name       string        // the user's own file name, if any
	headerWait time.Duration // per request: how long to wait for response headers
	budget     time.Duration // the whole probe, landing pages and fallbacks included
}

// Inspect's caller (the New Download dialog) gives up after 30 s, so its probe
// must finish well before; a starting download can afford to wait longer on a
// slow server.
var (
	inspectProbe  = probeOpts{headerWait: 10 * time.Second, budget: 25 * time.Second}
	downloadProbe = probeOpts{headerWait: 20 * time.Second, budget: 60 * time.Second}
)

// probeStatusError is a non-success HTTP answer to the probe.
type probeStatusError struct {
	code      int
	status    string
	challenge bool // Cloudflare answered with a bot check ("cf-mitigated: challenge")
}

func (p *probeStatusError) Error() string {
	msg := "server returned " + p.status
	if p.challenge {
		return msg + " — the site's Cloudflare bot check blocked the request"
	}
	switch p.code {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += " — the link may have expired, need a login, or only work from the page it came from"
	case http.StatusNotFound, http.StatusGone:
		msg += " — the file is no longer at this address"
	case http.StatusTooManyRequests:
		msg += " — the server is rate-limiting; try again later"
	}
	return msg
}

// probe inspects rawURL with the New Download dialog's time limits.
func (e *Engine) probe(ctx context.Context, rawURL string, ri reqInfo, title string) (probeResult, error) {
	o := inspectProbe
	o.title = title
	return e.probeWith(ctx, rawURL, ri, o)
}

// probeWith asks for the first sniffLen bytes and inspects the answer:
//   - 206 -> server honors ranges; total size from Content-Range.
//   - 200 -> no range support; size from Content-Length (may be -1/chunked).
//
// A small ranged GET is more truthful than HEAD: many servers answer HEAD with
// Accept-Ranges they don't honor, or omit Content-Length. When a server turns
// the request away — with an error status, or softly, with a web page where a
// file was promised — it retries with alternative Referers (hotlink protection
// is the usual cause) and follows a landing page's download link. Size still
// unknown? A HEAD may know it.
func (e *Engine) probeWith(ctx context.Context, rawURL string, ri reqInfo, o probeOpts) (probeResult, error) {
	bctx, cancel := context.WithTimeout(ctx, o.budget)
	defer cancel()
	pr, err := e.probeLink(bctx, rawURL, ri, o, expectFor(rawURL, o.name), 0)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return probeResult{}, fmt.Errorf("the server did not answer within %d seconds", int(o.budget/time.Second))
	}
	return pr, err
}

// probeLink probes one address, cycling Referers until an answer is the file.
func (e *Engine) probeLink(ctx context.Context, rawURL string, ri reqInfo, o probeOpts, ex expectation, depth int) (probeResult, error) {
	keepEnc := keepsEncoding(ex.ext)
	var (
		lastErr     error
		refusal     *probeResult // the first soft refusal
		refusalKind int
		scanned     bool
	)
	cands := refererCandidates(rawURL, ri.Referer)
	for i := 0; i < len(cands); i++ {
		try := reqInfo{Referer: cands[i], Headers: ri.Headers}
		for _, ranged := range []bool{true, false} {
			pr, sniff, err := e.probeOnce(ctx, rawURL, try, ranged, keepEnc, o.headerWait)
			if err != nil {
				if ctx.Err() != nil {
					if refusal != nil {
						return probeResult{}, newLandingError(refusal.FinalURL, refusalKind == refusalImage)
					}
					if lastErr != nil {
						return probeResult{}, lastErr
					}
					return probeResult{}, ctx.Err()
				}
				var se *probeStatusError
				if !errors.As(err, &se) {
					if refusal != nil { // a later fallback failing says nothing new
						break
					}
					return probeResult{}, err // network error — no point cycling headers
				}
				// Report the first real refusal (from the caller's own headers), not a
				// 416 from our range probe or whatever a fallback Referer got.
				var prev *probeStatusError
				if lastErr == nil || (errors.As(lastErr, &prev) && prev.code == http.StatusRequestedRangeNotSatisfiable &&
					se.code != http.StatusRequestedRangeNotSatisfiable) {
					lastErr = err
				}
				continue
			}
			pr.Referer = try.Referer
			pr.URL = rawURL
			pr.Expected = ex.ext
			kind := answerRefusal(ex, pr.FileName, rawURL, pr.FinalURL, sniff, pr.Magic, pr.Size)
			if kind == refusalNone {
				return e.acceptProbe(ctx, rawURL, ri, try, pr, sniff, o, ex, depth)
			}
			if refusal == nil {
				r := pr
				refusal, refusalKind = &r, kind
			}
			// The page may say where the file is: follow that link (once) before
			// cycling Referers.
			if !scanned && kind == refusalPage && depth < landingDepth && isHTMLAnswer(pr) && !isGrabVideoHost(linkHost(pr.FinalURL)) {
				scanned = true
				switch target := e.landingLink(ctx, pr, try, o, ex); {
				case target == "":
				case sameURL(target, rawURL):
					// The page links the very file: ask again as if clicked there.
					cands = insertAfter(cands, i, pr.FinalURL)
				default:
					return e.followLanding(ctx, target, pr.FinalURL, ri, o, ex, depth)
				}
			}
			break // a soft refusal isn't about Range: next Referer
		}
	}
	if refusal != nil {
		// A media link that opens a player page (not the site's home page) may
		// still hold the video: that is yt-dlp's job. Archives, programs and
		// documents have no player to extract from.
		if refusalKind == refusalPage && !strictExt(ex.ext) && isHTMLAnswer(*refusal) && !lostPath(rawURL, refusal.FinalURL) {
			pr := *refusal
			pr.Kind, pr.Size, pr.Ranged, pr.page = kindPage, -1, false, nil
			pr.FileName = resolveFileName(nameHints{Disposition: pr.FileName, OrigURL: rawURL, FinalURL: pr.FinalURL,
				ContentType: pr.ContentType, Title: o.title, Referer: ri.Referer})
			return pr, nil
		}
		return probeResult{}, newLandingError(refusal.FinalURL, refusalKind == refusalImage)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("server did not answer")
	}
	return probeResult{}, lastErr
}

// acceptProbe finishes a probe whose answer is what it claims to be: kind,
// size, name. A web page nobody promised a file for gets one look for an
// unmistakable download link before it is handed on as a page.
func (e *Engine) acceptProbe(ctx context.Context, rawURL string, ri, try reqInfo, pr probeResult, sniff []byte, o probeOpts, ex expectation, depth int) (probeResult, error) {
	disposition := pr.FileName // probeOnce stashes the raw header here
	name := o.name
	if name == "" {
		name = dispositionName(disposition)
	}
	pr.Kind = classifyProbe(rawURL, pr.FinalURL, pr.ContentType, sniff, name)
	if pr.Kind == kindPage {
		pr.Size, pr.Ranged = -1, false // the page's own size means nothing to the user
		if depth < landingDepth && !isGrabVideoHost(linkHost(pr.FinalURL)) {
			if target := e.landingLink(ctx, pr, try, o, ex); target != "" && !sameURL(target, rawURL) {
				if fr, err := e.followLanding(ctx, target, pr.FinalURL, ri, o, ex, depth); err == nil {
					return fr, nil
				}
				// The link led nowhere: the page itself may still hold a video.
			}
		}
	}
	pr.page = nil
	if pr.Kind == kindFile && pr.Size < 0 && pr.Encoding == "" {
		size, hdisp := e.headInfo(ctx, rawURL, try, o.headerWait)
		if size > 0 {
			pr.Size = size
		}
		if disposition == "" {
			disposition = hdisp // download scripts often name the file only on a full answer
		}
	}
	pr.FileName = resolveFileName(nameHints{
		Disposition: disposition,
		OrigURL:     rawURL,
		FinalURL:    pr.FinalURL,
		ContentType: pr.ContentType,
		Sniff:       sniff,
		Title:       o.title,
		Referer:     ri.Referer,
	})
	if pr.Kind == kindFile {
		pr.FileName = reconcileExt(pr.FileName, pr.Magic)
	}
	return pr, nil
}

// landingLink reads a landing page (fetching all of it when the probe only
// got its first bytes) and picks the link that is clearly the download.
func (e *Engine) landingLink(ctx context.Context, pr probeResult, try reqInfo, o probeOpts, ex expectation) string {
	doc := pr.page
	if pr.partial || len(doc) == 0 {
		full, _, err := e.probeOnce(ctx, pr.FinalURL, try, false, false, o.headerWait)
		if err != nil {
			return ""
		}
		doc = full.page
	}
	if len(doc) == 0 {
		return ""
	}
	return landingTarget(pr.FinalURL, doc, ex)
}

// followLanding probes the link a landing page pointed at, as a browser would
// after the click: with the page as Referer and its cookies (the engine's
// jar keeps them). Only a real file counts.
func (e *Engine) followLanding(ctx context.Context, target, landing string, ri reqInfo, o probeOpts, ex expectation, depth int) (probeResult, error) {
	next := ex
	next.names = append([]string(nil), ex.names...)
	next.addURLNames(target)
	pr, err := e.probeLink(ctx, target, reqInfo{Referer: landing, Headers: ri.Headers}, o, next, depth+1)
	if err != nil {
		return probeResult{}, err
	}
	if pr.Kind != kindFile {
		return probeResult{}, newLandingError(landing, false)
	}
	pr.Landing = landing
	return pr, nil
}

// linkHost returns rawURL's host name ("" when unparseable).
func linkHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Hostname()
	}
	return ""
}

// insertAfter puts s into list right after index i, unless it is there already.
func insertAfter(list []string, i int, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	out := append([]string(nil), list[:i+1]...)
	out = append(out, s)
	return append(out, list[i+1:]...)
}

// probeOnce performs one probe request. On success FileName temporarily holds
// the raw Content-Disposition header (probe resolves the real name), and the
// returned sniff is the body's first bytes — decoded when the server forced a
// Content-Encoding on us, unless keepEnc says the target is compressed data.
// A textual answer (a page, an error) is read further into pr.page.
func (e *Engine) probeOnce(ctx context.Context, rawURL string, ri reqInfo, ranged, keepEnc bool, headerWait time.Duration) (probeResult, []byte, error) {
	for attempt := 0; ; attempt++ {
		pr, sniff, retry, err := e.probeAttempt(ctx, rawURL, ri, ranged, keepEnc, headerWait)
		if retry > 0 && attempt < 2 {
			if sleepCtx(ctx, retry) != nil {
				return probeResult{}, nil, ctx.Err()
			}
			continue
		}
		return pr, sniff, err
	}
}

// probeAttempt is one request of probeOnce; retry > 0 asks for another try
// after that long (a network error, or a busy server).
func (e *Engine) probeAttempt(ctx context.Context, rawURL string, ri reqInfo, ranged, keepEnc bool, headerWait time.Duration) (probeResult, []byte, time.Duration, error) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := e.newRequest(actx, http.MethodGet, rawURL, ri)
	if err != nil {
		return probeResult{}, nil, 0, err
	}
	if ranged {
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", sniffLen-1))
	}
	// Bound the wait for headers only: reading a landing page afterwards is
	// bounded by the probe's overall budget.
	timer := time.AfterFunc(headerWait, cancel)
	resp, err := e.client.Do(req)
	if !timer.Stop() && ctx.Err() == nil {
		// Too slow (or the headers arrived just as we gave up, with the body
		// already cancelled). Retrying would only wait as long again.
		if err == nil {
			resp.Body.Close()
		}
		return probeResult{}, nil, 0, fmt.Errorf("the server did not answer within %d seconds", int(headerWait/time.Second))
	}
	if err != nil {
		if ctx.Err() == nil {
			return probeResult{}, nil, time.Second, err
		}
		return probeResult{}, nil, 0, err
	}
	defer resp.Body.Close()
	// Busy server: honor a short Retry-After once or twice before giving up.
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		wait := retryAfter(resp.Header.Get("Retry-After"), 2*time.Second)
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > wait {
			err := &probeStatusError{code: resp.StatusCode, status: resp.Status, challenge: isChallenge(resp)}
			return probeResult{}, nil, max(wait, time.Millisecond), err
		}
	}

	pr := probeResult{
		Size:         -1,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ContentType:  resp.Header.Get("Content-Type"),
		FileName:     resp.Header.Get("Content-Disposition"),
		FinalURL:     rawURL,
	}
	if resp.Request != nil && resp.Request.URL != nil {
		pr.FinalURL = resp.Request.URL.String()
	}
	body := io.Reader(resp.Body)
	if ce := resp.Header.Get("Content-Encoding"); !isIdentityEncoding(ce) {
		// Sizes and offsets describe the encoded stream: no ranges, size
		// unknown until the (decoded) download ends.
		pr.Encoding = strings.ToLower(strings.TrimSpace(ce))
		if !keepEnc {
			body, _ = decodeBody(resp.Body, ce)
		}
	}
	var total int64 = -1
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		pr.Ranged = pr.Encoding == ""
		if t, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok {
			total = t
			pr.Size = t
		}
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		pr.Ranged = false
		if resp.ContentLength >= 0 && pr.Encoding == "" {
			pr.Size = resp.ContentLength
		}
	default:
		return probeResult{}, nil, 0, &probeStatusError{code: resp.StatusCode, status: resp.Status,
			challenge: isChallenge(resp)}
	}
	sniff := make([]byte, sniffLen)
	n, _ := io.ReadFull(body, sniff)
	sniff = sniff[:n]
	pr.Magic = sniffExt(sniff)
	if textualSample(sniff) {
		// A page or an error message: keep it for the landing-page scan.
		rest, _ := io.ReadAll(io.LimitReader(body, landingMaxBytes-int64(n)))
		pr.page = append(append(make([]byte, 0, n+len(rest)), sniff...), rest...)
		pr.partial = resp.StatusCode == http.StatusPartialContent && (total < 0 || total > int64(len(pr.page)))
	}
	return pr, sniff, 0, nil
}

// headInfo asks HEAD for what the GET didn't say: a Content-Length (chunked
// replies, "Content-Range: bytes 0-0/*") and a Content-Disposition (download
// scripts that name the file only on a full answer). Zero values when HEAD
// fails or doesn't know either.
func (e *Engine) headInfo(ctx context.Context, rawURL string, ri reqInfo, wait time.Duration) (int64, string) {
	hctx, cancel := context.WithTimeout(ctx, min(wait, 8*time.Second))
	defer cancel()
	req, err := e.newRequest(hctx, http.MethodHead, rawURL, ri)
	if err != nil {
		return 0, ""
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, ""
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !isIdentityEncoding(resp.Header.Get("Content-Encoding")) {
		return 0, ""
	}
	return resp.ContentLength, resp.Header.Get("Content-Disposition")
}

// isChallenge reports a Cloudflare bot-check answer.
func isChallenge(resp *http.Response) bool {
	return strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge")
}

func isIdentityEncoding(ce string) bool {
	ce = strings.ToLower(strings.TrimSpace(ce))
	return ce == "" || ce == "identity" || ce == "none"
}

// retryAfter parses a Retry-After header (seconds form), capped at 10s.
func retryAfter(h string, def time.Duration) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && s >= 0 {
		d := time.Duration(s) * time.Second
		if d > 10*time.Second {
			d = 10 * time.Second
		}
		return d
	}
	return def
}

// sleepCtx waits d or until ctx is done (returning ctx.Err()).
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseContentRangeTotal extracts N from "bytes 0-0/N".
func parseContentRangeTotal(h string) (int64, bool) {
	idx := strings.LastIndexByte(h, '/')
	if idx < 0 {
		return 0, false
	}
	totalStr := strings.TrimSpace(h[idx+1:])
	if totalStr == "*" {
		return 0, false
	}
	total, err := strconv.ParseInt(totalStr, 10, 64)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}
