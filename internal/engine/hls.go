package engine

// Native HLS (.m3u8) downloader — the format most "random" web video and live
// streams use. Before this, an .m3u8 link was saved as the tiny playlist text
// file itself (named "index.m3u8", size a few KB) instead of the video.
//
//   - Master playlists: the best variant is chosen (highest resolution, then
//     bitrate), plus its separate audio rendition when the audio isn't muxed in.
//   - Segments download in parallel into a work folder, one file per media
//     sequence number, so a paused download resumes exactly where it stopped.
//   - AES-128 encryption (keys fetched with the same Referer/cookies), byte-
//     range playlists and fMP4 init sections (EXT-X-MAP) are supported. DRM
//     (SAMPLE-AES / FairPlay / Widevine) is reported clearly — it can't be saved.
//   - Size: exact when the playlist lists byte ranges, otherwise estimated from
//     the variant bitrate and refined from real segment sizes as they arrive.
//   - Live streams are RECORDED: new segments are fetched as the playlist grows;
//     Pause ("Stop") saves what was captured as a finished file.
//   - The result is rewrapped to MP4 by ffmpeg (fetched on demand); without
//     ffmpeg a playable .ts (or fMP4) is kept.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"myidm/internal/ytdlp"
)

const (
	hlsSegmentRetries = 5
	hlsMaxSegment     = 512 << 20 // a single segment larger than this is not a sane HLS stream
	hlsMaxPlaylist    = 32 << 20
)

// hlsTrack is one media playlist being captured: the video variant, or the
// separate audio rendition.
type hlsTrack struct {
	name string // "video" / "audio" (also its folder name)
	url  string // media playlist URL (post-redirect)
	pl   *hlsPlaylist
	dir  string

	mu        sync.Mutex
	totalDur  float64
	doneDur   float64
	doneBytes int64
	maps      map[int64]*hlsMap // init section per segment seq (fMP4)

	audioOnly bool   // the chosen variant carries no picture (by its CODECS)
	audioExt  string // "m4a" or "mp3" for an audio-only variant

	// A fresh copy of the media playlist, re-read when a segment answers with
	// a web page (its token expired) — see refreshSegment.
	refreshMu sync.Mutex
	fresh     *hlsPlaylist
	freshAt   time.Time
	refreshes int
}

func (tr *hlsTrack) addDone(dur float64, n int64) {
	tr.mu.Lock()
	tr.doneDur += dur
	tr.doneBytes += n
	tr.mu.Unlock()
}

func (tr *hlsTrack) rememberMap(seq int64, m *hlsMap) {
	if m == nil {
		return
	}
	tr.mu.Lock()
	if tr.maps == nil {
		tr.maps = map[int64]*hlsMap{}
	}
	tr.maps[seq] = m
	tr.mu.Unlock()
}

// httpStatusError is a non-success HTTP answer for a segment/key/playlist.
type httpStatusError struct {
	code      int
	status    string
	challenge bool // Cloudflare bot check
}

func (h *httpStatusError) Error() string {
	if h.challenge {
		return "server returned " + h.status + " — the site's Cloudflare bot check blocked the request"
	}
	return "server returned " + h.status
}

// permanent reports whether retrying cannot help (4xx other than timeouts and
// rate limits).
func (h *httpStatusError) permanent() bool {
	return h.code >= 400 && h.code < 500 && h.code != http.StatusRequestTimeout && h.code != http.StatusTooManyRequests
}

// hlsWorkDir is where a stream's segments collect until it is assembled.
func (e *Engine) hlsWorkDir(t *Task) string {
	stem, _ := splitExt(t.FileName)
	if stem == "" {
		stem = "stream"
	}
	return filepath.Join(t.Dir, fmt.Sprintf("%s.%s.hls-parts", stem, t.ID))
}

// appCtx is the app-lifetime context (for work that must outlive a paused
// task's context, like saving a stopped live recording).
func (e *Engine) appCtx() context.Context {
	if e.rootCtx != nil {
		return e.rootCtx
	}
	return context.Background()
}

