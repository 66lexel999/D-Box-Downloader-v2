package engine

// yt-dlp downloads through the external binary: the task-unique output name,
// the retries that get past common failures, and a look at what came out.
//
// yt-dlp never overwrites: with a fixed "<title>.%(ext)s" template a second
// download of the same video (another quality, or two playlist entries whose
// titles match) "completed" instantly with the old file, and every entry of a
// page with several media landed on the same name. yt-dlp therefore writes to
// "<title>.<task id>.%(ext)s" and the result is renamed to a free clean name
// at the end, as the burst path does.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"myidm/internal/ytdlp"
)

// errMultiEntry stops a run that found several media on a page while the task
// named none: yt-dlp would save them all, the first (often a sound effect, an
// intro or an ad) under the task's name.
var errMultiEntry = errors.New("the page holds several media")

const onlyClipMsg = "Only an audio/ad clip was found on this page — use the Link Grabber to pick another item"

var audioFileExts = map[string]bool{
	"mp3": true, "m4a": true, "aac": true, "opus": true, "ogg": true, "oga": true, "flac": true,
	"wav": true, "wma": true, "weba": true, "mka": true,
}

// ytdlpStem is the clean name (no extension) a yt-dlp task's file gets.
func ytdlpStem(t *Task) string { return SanitizeFileName(t.Title) }

// ytdlpWorkStem is the task-unique name yt-dlp writes under.
func ytdlpWorkStem(t *Task) string { return ytdlpStem(t) + "." + t.ID }

// ytdlpTemplate is the -o template. "%" starts a yt-dlp field, so a title's
// own "%" is escaped.
func ytdlpTemplate(t *Task) string {
	return filepath.Join(t.Dir, strings.ReplaceAll(ytdlpWorkStem(t), "%", "%%")+".%(ext)s")
}

// ytdlpDisplayName maps a path yt-dlp reports ("<stem>.<id>.webm") to the name
// the row shows ("<stem>.webm"); false for a per-format part ("….f137.mp4"),
// which the merge replaces.
func ytdlpDisplayName(t *Task, reported string) (string, bool) {
	rest, ok := strings.CutPrefix(filepath.Base(reported), ytdlpWorkStem(t)+".")
	if !ok || rest == "" || strings.Contains(rest, ".") {
		return "", false
	}
	return ytdlpStem(t) + "." + rest, true
}

// placeYtdlpOutput moves a finished "<stem>.<id>.<ext>" to a free
// "<stem>.<ext>" (or "<stem> (1).<ext>"). A file not named that way is kept.
func placeYtdlpOutput(t *Task, produced string) string {
	rest, ok := strings.CutPrefix(filepath.Base(produced), ytdlpWorkStem(t))
	if !ok || !strings.HasPrefix(rest, ".") {
		return produced
	}
	final := uniquePath(filepath.Join(filepath.Dir(produced), ytdlpStem(t)+rest))
	if os.Rename(produced, final) != nil {
		return produced
	}
	return final
}

// removeWorkFiles deletes everything a run of this task left under its work
// name (parts, fragments, aria2c control files).
func removeWorkFiles(t *Task) {
	prefix := ytdlpWorkStem(t) + "."
	ents, err := os.ReadDir(t.Dir)
	if err != nil {
		return
	}
	for _, en := range ents {
		if !en.IsDir() && strings.HasPrefix(en.Name(), prefix) {
			os.Remove(filepath.Join(t.Dir, en.Name()))
		}
	}
}

// multiEntryRE matches yt-dlp announcing a playlist ("[generic] Playlist X:
// Downloading 3 items of 3", "[download] Downloading item 1 of 3").
var multiEntryRE = regexp.MustCompile(`Downloading (?:item \d+ of|\d+ items of) (\d+)`)

func multiEntryLine(line string) bool {
	m := multiEntryRE.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	n, _ := strconv.Atoi(m[1])
	return n > 1
}

