package ytdlp

// yt-dlp's failures, turned into one line a person can act on, and the few
// failures that a different attempt gets past.

import (
	"errors"
	"os/exec"
	"regexp"
	"strings"
)

// NeedsImpersonation reports whether a yt-dlp error is a bot check that its
// browser impersonation gets past: Cloudflare says so in the message, and
// some sites' API endpoints (Rumble's "Unable to download JSON metadata: HTTP
// Error 403") refuse plain Python requests with a bare 403.
func NeedsImpersonation(msg string) bool {
	m := strings.ToLower(msg)
	for _, k := range []string{
		"cloudflare", "impersonat", "cf-mitigated", "cf_chl", "just a moment", "attention required",
		"unable to download json metadata: http error 403", "unable to download webpage: http error 403",
		"unable to download api page: http error 403",
	} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// ytdlpErr turns a failed yt-dlp run into its useful message: yt-dlp writes the
// real reason (login required, private, rate limit) to STDERR, which
// cmd.Output() stashes on *ExitError.Stderr — otherwise all the caller sees is
// "exit status 1".
func ytdlpErr(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		if l := ErrorLine(string(ee.Stderr)); l != "" {
			return l
		}
	}
	return err.Error()
}

// ErrorLine picks the actionable line of yt-dlp's stderr: its last "ERROR:"
// line (a traceback or a warning may follow it), else the last line.
func ErrorLine(stderr string) string {
	lines := strings.Split(strings.ReplaceAll(stderr, "\r\n", "\n"), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "ERROR:") {
			return clip(strings.TrimSpace(strings.TrimPrefix(l, "ERROR:")))
		}
		if last == "" {
			last = l
		}
	}
	return clip(last)
}

func clip(s string) string {
	if r := []rune(s); len(r) > 240 {
		return string(r[:240]) + "…"
	}
	return s
}

var (
	// "[RumbleEmbed] vb0ofn: " — the extractor and id say nothing to a user.
	extractorTagRE = regexp.MustCompile(`^\[[\w:.-]+\]\s+(?:[^\s:]+:\s+)?`)
	reportTailRE   = regexp.MustCompile(`(?i)[;.]?\s*(please report this issue|confirm you are on the latest version|` +
		`see\s+https://github\.com/yt-dlp|also see\s+https://).*$`)
)

// FriendlyError reduces a yt-dlp error to one meaningful line: the report-this-
// issue boilerplate goes, and the common dead ends (a broken extractor, a
// login wall, DRM, a geo block) say what they mean.
func FriendlyError(msg string) string {
	m := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg), "ERROR:"))
	if i := strings.IndexByte(m, '\n'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	m = strings.TrimSpace(reportTailRE.ReplaceAllString(m, ""))
	low := strings.ToLower(m)
	has := func(ks ...string) bool {
		for _, k := range ks {
			if strings.Contains(low, k) {
				return true
			}
		}
		return false
	}
	switch {
	case has("the json object must be", "nonetype", "object has no attribute", "keyerror", "typeerror",
		"indexerror", "traceback", "unable to extract", "unexpected response"):
		return "yt-dlp can't read this site right now (the site changed; a yt-dlp update usually fixes it)"
	case has("page needs to be reloaded"):
		return "YouTube refused the request (\"the page needs to be reloaded\") — press Resume to try again; if it keeps happening, update yt-dlp"
	case has("drm", "fairplay", "widevine"):
		return "this video is DRM-protected (encrypted) — it can't be saved"
	case has("logged-in", "logged in", "login required", "log in to", "sign in to", "requires authentication",
		"private video", "members-only", "members only", "--cookies"):
		return "this video needs you to be signed in — open it in your browser with the D BOX extension so D BOX can use your login"
	case has("not available in your country", "geo restrict", "geo-restrict", "not available from your location",
		"not available in your region"):
		return "this video isn't available in your country or region"
	case has("unsupported url"):
		return "no video found on this page (yt-dlp doesn't support this site)"
	case has("requested format is not available"):
		return "the chosen quality isn't available for this video — pick another one"
	}
	m = extractorTagRE.ReplaceAllString(m, "")
	if m == "" {
		return "yt-dlp failed"
	}
	return clip(m)
}

var vimeoIDRE = regexp.MustCompile(`^https?://(?:www\.)?vimeo\.com/(?:channels/[^/]+/)?(\d+)(?:[/?#]|$)`)

// AltURL is another address that gets past an error, or "": vimeo.com/<id>
// "only works when logged-in" for yt-dlp's web client, while the player
// embed of the same public video downloads anonymously.
func AltURL(rawURL, msg string) string {
	if m := vimeoIDRE.FindStringSubmatch(rawURL); m != nil && strings.Contains(strings.ToLower(msg), "logged-in") {
		return "https://player.vimeo.com/video/" + m[1]
	}
	return ""
}
