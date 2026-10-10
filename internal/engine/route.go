package engine

// Routing: the probe can discover that a "file" link is really something else.
// A web page (someone pasted the page, not the video) or a DASH manifest is
// handed to yt-dlp, which knows how to pull the media out; an HLS playlist is
// downloaded by the native HLS engine (hls.go). Either way the task keeps its
// id, folder and queue slot — only its Kind changes.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"myidm/internal/ytdlp"
)

const needTools = `install D BOX with the "Video download tools" option, or put yt-dlp.exe next to DBox.exe`

// convertToVideo turns a probed page/DASH task into a yt-dlp task, asking
// yt-dlp for the real title and an estimated size so the row shows both before
// the transfer starts. A page with several media downloads its main one (not
// the first, which is often a sound effect or an ad); a page whose only "media"
// is a plain file hands that file back to the native downloader
// (errRetryAsFile).
func (e *Engine) convertToVideo(ctx context.Context, t *Task, pr probeResult) error {
	if !ytdlp.Available() {
		if pr.Kind == kindDASH {
			return fmt.Errorf("this is a DASH (.mpd) video stream, which needs yt-dlp — %s", needTools)
		}
		return fmt.Errorf("this link opens a web page, not a file — to grab the video on it D BOX needs yt-dlp (%s)", needTools)
	}
	e.setNote(t, "Finding the video…")
	pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	res, err := ytdlp.Probe(pctx, t.URL, e.ytdlpOpts(t))
	cancel()
	e.setNote(t, "")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if pr.Kind == kindPage {
			return fmt.Errorf("no downloadable video found on this page (%v)", err)
		}
		return fmt.Errorf("yt-dlp: %v", err)
	}
	if res.DirectFile != "" {
		e.mu.Lock()
		if t.Referer == "" {
			t.Referer = t.URL // the page is what a mirror expects as Referer
		}
		t.URL = res.DirectFile
		t.Probed = false
		e.saveLocked()
		e.mu.Unlock()
		e.log.Info("yt-dlp found a plain file behind the page", "id", t.ID, "url", res.DirectFile)
		return errRetryAsFile
	}
	if len(res.Options) == 0 {
		return fmt.Errorf("no downloadable video found on this page")
	}
	best := res.Options[0]
	var entry *ytdlp.ProbeEntry
	for i := range res.Entries {
		if res.Entries[i].Index == res.Recommended {
			entry = &res.Entries[i]
		}
	}
	if pr.Kind == kindPage && !res.HasVideo && onlyClip(best, res.Duration) {
		return errors.New(onlyClipMsg)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	title := ""
	if t.FileName != "" { // the user named it in the New Download box
		if stem, _ := splitExt(t.FileName); !isGenericStem(stem) {
			title = stem
		}
	}
	if title == "" && entry != nil {
		title = cleanTitleFor(entry.Title, t.URL, t.Referer)
	}
	if title == "" {
		title = cleanTitleFor(res.Title, t.URL, t.Referer)
	}
	if title == "" {
		title = streamBaseName(t.Title, t.URL, t.Referer)
	}
	t.Kind = "ytdlp"
	t.Selector = best.Selector
	t.Audio = best.Audio
	t.PlaylistItem = res.Recommended
	t.Title = title
	t.FileName = SanitizeFileName(title) + "." + best.Ext
	t.Probed = true
	t.Size, t.SizeEstimated = -1, false
	t.Segments = []*Segment{{Start: 0, End: -1}}
	if best.Size > 0 {
		t.Size, t.SizeEstimated = best.Size, true
		t.Segments[0].End = best.Size - 1
	}
	e.saveLocked()
	return nil
}

// onlyClip reports sound-only media too small to be what a page link was
// for: arabic-toons' 1.1 MB sound effect beside an episode only the browser
// saw. A long podcast mp3 is downloaded as is.
func onlyClip(best ytdlp.DownOption, duration float64) bool {
	return (best.Size > 0 && best.Size < 2<<20) || (duration > 0 && duration < 60)
}

// adoptStream turns a probed task into a native HLS task, naming it from the
// user's choice, the caller's title hint, or the page it came from — never the
// playlist's own "index.m3u8".
func (e *Engine) adoptStream(t *Task, pr probeResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stem := ""
	if s, _ := splitExt(t.FileName); t.FileName != "" && !isGenericStem(s) {
		stem = s
	}
	if stem == "" {
		stem = streamBaseName(t.Title, t.URL, t.Referer) // the caller's referer, before it's replaced below
	}
	t.Kind = "hls"
	t.Referer = pr.Referer
	t.ContentType = pr.ContentType
	t.FileName = SanitizeFileName(stem) + ".mp4"
	t.Probed = true
	t.Size, t.SizeEstimated = -1, false
	t.Segments = []*Segment{{Start: 0, End: -1}}
	e.saveLocked()
}

// refused reports an HTTP refusal (auth, forbidden, rate limit, bot check)
// that yt-dlp — with browser impersonation — may get past.
func refused(err error) bool {
	var pe *probeStatusError
	if errors.As(err, &pe) {
		return pe.challenge || pe.code == http.StatusUnauthorized || pe.code == http.StatusForbidden || pe.code == http.StatusTooManyRequests
	}
	var he *httpStatusError
	if errors.As(err, &he) {
		return he.challenge || he.code == http.StatusUnauthorized || he.code == http.StatusForbidden || he.code == http.StatusTooManyRequests
	}
	return false
}

// streamLike reports URLs yt-dlp's generic extractor can take on directly:
// stream manifests and media files.
func streamLike(rawURL string) bool {
	_, ext := splitExt(pathName(rawURL))
	return mediaExts[ext] || ext == "m3u8" || ext == "mpd"
}

// handToYtdlp re-routes a stream/media task that the server refused to D BOX's
// own requests (typically a Cloudflare bot check) to yt-dlp, which retries as a
// real browser. Returns true when it took the task over (and drove it to a
// terminal state).
func (e *Engine) handToYtdlp(ctx context.Context, t *Task, cause error) bool {
	if !refused(cause) || !streamLike(t.URL) || !ytdlp.Available() {
		return false
	}
	e.mu.Lock()
	title := ""
	if s, _ := splitExt(t.FileName); t.FileName != "" && !isGenericStem(s) {
		title = s
	}
	if title == "" {
		title = streamBaseName(t.Title, t.URL, t.Referer)
	}
	ext := "mp4"
	if _, x := splitExt(pathName(t.URL)); audioFileExts[x] {
		ext = x // an audio stream stays audio (and isn't failed for having no picture)
	}
	t.Kind = "ytdlp"
	t.Selector = "bv*+ba/b"
	t.Audio = false
	t.PlaylistItem = 0
	t.Title = title
	t.FileName = SanitizeFileName(title) + "." + ext
	t.Probed = true
	t.Live = false
	t.Size, t.SizeEstimated = -1, false
	t.Segments = []*Segment{{Start: 0, End: -1}}
	e.saveLocked()
	e.mu.Unlock()
	e.log.Info("server refused D BOX's requests; handing the stream to yt-dlp", "id", t.ID, "err", cause)
	e.runYtdlp(ctx, t)
	return true
}
