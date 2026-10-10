package ytdlp

// Browser context vs. YouTube. The extension hands D BOX the page's cookies
// and the browser's User-Agent so hotlink-protected hosts accept yt-dlp's
// requests. YouTube is the exception: yt-dlp talks to it through its own API
// clients, each of which must present its own User-Agent, and it can only sign
// requests for a logged-in session (the SAPISIDHASH header) when the cookies
// come from a cookie jar. Passed as a raw "Cookie:" header they make YouTube
// see an unsigned logged-in session and answer "The page needs to be
// reloaded" — for every video, however often the page is refreshed. So YouTube
// gets no browser context at all unless a video actually needs the account
// (age-restricted, members-only, private); that retry uses a proper jar.

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// requestArgs renders the browser context for one yt-dlp run against rawURL:
// the Opts flags plus the site's cookies, as a cookie jar (never a header).
func requestArgs(rawURL string, o Opts) []string {
	cookie := headerValue(o.Headers, "Cookie")
	youtube := isYouTubeURL(rawURL)
	if youtube && !o.UseAccount {
		o.UserAgent, o.Referer, o.Headers = "", "", nil
		cookie = ""
	}
	a := o.args()
	if youtube && !o.UseAccount {
		return a
	}
	if jar := cookieArgsFor(rawURL); len(jar) > 0 {
		return append(a, jar...) // the extension's full login jar (/api/cookies) wins
	}
	if cookie != "" {
		if p := headerCookieJar(rawURL, cookie); p != "" {
			a = append(a, "--cookies", p)
		}
	}
	return a
}

// HasAccount reports whether a run against rawURL could use the user's login:
// cookies from the browser, either in the request context or a saved jar.
func HasAccount(rawURL string, o Opts) bool {
	return headerValue(o.Headers, "Cookie") != "" || HasCookiesFor(rawURL)
}

// NeedsAccount reports a yt-dlp error that only a signed-in session can get
// past — the cue to retry YouTube with the browser's cookies.
func NeedsAccount(msg string) bool {
	low := strings.ToLower(msg)
	for _, k := range []string{"sign in to confirm", "sign in to view", "sign in if", "login required",
		"log in to", "logged-in", "age-restricted", "age restricted", "inappropriate for some users",
		"members-only", "members only", "join this channel", "private video", "use --cookies"} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// AccountRetry decides whether a failed YouTube run should be repeated with
// the browser's login (it needed an account and one is available).
func AccountRetry(rawURL string, o Opts, msg string) bool {
	return isYouTubeURL(rawURL) && !o.UseAccount && NeedsAccount(msg) && HasAccount(rawURL, o)
}

func headerValue(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// headerCookieJar writes a "Cookie:" header as a Netscape cookie file scoped
// to rawURL's site (yt-dlp then sends the cookies only there, not to CDNs on
// other domains, and can sign YouTube requests) and returns its path; "" when
// it can't. One file per site and cookie set, reused across runs.
func headerCookieJar(rawURL, cookie string) string {
	dom := RegistrableDomain(rawURL)
	if dom == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n")
	exp := time.Now().Add(24 * time.Hour).Unix()
	n := 0
	for _, part := range strings.Split(cookie, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" || strings.ContainsAny(name, "\t\r\n") || strings.ContainsAny(value, "\t\r\n") {
			continue
		}
		fmt.Fprintf(&b, ".%s\tTRUE\t/\tFALSE\t%d\t%s\t%s\n", dom, exp, name, strings.TrimSpace(value))
		n++
	}
	if n == 0 {
		return ""
	}
	sum := sha1.Sum([]byte(dom + "\x00" + cookie))
	dir := filepath.Join(os.TempDir(), "dbox-ytdlp-cookies")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	p := filepath.Join(dir, dom+"-"+hex.EncodeToString(sum[:6])+".txt")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		return ""
	}
	return p
}