// hlsReqInfo adds the Origin header hls.js-style players send on cross-site
// segment requests; some CDNs check it alongside the Referer.
func hlsReqInfo(t *Task) reqInfo {
	ri := t.reqInfo()
	if ri.Referer == "" {
		return ri
	}
	for k := range ri.Headers {
		if strings.EqualFold(k, "Origin") {
			return ri
		}
	}
	if o := originOf(ri.Referer); o != "" && o != originOf(t.URL) {
		h := make(map[string]string, len(ri.Headers)+1)
		for k, v := range ri.Headers {
			h[k] = v
		}
		h["Origin"] = o
		ri.Headers = h
	}
	return ri
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// runHLS drives an HLS task to a terminal state.
func (e *Engine) runHLS(ctx context.Context, t *Task) {
	fail := func(err error) {
		if ctx.Err() != nil {
			e.finishInterrupted(t)
			return
		}
		e.finishTask(t, err)
	}
	ri := hlsReqInfo(t)

	e.setNote(t, "Reading playlist…")
	top, topURL, err := e.fetchPlaylist(ctx, t.URL, ri)
	if err != nil {
		if ctx.Err() == nil && e.handToYtdlp(ctx, t, err) {
			return
		}
		fail(fmt.Errorf("playlist: %w", err))
		return
	}
	var (
		tracks    []*hlsTrack
		bandwidth int64
	)
	if top.Master {
		ranked := rankVariants(top.Variants)
		if len(ranked) == 0 {
			fail(errors.New("this HLS playlist lists no playable streams"))
			return
		}
		// The best variant first; when its playlist is offline (redundant
		// streams list a backup server) the next ones.
		var (
			v        hlsVariant
			vpl      *hlsPlaylist
			vurl     string
			firstErr error
		)
		for i, cand := range ranked {
			if i >= 3 || ctx.Err() != nil {
				break
			}
			pl, u, err := e.fetchPlaylist(ctx, cand.URI, ri)
			if err == nil {
				v, vpl, vurl = cand, pl, u
				break
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		if vpl == nil {
			if ctx.Err() == nil && firstErr != nil && e.handToYtdlp(ctx, t, firstErr) {
				return
			}
			fail(fmt.Errorf("video playlist: %w", firstErr))
			return
		}
		bandwidth = v.sizeRate()
		tracks = append(tracks, &hlsTrack{name: "video", url: vurl, pl: vpl, audioOnly: v.audioOnly(), audioExt: v.audioExt()})
		if a, ok := pickAudio(top, v); ok {
			apl, aurl, err := e.fetchPlaylist(ctx, a.URI, ri)
			if err != nil {
				fail(fmt.Errorf("audio playlist: %w", err))
				return
			}
			tracks = append(tracks, &hlsTrack{name: "audio", url: aurl, pl: apl})
		}
	} else {
		tracks = []*hlsTrack{{name: "video", url: topURL, pl: top}}
	}
	for _, tr := range tracks {
		if tr.pl.Master {
			fail(errors.New("unsupported HLS layout (a master playlist inside a master playlist)"))
			return
		}
		if err := checkHLSKeys(tr.pl); err != nil {
			fail(err)
			return
		}
	}
	live := tracks[0].pl.live()

	// ffmpeg merges separate audio and turns MPEG-TS into MP4. Fetch it (once)
	// in the background while the segments download.
	ffReady := e.prefetchFFmpeg()

	work := e.hlsWorkDir(t)
	for _, tr := range tracks {
		tr.dir = filepath.Join(work, tr.name)
		if err := os.MkdirAll(tr.dir, 0o755); err != nil {
			fail(err)
			return
		}
	}
	if !live {
		hlsCheckResume(work, tracks)
	}

	e.mu.Lock()
	t.Live = live
	t.note = ""
	if live {
		t.note = "● Recording"
		t.Size, t.SizeEstimated = -1, false
		t.Segments[0].End = -1
	}
	e.saveLocked()
	e.mu.Unlock()

	keys := &hlsKeyCache{m: map[string][]byte{}}
	if live {
		err = e.hlsRecord(ctx, t, tracks, ri, keys)
	} else {
		err = e.hlsFetchAll(ctx, t, tracks, ri, keys, bandwidth)
	}

	if ctx.Err() != nil {
		e.mu.Lock()
		stop := live && t.intent == intentPause
		if stop {
			t.intent = intentNone
		}
		e.mu.Unlock()
		if !stop || !hlsHasSegments(tracks) {
			e.finishInterrupted(t) // VOD pause keeps the parts for resume
			return
		}
		// Pausing a live recording means "stop and save what we have".
	} else if err != nil {
		e.finishTask(t, err)
		return
	}
	e.hlsFinalize(t, tracks, ffReady)
}

// prefetchFFmpeg starts fetching ffmpeg if it's missing; the channel closes
// when it is available (or definitively not).
func (e *Engine) prefetchFFmpeg() <-chan struct{} {
	ch := make(chan struct{})
	if ytdlp.HasFFmpeg() {
		close(ch)
		return ch
	}
	go func() {
		defer close(ch)
		ctx, cancel := context.WithTimeout(e.appCtx(), 20*time.Minute)
		defer cancel()
		if _, err := provisionFFmpeg(ctx, nil); err != nil && e.log != nil {
			e.log.Info("ffmpeg not available — HLS captures will be saved as .ts", "err", err)
		}
	}()
	return ch
}

// checkHLSKeys rejects encryption we can't decrypt, with a clear reason.
func checkHLSKeys(pl *hlsPlaylist) error {
	for _, s := range pl.Segments {
		for _, k := range []*hlsKey{s.Key, mapKey(s.Map)} {
			if err := keyError(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// keyKind sorts an EXT-X-KEY: "" = full-segment AES-128/AES-256(-CTR) with a plain
// key (decryptable), "drm" = a DRM key system or sample-level encryption,
// "unsupported" = another scheme.
func keyKind(k *hlsKey) string {
	if k == nil {
		return ""
	}
	kf := strings.ToLower(strings.TrimSpace(k.KeyFormat))
	switch {
	case strings.HasPrefix(k.URI, "skd:"), strings.HasPrefix(k.Method, "SAMPLE-AES"), k.Method == "ISO-23001-7":
		return "drm"
	case kf != "" && kf != "identity":
		return "drm" // FairPlay (com.apple.streamingkeydelivery), Widevine, PlayReady
	case k.Method == "AES-128" || k.Method == "AES-256" || k.Method == "AES-256-CTR":
		return ""
	}
	return "unsupported"
}

// keyLen is the key size a decryptable method uses.
func keyLen(method string) int {
	if method == "AES-256" || method == "AES-256-CTR" {
		return 32
	}
	return aes.BlockSize
}

// keyError explains why a key can't be used, or nil.
func keyError(k *hlsKey) error {
	switch keyKind(k) {
	case "drm":
		return fmt.Errorf("this stream is DRM-protected (%s) and can't be downloaded", k.Method)
	case "unsupported":
		return fmt.Errorf("this stream uses %s encryption, which isn't supported", k.Method)
	}
	if k != nil && k.URI == "" {
		return errors.New("encrypted HLS stream without a key address")
	}
	return nil
}

func mapKey(m *hlsMap) *hlsKey {
	if m == nil {
		return nil
	}
	return m.Key
}

// fetchPlaylist downloads and parses a playlist, returning its final URL.
func (e *Engine) fetchPlaylist(ctx context.Context, rawURL string, ri reqInfo) (*hlsPlaylist, string, error) {
	var body []byte
	final := rawURL
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt)*time.Second); err != nil {
				return nil, "", err
			}
		}
		req, err := e.newRequest(ctx, http.MethodGet, rawURL, ri)
		if err != nil {
			return nil, "", err
		}
		resp, err := e.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			se := &httpStatusError{code: resp.StatusCode, status: resp.Status, challenge: isChallenge(resp)}
			if se.permanent() {
				return nil, "", se
			}
			lastErr = se
			continue
		}
		// Some servers gzip playlists although we asked for identity.
		rd, _ := decodeBody(resp.Body, resp.Header.Get("Content-Encoding"))
		body, err = io.ReadAll(io.LimitReader(rd, hlsMaxPlaylist))
		if err == nil {
			body, err = gunzipIfMagic(body)
		}
		if resp.Request != nil && resp.Request.URL != nil {
			final = resp.Request.URL.String()
		}
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	base, _ := url.Parse(final)
	pl, err := parseHLS(string(body), base)
	if err != nil {
		return nil, "", err
	}
	return pl, final, nil
}

// gunzipIfMagic decodes a playlist that is still gzip data (sent without a
// Content-Encoding header, or double-encoded).
func gunzipIfMagic(b []byte) ([]byte, error) {
	for i := 0; i < 2 && len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b; i++ {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return b, nil
		}
		out, err := io.ReadAll(io.LimitReader(zr, hlsMaxPlaylist))
		if err != nil {
			return nil, fmt.Errorf("gzip playlist: %w", err)
		}
		b = out
	}
	return b, nil
}

// hlsCheckResume drops a previous session's segments when the playlist no
// longer matches them (a different rendition or a re-cut stream), recorded in
// the work folder's info.json.
func hlsCheckResume(work string, tracks []*hlsTrack) {
	type trackInfo struct {
		First int64 `json:"first"`
		Count int   `json:"count"`
	}
	cur := map[string]trackInfo{}
	for _, tr := range tracks {
		ti := trackInfo{Count: len(tr.pl.Segments)}
		if len(tr.pl.Segments) > 0 {
			ti.First = tr.pl.Segments[0].Seq
		}
		cur[tr.name] = ti
	}
	infoPath := filepath.Join(work, "info.json")
	var prev map[string]trackInfo
	if b, err := os.ReadFile(infoPath); err == nil && json.Unmarshal(b, &prev) == nil {
		for _, tr := range tracks {
			if p, ok := prev[tr.name]; ok && p != cur[tr.name] {
				os.RemoveAll(tr.dir)
				os.MkdirAll(tr.dir, 0o755)
			}
		}
	}
	if b, err := json.Marshal(cur); err == nil {
		os.WriteFile(infoPath, b, 0o644)
	}
}

// segPath / mapPath name a segment and an init section inside a track folder.
func segPath(dir string, seq int64) string {
	return filepath.Join(dir, strconv.FormatInt(seq, 10)+".seg")
}

func mapPath(dir string, m *hlsMap) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%d|%d", m.URI, m.Off, m.Len)
	return filepath.Join(dir, fmt.Sprintf("init-%016x.bin", h.Sum64()))
}

// hlsProgress aggregates received bytes (finished + in-flight) into the task's
// synthetic segment.
type hlsProgress struct {
	t     *Task
	bytes atomic.Int64
}

func (p *hlsProgress) add(n int64) {
	v := p.bytes.Add(n)
	if len(p.t.Segments) > 0 {
		p.t.Segments[0].SetDone(v)
	}
}

type hlsJob struct {
	tr  *hlsTrack
	seg hlsSegment
}

// hlsFetchAll downloads every segment of a finished (VOD) stream.
func (e *Engine) hlsFetchAll(ctx context.Context, t *Task, tracks []*hlsTrack, ri reqInfo, keys *hlsKeyCache, bandwidth int64) error {
	prog := &hlsProgress{t: t}
	var (
		jobs       []hlsJob
		knownTotal int64
		allKnown   = true
	)
	for _, tr := range tracks {
		tr.totalDur = tr.pl.duration()
		for _, s := range tr.pl.Segments {
			tr.rememberMap(s.Seq, s.Map)
			if s.Len < 0 {
				allKnown = false
			} else {
				knownTotal += s.Len
			}
			if fi, err := os.Stat(segPath(tr.dir, s.Seq)); err == nil && fi.Size() > 0 {
				tr.addDone(s.Duration, fi.Size()) // fetched by a previous session
				prog.add(fi.Size())
				continue
			}
			jobs = append(jobs, hlsJob{tr, s})
		}
		if err := e.hlsFetchMaps(ctx, tr, ri, keys); err != nil {
			return err
		}
	}
	est := int64(-1)
	switch {
	case allKnown && knownTotal > 0:
		est = knownTotal
	case bandwidth > 0 && tracks[0].totalDur > 0:
		est = int64(float64(bandwidth) / 8 * tracks[0].totalDur)
	}
	if e2 := hlsEstimate(tracks); e2 > 0 && !allKnown {
		est = e2 // resumed: real sizes beat the bitrate guess
	}
	e.hlsSetSize(t, est)
	e.setNote(t, "")

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
		next     atomic.Int64
	)
	workers := hlsWorkers(t)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(jobs) || wctx.Err() != nil {
					return
				}
				j := jobs[i]
				n, err := e.fetchSegment(wctx, j.seg, ri, keys, segPath(j.tr.dir, j.seg.Seq), prog.add)
				// A web page instead of media: the segment's token most likely
				// expired. Re-read the playlist for fresh addresses, twice.
				for r := 0; r < 2 && err != nil && errors.Is(err, errNotMedia) && wctx.Err() == nil; r++ {
					fresh, ok := e.refreshSegment(wctx, j.tr, j.seg, ri)
					if !ok {
						break
					}
					n, err = e.fetchSegment(wctx, fresh, ri, keys, segPath(j.tr.dir, j.seg.Seq), prog.add)
				}
				if err != nil {
					if wctx.Err() == nil {
						errOnce.Do(func() {
							firstErr = fmt.Errorf("segment %d: %w", j.seg.Seq, err)
							cancel()
						})
					}
					return
				}
				j.tr.addDone(j.seg.Duration, n)
				if !allKnown {
					e.hlsSetSize(t, hlsEstimate(tracks))
				}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return firstErr
}