// runYtdlpNative lets yt-dlp (with aria2c) perform the download — the fallback
// when the fast path can't run (no ffmpeg, audio-only, a non-YouTube site, or a
// resolve/mux failure). Progress is parsed from yt-dlp/aria2c output into the
// single synthetic segment. Pause/delete cancel the context; a re-run resumes
// via yt-dlp --continue (the work name is stable per task).
func (e *Engine) runYtdlpNative(ctx context.Context, t *Task) {
	if !ytdlp.Available() {
		e.finishTask(t, ytdlp.ErrNotInstalled)
		return
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		e.finishTask(t, err)
		return
	}

	e.mu.Lock()
	_, wantExt := splitExt(t.FileName)
	wantVideo := !t.Audio && !audioFileExts[wantExt]
	e.mu.Unlock()

	opts := e.ytdlpOpts(t)
	started := time.Now()
	var reported string
	altTried, reprobed := false, false
	for {
		fp, msg, err := e.ytdlpAttempt(ctx, t, opts)
		if ctx.Err() != nil { // paused / deleted / shutdown
			e.finishInterrupted(t)
			return
		}
		if err == nil {
			reported = fp
			break
		}
		switch {
		case errors.Is(err, errMultiEntry) && !reprobed:
			reprobed = true
			removeWorkFiles(t)
			opts.PlaylistItem = e.pickPlaylistItem(ctx, t)
			if ctx.Err() != nil {
				e.finishInterrupted(t)
				return
			}
			continue
		case ytdlp.AccountRetry(t.URL, opts, msg):
			// Age-restricted / members-only YouTube video: retry once with the
			// browser's login cookies (runs without them by default).
			opts.UseAccount = true
			e.log.Info("video needs a signed-in account; retrying with the browser's cookies", "id", t.ID)
			e.setNote(t, "Retrying signed in…")
			continue
		case !opts.Impersonate && ytdlp.NeedsImpersonation(msg):
			// A site's bot check (Cloudflare) turned yt-dlp's plain requests away;
			// retry once looking like a real browser.
			opts.Impersonate = true
			e.log.Info("site blocked yt-dlp; retrying with browser impersonation", "id", t.ID)
			e.setNote(t, "Retrying as a browser…")
			continue
		case !altTried && ytdlp.AltURL(t.URL, msg) != "":
			altTried = true
			e.mu.Lock()
			t.URL = ytdlp.AltURL(t.URL, msg)
			e.saveLocked()
			e.mu.Unlock()
			e.log.Info("retrying yt-dlp with the site's alternative address", "id", t.ID, "url", t.URL)
			continue
		case fp != "" && ytdlp.UsesAria2c(opts):
			// The transfer itself failed under aria2c: hosts that cap connections
			// per IP or refuse parallel ranges answer its 16 connections with
			// 403/429/503. aria2c's out-of-order pieces can't be resumed by another
			// downloader, so start clean.
			opts.NativeDownloader = true
			removeWorkFiles(t)
			e.log.Info("aria2c download failed; retrying with yt-dlp's own downloader", "id", t.ID, "err", msg)
			e.setNote(t, "Retrying…")
			continue
		}
		e.finishTask(t, fmt.Errorf("yt-dlp: %s", ytdlp.FriendlyError(msg)))
		return
	}

	picked := e.pickYtdlpOutput(ctx, t, reported, started)
	if !fileExistsOnDisk(picked) {
		e.finishTask(t, fmt.Errorf("yt-dlp finished but produced no file"))
		return
	}
	finalPath := placeYtdlpOutput(t, picked)
	if wantVideo {
		// A "video" that came out without a picture is the page's sound effect or
		// ad, not what the user wanted; say so instead of reporting success.
		if hasVideo, _, _, ok := ytdlp.MediaStreams(ctx, finalPath); ok && !hasVideo {
			e.finishTask(t, fmt.Errorf("%s (the clip was kept as %s)", onlyClipMsg, filepath.Base(finalPath)))
			return
		}
	}
	e.completeTask(t, finalPath)
	e.log.Info("video task completed", "id", t.ID, "file", finalPath)
	e.notifyCompleted(t.ID)
}

// pickPlaylistItem asks yt-dlp which entry of a multi-media page is the main
// one, for a task added without that choice. Falls back to the first entry.
func (e *Engine) pickPlaylistItem(ctx context.Context, t *Task) int {
	e.setNote(t, "Finding the main video…")
	pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	res, err := ytdlp.Probe(pctx, t.URL, e.ytdlpOpts(t))
	cancel()
	e.setNote(t, "")
	item := 1
	if err == nil && res.Recommended > 0 {
		item = res.Recommended
	}
	e.mu.Lock()
	t.PlaylistItem = item
	if err == nil && len(res.Options) > 0 && res.Options[0].Size > 0 && (t.Size <= 0 || t.SizeEstimated) {
		t.Size, t.SizeEstimated = res.Options[0].Size, true
		t.Segments[0].End = t.Size - 1
	}
	e.saveLocked()
	e.mu.Unlock()
	e.log.Info("page holds several media; downloading the main one", "id", t.ID, "item", item, "probeErr", err)
	return item
}

