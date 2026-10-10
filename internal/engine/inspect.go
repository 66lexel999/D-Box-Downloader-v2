package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// InspectResult is what the New Download dialog shows before a download starts:
// the real file name, size and kind, found by the same probe the download uses.
// (The dialog used to HEAD the URL from the page itself, which the browser's
// CORS rules block for nearly every site — hence "unknown" sizes and raw URL
// names like "videoplayback".)
type InspectResult struct {
	URL           string  `json:"url"`
	Kind          string  `json:"kind"` // file | hls | dash | page | drm
	FileName      string  `json:"fileName"`
	Size          int64   `json:"size"` // -1 unknown
	SizeEstimated bool    `json:"sizeEstimated,omitempty"`
	ContentType   string  `json:"contentType,omitempty"`
	Resumable     bool    `json:"resumable"`
	Live          bool    `json:"live,omitempty"`
	Duration      float64 `json:"duration,omitempty"` // seconds (streams)
}

// Inspect probes rawURL without creating a task.
func (e *Engine) Inspect(ctx context.Context, rawURL, referer string, headers map[string]string, title string) (InspectResult, error) {
	u, err := parseDownloadURL(rawURL)
	if err != nil {
		return InspectResult{}, err
	}
	ri := reqInfo{Referer: cleanReferer(referer), Headers: CleanHeaders(headers)}
	pr, err := e.probe(ctx, u.String(), ri, title)
	if err != nil {
		return InspectResult{}, err
	}
	res := InspectResult{URL: u.String(), Kind: pr.Kind, FileName: pr.FileName, Size: pr.Size,
		ContentType: pr.ContentType, Resumable: pr.Ranged}
	switch pr.Kind {
	case kindDASH:
		// yt-dlp downloads DASH. Name it like any stream (not "dash.mpd" or
		// "Manifest.mpd"); the manifest's own byte count is not the video's
		// size, which stays unknown until yt-dlp picks the formats.
		res.FileName = streamBaseName(title, u.String(), ri.Referer) + ".mp4"
		res.Size, res.SizeEstimated, res.Resumable = -1, false, false
		return res, nil
	case kindHLS:
	default:
		return res, nil
	}

	// A stream: name it from the page/title, and estimate its size from the
	// best variant's bitrate × duration (one or two small playlist fetches).
	stem := streamBaseName(title, u.String(), ri.Referer)
	res.FileName = stem + ".mp4"
	res.Size, res.Resumable = -1, true
	ri.Referer = pr.Referer
	top, _, err := e.fetchPlaylist(ctx, u.String(), ri)
	if err != nil {
		return res, nil // still a stream; details unknown
	}
	media := top
	var (
		v        hlsVariant
		audioURI string
	)
	if top.Master {
		ranked := rankVariants(top.Variants)
		if len(ranked) == 0 {
			return InspectResult{}, errors.New("this HLS playlist lists no playable streams")
		}
		// The download falls back to the next variants the same way when the
		// best one's playlist is offline (redundant streams).
		var firstErr error
		media = nil
		for i, cand := range ranked {
			if i >= 3 {
				break
			}
			m, _, err := e.fetchPlaylist(ctx, cand.URI, ri)
			if err == nil {
				media, v = m, cand
				break
			}
			if ctx.Err() != nil {
				return InspectResult{}, ctx.Err()
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		if media == nil {
			return InspectResult{}, fmt.Errorf("the stream is offline: its video playlist can't be loaded (%v)", firstErr)
		}
		if a, ok := pickAudio(top, v); ok {
			audioURI = a.URI
		}
	}
	res.Live = media.live()
	res.Duration = media.duration()
	for _, s := range media.Segments {
		for _, k := range []*hlsKey{s.Key, mapKey(s.Map)} {
			switch keyKind(k) {
			case "drm":
				res.Kind = "drm"
				return res, nil
			case "unsupported":
				return InspectResult{}, keyError(k)
			}
		}
	}

	// Audio only? The chosen variant's CODECS say so; a bare media playlist
	// (or a variant without CODECS) is judged by its first segment.
	audio, aext := v.audioOnly(), v.audioExt()
	unknownContent := !top.Master || (len(codecList(v.Codecs)) == 0 && v.Height == 0)
	if unknownContent && len(media.Segments) > 0 {
		switch strings.ToLower(path.Ext(urlPath(media.Segments[0].URI))) {
		case ".aac", ".m4a":
			audio, aext = true, "m4a"
		case ".mp3":
			audio, aext = true, "mp3"
		}
	}

	size := int64(-1)
	var head []byte
	sampled := false
	vExact, vBytes := exactBytes(media)
	switch {
	case res.Live:
	case vExact:
		// Byte ranges: exact — plus the separate audio rendition's bytes.
		size = vBytes
		if audioURI != "" {
			if apl, _, err := e.fetchPlaylist(ctx, audioURI, ri); err == nil {
				if ok, n := exactBytes(apl); ok {
					size += n
				} else if rate, _ := e.sampleSegments(ctx, apl, ri, 1); rate > 0 {
					size += int64(rate*apl.duration() + 0.5)
				}
			}
		}
	case v.sizeRate() > 0 && res.Duration > 0:
		// BANDWIDTH covers the whole variant, separate audio included.
		size = int64(float64(v.sizeRate()) / 8 * res.Duration)
	case len(media.Segments) > 0:
		// No bitrate listed: measure a segment or two.
		rate, h := e.sampleSegments(ctx, media, ri, 2)
		head, sampled = h, true
		if rate > 0 {
			size = int64(rate*res.Duration + 0.5)
			if audioURI != "" {
				if apl, _, err := e.fetchPlaylist(ctx, audioURI, ri); err == nil {
					if ar, _ := e.sampleSegments(ctx, apl, ri, 1); ar > 0 {
						size += int64(ar*apl.duration() + 0.5)
					}
				}
			}
		}
	}
	if unknownContent && !audio && len(media.Segments) > 0 && media.Segments[0].Key == nil {
		if !sampled {
			_, head = e.sampleSegments(ctx, media, ri, 0)
		}
		switch mediaPayload(head) {
		case "aac":
			audio, aext = true, "m4a"
		case "mp3":
			audio, aext = true, "mp3"
		case "ac3":
			audio, aext = true, "ac3"
		}
	}
	if audio {
		res.FileName = stem + "." + aext
	}
	if res.Live {
		res.Resumable = false
		return res, nil
	}
	if size > 0 {
		res.Size, res.SizeEstimated = size, true
	}
	return res, nil
}

// exactBytes sums a playlist's byte ranges; ok only when every segment has one.
func exactBytes(pl *hlsPlaylist) (bool, int64) {
	var n int64
	for _, s := range pl.Segments {
		if s.Len < 0 {
			return false, 0
		}
		n += s.Len
	}
	return len(pl.Segments) > 0, n
}

// urlPath is the path of a URL (for its extension), or the string itself.
func urlPath(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Path
	}
	return raw
}

// sampleSegments measures up to n segments of pl (the first, then one from
// the middle) and returns the media's bytes per second (0 when unknown) and
// the first segment's leading bytes (its format). n = 0 only reads the
// first segment's bytes. Time-boxed: an estimate must not hold up the dialog.
func (e *Engine) sampleSegments(ctx context.Context, pl *hlsPlaylist, ri reqInfo, n int) (float64, []byte) {
	if len(pl.Segments) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	idx := []int{0}
	if n > 1 && len(pl.Segments) > 2 {
		idx = append(idx, len(pl.Segments)/2)
	}
	var (
		total int64
		dur   float64
		head  []byte
	)
	for k, i := range idx {
		s := pl.Segments[i]
		size, h, err := e.segmentHead(ctx, s, ri)
		if err != nil {
			break
		}
		if k == 0 {
			head = h
		}
		if size > 0 && s.Duration > 0 {
			total += size
			dur += s.Duration
		}
	}
	if n == 0 || dur <= 0 {
		return 0, head
	}
	return float64(total) / dur, head
}

// segmentHead asks for a segment's first 4 KB: the answer's Content-Range (or
// Content-Length) is the whole segment's size, the bytes tell its format.
func (e *Engine) segmentHead(ctx context.Context, s hlsSegment, ri reqInfo) (int64, []byte, error) {
	req, err := e.newRequest(ctx, http.MethodGet, s.URI, ri)
	if err != nil {
		return -1, nil, err
	}
	off, end := int64(0), int64(4095)
	if s.Len > 0 {
		off, end = s.Off, s.Off+min(s.Len, 4096)-1
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	resp, err := e.client.Do(req)
	if err != nil {
		return -1, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return -1, nil, &httpStatusError{code: resp.StatusCode, status: resp.Status, challenge: isChallenge(resp)}
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	size := int64(-1)
	switch {
	case s.Len >= 0:
		size = s.Len
		if resp.StatusCode != http.StatusPartialContent && off > 0 {
			head = nil // the whole resource came back: these bytes are another segment's
		}
	case resp.StatusCode == http.StatusPartialContent:
		if t, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok {
			size = t
		}
	case resp.ContentLength > 0:
		size = resp.ContentLength
	}
	return size, head, nil
}