// refreshSegment re-reads a track's media playlist and returns seg as it is
// listed now (same sequence number, fresh URL). Workers hitting expired
// tokens together share one re-read; a track re-reads at most 8 times.
func (e *Engine) refreshSegment(ctx context.Context, tr *hlsTrack, seg hlsSegment, ri reqInfo) (hlsSegment, bool) {
	tr.refreshMu.Lock()
	defer tr.refreshMu.Unlock()
	if tr.fresh == nil || time.Since(tr.freshAt) > 3*time.Second {
		if tr.refreshes >= 8 || tr.url == "" {
			return seg, false
		}
		tr.refreshes++
		pl, _, err := e.fetchPlaylist(ctx, tr.url, ri)
		if err != nil || pl.Master {
			return seg, false
		}
		tr.fresh, tr.freshAt = pl, time.Now()
	}
	for _, s := range tr.fresh.Segments {
		if s.Seq == seg.Seq {
			return s, true
		}
	}
	return seg, false
}

// hlsWorkers is how many segments download at once.
func hlsWorkers(t *Task) int {
	n := t.WantSegments
	if n < 1 {
		n = 8
	}
	if n < 2 {
		n = 2
	}
	if n > 16 {
		n = 16
	}
	return n
}

// hlsEstimate projects the final size from the segments fetched so far:
// bytes-per-second of media actually received, times the total duration.
func hlsEstimate(tracks []*hlsTrack) int64 {
	var total int64
	for _, tr := range tracks {
		tr.mu.Lock()
		dd, db, td := tr.doneDur, tr.doneBytes, tr.totalDur
		tr.mu.Unlock()
		if dd <= 0 || td <= 0 {
			return -1 // not enough data for this track yet
		}
		total += int64(float64(db) / dd * td)
	}
	return total
}