// ytdlpAttempt runs yt-dlp once, streaming its progress into the task. It
// returns the destination yt-dlp reported, and on failure the most useful
// error line.
func (e *Engine) ytdlpAttempt(ctx context.Context, t *Task, opts ytdlp.Opts) (string, string, error) {
	actx, acancel := context.WithCancel(ctx) // lets a multi-entry page stop this run alone
	defer acancel()
	cmd := ytdlp.DownloadCmd(actx, t.URL, t.Selector, ytdlpTemplate(t), opts, t.Audio, e.cfg.Segments)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err.Error(), err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err.Error(), err
	}
	if err := cmd.Start(); err != nil {
		return "", err.Error(), err
	}

	// The probe's estimate stays the row's total while a video+audio download
	// is still on its first stream (which reports only its own size).
	e.mu.Lock()
	var hint int64
	if t.SizeEstimated && t.Size > 0 {
		hint = t.Size
	}
	e.mu.Unlock()
	wantStreams := 1
	if first, _, _ := strings.Cut(t.Selector, "/"); strings.Contains(first, "+") && !t.Audio {
		wantStreams = 2
	}

	var (
		finalPath string
		errTail   string
		multi     bool
		smu       sync.Mutex
		// A video+audio format is fetched as separate streams, each reporting
		// its own 0-100%. base carries the finished streams' bytes forward so
		// the row's progress and size keep growing instead of jumping back.
		base, curTotal int64
		streams        int
	)
	applyProgress := func(p ytdlp.Progress) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if p.SpeedBPS > 0 {
			t.speed = p.SpeedBPS
		}
		if p.Percent < 0 { // total unknown (live / chunked): show bytes received
			if p.Downloaded > 0 {
				t.Segments[0].SetDone(base + p.Downloaded)
			}
			return
		}
		if p.Total > 0 {
			curTotal = p.Total
			size := base + p.Total
			pending := streams < wantStreams
			if pending && hint > size {
				size = hint
			}
			t.Size = size
			t.SizeEstimated = p.Estimated || pending
			t.Segments[0].End = t.Size - 1
		}
		if curTotal > 0 {
			done := int64(p.Percent / 100 * float64(curTotal))
			if done > curTotal {
				done = curTotal
			}
			t.Segments[0].SetDone(base + done)
		}
	}
	// newStream is called at each "[download] Destination:" line.
	newStream := func() {
		e.mu.Lock()
		if streams > 0 {
			base += curTotal
			curTotal = 0
		}
		streams++
		e.mu.Unlock()
	}
	// yt-dlp's own progress prints on stdout, but an external downloader (aria2c)
	// and yt-dlp's errors print on stderr — so scan BOTH live. aria2c rewrites
	// its progress line in place with \r (not \n), hence the \r-aware splitter;
	// without this the row sits on "Receiving…" until the very end.
	scan := func(r io.Reader, isErr bool) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		sc.Split(splitLinesCR)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if p, ok := ytdlp.ParseProgress(line); ok {
				applyProgress(p)
			} else if p, ok := ytdlp.ParseAria2cProgress(line); ok {
				applyProgress(p)
			} else if p, ok := ytdlp.ParseFFmpegProgress(line); ok {
				applyProgress(p)
			} else if d, ok := ytdlp.ParseDestination(line); ok {
				smu.Lock()
				finalPath = d
				smu.Unlock()
				if name, ok := ytdlpDisplayName(t, d); ok {
					e.mu.Lock()
					t.FileName = name // the row shows the real extension while it runs
					e.mu.Unlock()
				}
				switch {
				case strings.HasPrefix(line, "[Merger]"):
					e.setNote(t, "Merging…")
				case strings.HasPrefix(line, "[ExtractAudio]"):
					e.setNote(t, "Converting…")
				default:
					newStream()
				}
			} else if opts.PlaylistItem == 0 && multiEntryLine(line) {
				smu.Lock()
				multi = true
				smu.Unlock()
				acancel()
			} else if isErr {
				smu.Lock()
				errTail = line // keep the last real stderr line for the error message
				smu.Unlock()
			}
		}
	}
	var swg sync.WaitGroup
	swg.Add(2)
	go func() { defer swg.Done(); scan(stdout, false) }()
	go func() { defer swg.Done(); scan(stderr, true) }()
	swg.Wait()
	waitErr := cmd.Wait()
	smu.Lock()
	defer smu.Unlock()
	if multi {
		return finalPath, "", errMultiEntry
	}
	if waitErr != nil {
		msg := errTail
		if msg == "" {
			msg = waitErr.Error()
		}
		return finalPath, msg, waitErr
	}
	return finalPath, "", nil
}