// hlsSetSize records an (estimated) total size.
func (e *Engine) hlsSetSize(t *Task, est int64) {
	if est <= 0 {
		return
	}
	e.mu.Lock()
	t.Size = est
	t.SizeEstimated = true
	if len(t.Segments) > 0 {
		t.Segments[0].End = est - 1
	}
	e.mu.Unlock()
}

// hlsRecord captures a live stream until it ends or the user stops it.
func (e *Engine) hlsRecord(ctx context.Context, t *Task, tracks []*hlsTrack, ri reqInfo, keys *hlsKeyCache) error {
	prog := &hlsProgress{t: t}
	seen := make([]map[int64]bool, len(tracks))
	var recorded atomic.Int64 // milliseconds of video captured
	for i, tr := range tracks {
		seen[i] = map[int64]bool{}
		// Segments from an earlier session (the app was closed mid-recording).
		if ents, err := os.ReadDir(tr.dir); err == nil {
			for _, en := range ents {
				if seq, ok := parseSegName(en.Name()); ok {
					seen[i][seq] = true
					if fi, err := en.Info(); err == nil {
						prog.add(fi.Size())
					}
				}
			}
		}
	}

	jobs := make(chan hlsJob, 64)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue // drain
				}
				n, err := e.fetchSegment(ctx, j.seg, ri, keys, segPath(j.tr.dir, j.seg.Seq), prog.add)
				if err != nil {
					// Live segments expire fast; skipping one beats ending the recording.
					if ctx.Err() == nil && e.log != nil {
						e.log.Info("live segment skipped", "id", t.ID, "seq", j.seg.Seq, "err", err)
					}
					continue
				}
				j.tr.addDone(j.seg.Duration, n)
				if j.tr.name == "video" {
					ms := recorded.Add(int64(j.seg.Duration * 1000))
					e.setNote(t, "● Recording "+clockText(time.Duration(ms)*time.Millisecond))
				}
			}
		}()
	}
	defer func() {
		close(jobs)
		wg.Wait()
	}()

	// A sliding-window live playlist can hold a long DVR window (30+ minutes):
	// start at the live edge like a player does, not at the window's start.
	for i, tr := range tracks {
		if len(seen[i]) > 0 {
			continue // resuming an earlier session's recording
		}
		for _, s := range tr.pl.Segments[:liveEdgeStart(tr.pl)] {
			seen[i][s.Seq] = true
		}
	}

	failures := 0
	for {
		for i, tr := range tracks {
			if err := e.hlsFetchMaps(ctx, tr, ri, keys); err != nil && ctx.Err() == nil && e.log != nil {
				e.log.Info("live init section failed", "id", t.ID, "err", err)
			}
			for _, s := range tr.pl.Segments {
				if seen[i][s.Seq] {
					continue
				}
				seen[i][s.Seq] = true
				tr.rememberMap(s.Seq, s.Map)
				select {
				case jobs <- hlsJob{tr, s}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if !tracks[0].pl.live() {
			return nil // the broadcast ended: save it
		}
		if err := sleepCtx(ctx, refreshInterval(tracks[0].pl)); err != nil {
			return err
		}
		ok := true
		for _, tr := range tracks {
			pl, _, err := e.fetchPlaylist(ctx, tr.url, ri)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				ok = false
				break
			}
			tr.pl = pl
		}
		if ok {
			failures = 0
			continue
		}
		// The playlist vanished: the stream most likely ended. Save what we have.
		if failures++; failures >= 6 {
			return nil
		}
	}
}

// liveEdgeStart is the index of the first segment a new recording captures:
// the last three of a sliding window (RFC 8216 6.3.3: players start three
// target durations from the end). EVENT playlists keep everything — they
// grow from the event's start, which the viewer may want.
func liveEdgeStart(pl *hlsPlaylist) int {
	n := len(pl.Segments)
	if n <= 3 || pl.PlaylistType != "" {
		return 0
	}
	return n - 3
}

// refreshInterval is how long to wait before re-reading a live playlist: half
// the target segment duration, within sane bounds.
func refreshInterval(pl *hlsPlaylist) time.Duration {
	d := time.Duration(pl.TargetDuration * float64(time.Second) / 2)
	if d < time.Second {
		d = time.Second
	}
	if d > 6*time.Second {
		d = 6 * time.Second
	}
	return d
}

func clockText(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func parseSegName(name string) (int64, bool) {
	if !strings.HasSuffix(name, ".seg") {
		return 0, false
	}
	seq, err := strconv.ParseInt(strings.TrimSuffix(name, ".seg"), 10, 64)
	return seq, err == nil
}

func hlsHasSegments(tracks []*hlsTrack) bool {
	ents, err := os.ReadDir(tracks[0].dir)
	if err != nil {
		return false
	}
	for _, en := range ents {
		if _, ok := parseSegName(en.Name()); ok {
			return true
		}
	}
	return false
}

// hlsFetchMaps downloads the fMP4 init sections a track's segments reference.
func (e *Engine) hlsFetchMaps(ctx context.Context, tr *hlsTrack, ri reqInfo, keys *hlsKeyCache) error {
	done := map[string]bool{}
	for _, s := range tr.pl.Segments {
		m := s.Map
		if m == nil {
			continue
		}
		p := mapPath(tr.dir, m)
		if done[p] {
			continue
		}
		done[p] = true
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			continue
		}
		seg := hlsSegment{URI: m.URI, Off: m.Off, Len: m.Len, Key: m.Key, Seq: s.Seq}
		if m.Key != nil && len(m.Key.IV) == 0 {
			seg.Key = nil // an init section is only encrypted when the key carries an explicit IV
		}
		if _, err := e.fetchSegment(ctx, seg, ri, keys, p, nil); err != nil {
			return fmt.Errorf("init section: %w", err)
		}
	}
	return nil
}

// fetchSegment downloads (and decrypts) one segment into dest, retrying
// transient failures. onBytes (optional) tracks received bytes live; bytes of
// a failed attempt are subtracted again.
func (e *Engine) fetchSegment(ctx context.Context, seg hlsSegment, ri reqInfo, keys *hlsKeyCache, dest string, onBytes func(int64)) (int64, error) {
	var lastErr error
	for attempt := 0; attempt < hlsSegmentRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			if err := sleepCtx(ctx, backoff); err != nil {
				return 0, err
			}
		}
		data, err := e.getBytes(ctx, seg.URI, seg.Off, seg.Len, ri, onBytes)
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			var se *httpStatusError
			if errors.As(err, &se) && se.permanent() {
				return 0, err
			}
			lastErr = err
			continue
		}
		if len(data) == 0 || textualSample(data[:min(len(data), 512)]) {
			// An HTML/JSON/text answer with status 200: an expired token, a
			// hotlink refusal or a "not found" page. Never save it as video.
			return 0, notMediaError(data)
		}
		if seg.Key != nil {
			if err := keyError(seg.Key); err != nil { // a live stream switched to DRM mid-way
				return 0, err
			}
			key, err := keys.get(ctx, e, seg.Key.URI, ri, keyLen(seg.Key.Method))
			if err != nil {
				return 0, fmt.Errorf("decryption key: %w", err)
			}
			iv := seg.Key.IV
			if len(iv) != aes.BlockSize {
				iv = seqIV(seg.Seq)
			}
			if seg.Key.Method == "AES-256-CTR" {
				data, err = aesCTR(data, key, iv)
			} else {
				data, err = aes128Decrypt(data, key, iv)
			}
			if err != nil {
				return 0, err
			}
		}
		if data, err = stripDisguise(data); err != nil {
			return 0, err
		}
		tmp := dest + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return 0, err
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			return 0, err
		}
		return int64(len(data)), nil
	}
	return 0, lastErr
}

// getBytes GETs a whole resource or a byte range of it into memory.
func (e *Engine) getBytes(ctx context.Context, rawURL string, off, n int64, ri reqInfo, onBytes func(int64)) (data []byte, err error) {
	req, err := e.newRequest(ctx, http.MethodGet, rawURL, ri)
	if err != nil {
		return nil, err
	}
	if n >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpStatusError{code: resp.StatusCode, status: resp.Status, challenge: isChallenge(resp)}
	}
	if resp.ContentLength > hlsMaxSegment {
		return nil, fmt.Errorf("segment too large (%d bytes)", resp.ContentLength)
	}
	var counted int64
	defer func() {
		if err != nil && onBytes != nil && counted > 0 {
			onBytes(-counted) // this attempt's bytes don't count
		}
	}()
	var buf bytes.Buffer
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	}
	chunk := make([]byte, 64<<10)
	for {
		k, rerr := resp.Body.Read(chunk)
		if k > 0 {
			if err := e.limiter.Take(ctx, k); err != nil {
				return nil, err
			}
			buf.Write(chunk[:k])
			counted += int64(k)
			if onBytes != nil {
				onBytes(int64(k))
			}
			if buf.Len() > hlsMaxSegment {
				return nil, errors.New("segment too large")
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if resp.ContentLength >= 0 && int64(buf.Len()) != resp.ContentLength {
		return nil, fmt.Errorf("connection closed early (%d of %d bytes)", buf.Len(), resp.ContentLength)
	}
	out := buf.Bytes()
	if n >= 0 && resp.StatusCode != http.StatusPartialContent {
		// The server ignored our Range and sent the whole file: cut the part out.
		if int64(len(out)) < off+n {
			return nil, fmt.Errorf("byte range %d-%d beyond end of %d-byte resource", off, off+n-1, len(out))
		}
		out = out[off : off+n]
	}
	return out, nil
}

// hlsKeyCache holds fetched AES keys by URI.
type hlsKeyCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (c *hlsKeyCache) get(ctx context.Context, e *Engine, uri string, ri reqInfo, want int) ([]byte, error) {
	c.mu.Lock()
	if k, ok := c.m[uri]; ok {
		c.mu.Unlock()
		return k, nil
	}
	c.mu.Unlock()

	var raw []byte
	if strings.HasPrefix(uri, "data:") {
		_, payload, ok := strings.Cut(uri, ",")
		if !ok {
			return nil, errors.New("malformed data: key")
		}
		if strings.Contains(strings.SplitN(uri, ",", 2)[0], ";base64") {
			b, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return nil, err
			}
			raw = b
		} else {
			p, _ := url.PathUnescape(payload)
			raw = []byte(p)
		}
	} else {
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				if serr := sleepCtx(ctx, time.Duration(attempt)*time.Second); serr != nil {
					return nil, serr
				}
			}
			raw, err = e.getBytes(ctx, uri, 0, -1, ri, nil)
			if err == nil {
				break
			}
			var se *httpStatusError
			if errors.As(err, &se) && se.permanent() {
				break
			}
		}
		if err != nil {
			return nil, err
		}
	}
	key := normalizeKeyLen(raw, want)
	if len(key) != want {
		return nil, fmt.Errorf("unexpected key length %d (want %d)", len(raw), want)
	}
	c.mu.Lock()
	c.m[uri] = key
	c.mu.Unlock()
	return key, nil
}