// pickYtdlpOutput decides which file a finished yt-dlp run produced, checked
// against the filesystem rather than trusting yt-dlp's stdout (the reported
// destination can be mangled, or name an intermediate file the merge step has
// since replaced). When a video+audio download was left as two separate
// format files (yt-dlp couldn't merge them), they're merged here — before,
// the row pointed at whichever finished last: the audio.
func (e *Engine) pickYtdlpOutput(ctx context.Context, t *Task, reported string, since time.Time) string {
	ws := ytdlpWorkStem(t)
	if reported != "" && !filepath.IsAbs(reported) {
		reported = filepath.Join(t.Dir, reported)
	}
	if !t.Audio {
		if parts := formatParts(t.Dir, ws, since); len(parts) >= 2 {
			if merged := e.mergeParts(ctx, t, ws, parts); merged != "" {
				return merged
			}
			return keepVideoPart(t.Dir, ws, ytdlpStem(t), parts)
		}
	}
	if fileExistsOnDisk(reported) && !strings.HasSuffix(reported, ".part") {
		return reported
	}
	return producedFile(t.Dir, ws, since)
}

// formatParts lists yt-dlp's per-format files ("<stem>.f137.mp4",
// "<stem>.f140.m4a") written since the run started.
func formatParts(dir, stem string, since time.Time) []string {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(stem) + `\.f[0-9A-Za-z_-]+\.[0-9A-Za-z]+$`)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, en := range ents {
		if en.IsDir() || !re.MatchString(en.Name()) {
			continue
		}
		if fi, err := en.Info(); err == nil && !fi.ModTime().Before(since.Add(-time.Minute)) && fi.Size() > 0 {
			out = append(out, filepath.Join(dir, en.Name()))
		}
	}
	return out
}

// keepVideoPart handles unmergeable parts (no ffmpeg): the largest part is the
// video; it gets the work name (placed under the clean name afterwards), and
// the audio is kept beside it as "<name> (audio).<ext>" rather than thrown away.
func keepVideoPart(dir, workStem, stem string, parts []string) string {
	video := largestFile(parts)
	final := uniquePath(filepath.Join(dir, workStem+filepath.Ext(video)))
	if os.Rename(video, final) != nil {
		final = video
	}
	for _, p := range parts {
		if p != video {
			os.Rename(p, uniquePath(filepath.Join(dir, stem+" (audio)"+filepath.Ext(p))))
		}
	}
	return final
}

func largestFile(paths []string) string {
	best, bestSize := "", int64(-1)
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.Size() > bestSize {
			best, bestSize = p, fi.Size()
		}
	}
	return best
}

// mergeParts joins separate video/audio format files with ffmpeg (stream copy)
// and removes the parts. "" when ffmpeg is missing or the merge fails.
func (e *Engine) mergeParts(ctx context.Context, t *Task, stem string, parts []string) string {
	if !ytdlp.HasFFmpeg() {
		return ""
	}
	ext := "mp4"
	for _, p := range parts {
		switch strings.ToLower(filepath.Ext(p)) {
		case ".mp4", ".m4a", ".m4v", ".mov":
		default:
			ext = "mkv" // webm/opus/other codecs: MKV holds anything
		}
	}
	e.setNote(t, "Merging…")
	out := uniquePath(filepath.Join(t.Dir, stem+"."+ext))
	mctx, cancel := context.WithTimeout(e.appCtx(), 10*time.Minute)
	defer cancel()
	if err := ytdlp.MergeCmd(mctx, parts, out).Run(); err != nil {
		os.Remove(out)
		e.log.Info("merging yt-dlp format files failed", "id", t.ID, "err", err)
		return ""
	}
	for _, p := range parts {
		os.Remove(p)
	}
	return out
}

// findProducedFile finds the newest finished file named exactly
// "<stem>.<ext>" or "<stem> (N).<ext>" in dir (yt-dlp's output when its
// destination line wasn't captured). A longer title that merely starts with
// stem ("Episode 10" for "Episode 1") doesn't match.
func findProducedFile(dir, stem string) string { return producedFile(dir, stem, time.Time{}) }

// producedFile is findProducedFile limited to files written since the run
// started (a minute of slack for coarse timestamps); a zero since accepts any.
func producedFile(dir, stem string, since time.Time) string {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(stem) + `(?: \(\d+\))?\.[0-9A-Za-z]{1,6}$`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestMod time.Time
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !re.MatchString(name) ||
			strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") || strings.HasSuffix(name, ".aria2") {
			continue
		}
		fi, err := ent.Info()
		if err != nil || (!since.IsZero() && fi.ModTime().Before(since.Add(-time.Minute))) {
			continue
		}
		if best == "" || fi.ModTime().After(bestMod) {
			best, bestMod = name, fi.ModTime()
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(dir, best)
}