// normalizeKey accepts a raw 16-byte key, or one served as 32 hex digits.
func normalizeKey(b []byte) []byte { return normalizeKeyLen(b, aes.BlockSize) }

// normalizeKeyLen accepts a raw key of want bytes, or one served as hex.
func normalizeKeyLen(b []byte, want int) []byte {
	if len(b) == want {
		return b
	}
	if s := strings.TrimSpace(string(b)); len(s) == 2*want {
		if k, err := hex.DecodeString(s); err == nil {
			return k
		}
	}
	return b
}

// seqIV is the default AES IV: the media sequence number, big-endian, in 16 bytes.
func seqIV(seq int64) []byte {
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], uint64(seq))
	return iv
}

// aesCTR decrypts AES-CTR (the IV is the initial counter block; no padding).
func aesCTR(data, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCTR(block, iv).XORKeyStream(out, data)
	return out, nil
}

// aes128Decrypt decrypts AES-CBC (AES-128, or AES-256 with a 32-byte key) and
// strips PKCS#7 padding.
func aes128Decrypt(data, key, iv []byte) ([]byte, error) {
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted segment is %d bytes, not a multiple of 16", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	if n := len(out); n > 0 {
		p := int(out[n-1])
		if p >= 1 && p <= aes.BlockSize && p <= n && bytes.Equal(out[n-p:], bytes.Repeat([]byte{byte(p)}, p)) {
			out = out[:n-p]
		}
	}
	return out, nil
}

// hlsFinalize joins the captured segments, muxes/remuxes to MP4 when ffmpeg is
// available, and completes the task.
func (e *Engine) hlsFinalize(t *Task, tracks []*hlsTrack, ffReady <-chan struct{}) {
	ctx, cancel := context.WithTimeout(e.appCtx(), 60*time.Minute)
	defer cancel()
	if t.Live {
		e.setNote(t, "Saving recording…")
	} else {
		e.setNote(t, "Joining…")
	}
	work := e.hlsWorkDir(t)
	raw := make([]string, len(tracks))
	exts := make([]string, len(tracks))
	for i, tr := range tracks {
		p, ext, err := assembleTrack(tr, filepath.Join(work, tr.name))
		if err != nil {
			e.finishTask(t, fmt.Errorf("join segments: %w", err))
			return
		}
		raw[i], exts[i] = p, ext
	}

	select {
	case <-ffReady:
	default:
		e.setNote(t, "Getting ffmpeg (one-time)…")
		select {
		case <-ffReady:
		case <-ctx.Done():
		}
	}
	haveFF := ytdlp.HasFFmpeg()
	stem, _ := splitExt(t.FileName)
	audioOnly := len(tracks) == 1 && (tracks[0].audioOnly || exts[0] == "aac" || exts[0] == "mp3" || exts[0] == "ac3")
	// MP3 / AC-3 audio stays as it is: ffmpeg's .m4a muxer refuses MP3, and
	// the raw file plays everywhere.
	keepRaw := len(tracks) == 1 && (exts[0] == "mp3" || exts[0] == "ac3")

	final := ""
	switch {
	case len(tracks) == 2:
		if !haveFF {
			e.finishTask(t, errors.New("this stream keeps its audio separate and merging it needs ffmpeg, which couldn't be downloaded — check your connection and press Resume"))
			return
		}
		e.setNote(t, "Merging…")
		out := uniquePath(filepath.Join(t.Dir, stem+".mp4"))
		if err := ytdlp.MuxCmd(ctx, raw[0], raw[1], out).Run(); err != nil {
			os.Remove(out)
			e.finishTask(t, fmt.Errorf("merge audio and video: %w", err))
			return
		}
		final = out
	case haveFF && !keepRaw:
		e.setNote(t, "Converting to MP4…")
		ext := "mp4"
		if audioOnly {
			ext = "m4a"
		}
		out := uniquePath(filepath.Join(t.Dir, stem+"."+ext))
		err := ytdlp.RemuxCmd(ctx, raw[0], out).Run()
		if err == nil && !remuxLooksEmpty(raw[0], out) {
			final = out
		} else {
			os.Remove(out) // keep the playable raw capture instead
			if e.log != nil {
				e.log.Info("remux failed; keeping the raw stream", "id", t.ID, "err", err)
			}
		}
	}
	if final == "" {
		out := uniquePath(filepath.Join(t.Dir, stem+"."+exts[0]))
		if err := os.Rename(raw[0], out); err != nil {
			e.finishTask(t, fmt.Errorf("finalize: %w", err))
			return
		}
		final = out
	}
	os.RemoveAll(work)
	e.completeTask(t, final)
	if e.log != nil {
		e.log.Info("HLS task completed", "id", t.ID, "file", final)
	}
	e.notifyCompleted(t.ID)
}

// assembleTrack concatenates a track's downloaded segments (in sequence order,
// with their fMP4 init sections) into one file. Returns its path and the
// container extension it holds.
func assembleTrack(tr *hlsTrack, base string) (string, string, error) {
	ents, err := os.ReadDir(tr.dir)
	if err != nil {
		return "", "", err
	}
	var seqs []int64
	for _, en := range ents {
		if seq, ok := parseSegName(en.Name()); ok {
			seqs = append(seqs, seq)
		}
	}
	if len(seqs) == 0 {
		return "", "", errors.New("no segments were downloaded")
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	var fallbackMap *hlsMap
	if tr.pl != nil && len(tr.pl.Segments) > 0 {
		fallbackMap = tr.pl.Segments[0].Map
	}
	ext := ""
	if fallbackMap != nil || len(tr.maps) > 0 {
		ext = "mp4"
	} else {
		// Every segment passed fetchSegment's checks, but a junk capture (a
		// decoy stream, a site's binary error blob) must fail here clearly
		// rather than "complete" as an empty video.
		for i := 0; i < len(seqs) && i < 5 && ext == ""; i++ {
			if head, err := readHead(segPath(tr.dir, seqs[i]), 4096); err == nil {
				ext = mediaPayload(head)
			}
		}
		if ext == "" {
			return "", "", errors.New("the downloaded segments aren't audio or video (the site sent something else, often for an expired link) — start the download again from the video's page")
		}
	}
	out := base + "." + ext
	f, err := os.Create(out)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	lastMap := ""
	for _, seq := range seqs {
		m := tr.maps[seq]
		if m == nil {
			m = fallbackMap
		}
		if m != nil {
			if mp := mapPath(tr.dir, m); mp != lastMap {
				if err := appendFile(f, mp); err != nil {
					return "", "", fmt.Errorf("init section: %w", err)
				}
				lastMap = mp
			}
		}
		if err := appendFile(f, segPath(tr.dir, seq)); err != nil {
			return "", "", err
		}
	}
	return out, ext, f.Sync()
}

func appendFile(dst *os.File, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	_, err = io.Copy(dst, in)
	return err
}

func readHead(p string, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	k, err := io.ReadFull(f, b)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return b[:k], nil
}

// remuxLooksEmpty reports an ffmpeg run that "succeeded" but wrote a file
// with no streams (a few hundred bytes of MP4 header from a large input).
func remuxLooksEmpty(in, out string) bool {
	fi, err1 := os.Stat(in)
	fo, err2 := os.Stat(out)
	if err1 != nil || err2 != nil {
		return err2 != nil
	}
	return fi.Size() >= 16<<10 && fo.Size() < 1024
}

// errNotMedia marks a segment answer that is not audio/video: a web page, a
// JSON error or plain text sent with status 200 (usually an expired token).
var errNotMedia = errors.New("the server sent a web page instead of the video (its link has probably expired)")

// notMediaError wraps errNotMedia with the start of what the server said.
func notMediaError(b []byte) error {
	if len(b) == 0 {
		return fmt.Errorf("%w: empty answer", errNotMedia)
	}
	r := []rune(strings.Join(strings.Fields(leadingText(b[:min(len(b), 400)])), " "))
	if len(r) > 80 {
		r = append(r[:80], '…')
	}
	return fmt.Errorf("%w: %q", errNotMedia, string(r))
}

// imageMagic reports bytes that open as a picture.
func imageMagic(b []byte) bool {
	has := func(off int, sig string) bool {
		return len(b) >= off+len(sig) && string(b[off:off+len(sig)]) == sig
	}
	return has(0, "\x89PNG\r\n\x1a\n") || has(0, "\xff\xd8\xff") || has(0, "GIF87a") || has(0, "GIF89a") ||
		(has(0, "RIFF") && has(8, "WEBP")) || (has(0, "BM") && len(b) >= 26 && binary.LittleEndian.Uint32(b[6:10]) == 0)
}

// stripDisguise removes the fake image header some hosts put in front of
// each segment (MPEG-TS "hidden" in a .png/.jpg/.gif that image CDNs serve):
// the real payload starts where TS sync bytes repeat every 188 bytes, or at
// an fMP4 box. A picture with no media inside is an error.
func stripDisguise(b []byte) ([]byte, error) {
	if !imageMagic(b) {
		return b, nil
	}
	limit := min(len(b), 8<<20)
	for k := 1; k < limit && k+376 < len(b); k++ {
		if b[k] == 0x47 && b[k+188] == 0x47 && b[k+376] == 0x47 && (k+564 >= len(b) || b[k+564] == 0x47) {
			return b[k:], nil
		}
	}
	for _, box := range []string{"ftyp", "styp", "moof"} {
		for off := 4; off < limit; {
			i := bytes.Index(b[off:limit], []byte(box))
			if i < 0 {
				break
			}
			i += off
			if size := int(binary.BigEndian.Uint32(b[i-4 : i])); size >= 8 && size <= len(b)-(i-4) {
				return b[i-4:], nil
			}
			off = i + 4
		}
	}
	return nil, fmt.Errorf("%w: a picture with no video inside", errNotMedia)
}

// mediaPayload names the audio/video format a segment's first bytes hold:
// "ts", "mp4" (fMP4 boxes), "aac" (ADTS), "mp3", "ac3", or "" for anything
// else.
func mediaPayload(b []byte) string {
	// Packed audio segments (AAC / MP3 / AC-3) open with an ID3 timestamp tag;
	// the format is what follows it.
	if len(b) >= 10 && string(b[:3]) == "ID3" {
		skip := 10 + (int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f))
		if b[5]&0x10 != 0 {
			skip += 10 // footer
		}
		if skip < len(b) {
			if inner := mediaPayload(b[skip:]); inner != "" {
				return inner
			}
		}
		return "mp3"
	}
	if len(b) >= 188 && b[0] == 0x47 && (len(b) == 188 || b[188] == 0x47) {
		return "ts"
	}
	if len(b) >= 8 {
		switch string(b[4:8]) {
		case "ftyp", "styp", "moof", "sidx", "moov", "emsg", "prft", "free", "mdat", "skip", "uuid":
			return "mp4"
		}
	}
	if len(b) >= 2 && b[0] == 0x0b && b[1] == 0x77 {
		return "ac3"
	}
	switch e := sniffExt(b); e {
	case "aac", "mp3", "ts", "mp4":
		return e
	case "m4a", "mov":
		return "mp4"
	}
	return ""
}
