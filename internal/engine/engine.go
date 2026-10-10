// Package engine implements the download manager core: task lifecycle,
// scheduling, segmented HTTP transfer, pause/resume, and persistence.
//
// Architecture mirrors github.com/maxuanquang/idm's layering (handler ->
// logic -> dataaccess) collapsed for a single-process app: the Kafka queue
// becomes an in-process FIFO + scheduler, MySQL becomes a JSON store, and
// MinIO becomes direct-to-disk files.
package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"myidm/internal/config"
	"myidm/internal/store"
	"myidm/internal/ytdlp"
)

var (
	ErrNotFound = errors.New("task not found")
	ErrConflict = errors.New("operation not valid in current task state")
)

type Engine struct {
	cfg     *config.Config
	store   *store.Store
	log     *slog.Logger
	client  *http.Client
	limiter *limiter

	completionNotifier func(id string) // opens the native "done" window (GUI mode); nil = headless

	mu                      sync.Mutex
	tasks                   map[string]*Task
	order                   []string // insertion order
	queue                   []string // FIFO of queued task IDs
	running                 int
	completionSuppressUntil time.Time // "don't show for 2h" — guarded by mu

	// "Turn off the PC when downloads finish" (armed from the UI, per session).
	// completionsSinceArm counts completions since it was armed, so arming while
	// idle doesn't fire until an actual download finishes; guarded by mu.
	shutdownArmed       bool
	completionsSinceArm int

	kick    chan struct{}
	rootCtx context.Context
	wg      sync.WaitGroup

	settingsMu       sync.Mutex        // guards cfg.DownloadDir / cfg.Categories / overrides / startWithWindows / hideDone / auto
	overrides        map[string]string // category -> remembered custom folder (persisted in settings.json)
	defaultDir       string            // app-default base download folder, for Reset
	startWithWindows *bool             // "start D BOX when Windows starts"; nil = never chosen (persisted)
	dirChosen        bool              // the base folder came from settings.json or Options (not -dir / default)
	concChosen       bool              // the simultaneous limit came from settings.json or Options
	hideDone         bool              // "Don't show again" on the completion popup (persisted; Options turns it back on)
	auto             autoDownload      // auto download: skip the New Download window (persisted; autodl.go)
}

// SetCompletionNotifier wires the native download-complete popup. Set in GUI
// mode; when nil, the browser UI shows its own in-app modal instead.
func (e *Engine) SetCompletionNotifier(f func(id string)) {
	e.mu.Lock()
	e.completionNotifier = f
	e.mu.Unlock()
}

// SuppressCompletionPopup hides the completion popup for d (the "don't show for
// 2 hours" checkbox).
func (e *Engine) SuppressCompletionPopup(d time.Duration) {
	e.mu.Lock()
	e.completionSuppressUntil = time.Now().Add(d)
	e.mu.Unlock()
}

// DonePopupEnabled reports whether a finished download pops the completion
// window (off after its "Don't show again"; Options turns it back on).
func (e *Engine) DonePopupEnabled() bool {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	return !e.hideDone
}

// SetDonePopupEnabled turns the completion popup on or off for good (persisted).
// Turning it on also ends a "don't show for 2 hours" pause.
func (e *Engine) SetDonePopupEnabled(on bool) {
	e.settingsMu.Lock()
	if hide := !on; e.hideDone != hide {
		e.hideDone = hide
		e.saveSettingsLocked()
	}
	e.settingsMu.Unlock()
	if on {
		e.mu.Lock()
		e.completionSuppressUntil = time.Time{}
		e.mu.Unlock()
	}
}

// GUIEnabled reports whether MyIDM is running as a native desktop app (vs a
// headless server). The web UI uses this to avoid double completion popups.
func (e *Engine) GUIEnabled() bool { return e.cfg.GUI }

// notifyCompleted opens the native completion popup unless suppressed. The
// notifier is read under the lock (it can be set concurrently at startup) but
// invoked outside it (it spawns a process).
func (e *Engine) notifyCompleted(id string) {
	e.mu.Lock()
	notifier := e.completionNotifier
	suppressed := time.Now().Before(e.completionSuppressUntil)
	e.mu.Unlock()
	if notifier != nil && !suppressed && e.DonePopupEnabled() {
		notifier(id)
	}
}

func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Engine {
	ytdlp.SetSearchDirs(cfg.DownloadDir)                       // also finds tools in <dir>/Programs/...
	ytdlp.SetCookiesDir(filepath.Join(cfg.DataDir, "cookies")) // browser-supplied login cookies (IG etc.)
	ytdlp.SetToolsDir(filepath.Join(cfg.DataDir, "tools"))     // on-demand ffmpeg lands here
	e := &Engine{
		cfg:        cfg,
		store:      st,
		log:        log,
		limiter:    newLimiter(cfg.SpeedLimit),
		tasks:      make(map[string]*Task),
		kick:       make(chan struct{}, 1),
		overrides:  map[string]string{},
		defaultDir: config.DefaultDownloadDir(),
		client: &http.Client{
			// Cookie-check redirects ("set this cookie and come back", rumble's
			// 307 loop) only resolve when cookies persist across hops, and a
			// landing page's session must reach the file it links to.
			Jar: newCookieJar(),
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				// Force HTTP/1.1. A download manager needs one TCP connection PER
				// segment so each gets its own congestion/receive window and they
				// run in parallel. With HTTP/2 the Go client multiplexes every
				// segment over a SINGLE socket, capping aggregate throughput at one
				// connection's rate (the ~13 MB/s symptom). An empty TLSNextProto
				// disables h2 negotiation on TLS.
				ForceAttemptHTTP2: false,
				TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
				// The default socket read buffer is only 4 KB — far too small to
				// keep a fast link saturated. Large buffers cut syscalls per byte.
				ReadBufferSize:        256 << 10,
				WriteBufferSize:       128 << 10,
				MaxIdleConns:          256,
				MaxIdleConnsPerHost:   64,
				MaxConnsPerHost:       0, // unlimited
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				DisableCompression:    true,
			},
		},
	}
	e.loadSettings()
	return e
}

// Start loads persisted tasks, re-queues anything interrupted last session,
// and launches the scheduler and progress sampler.
func (e *Engine) Start(ctx context.Context) error {
	e.rootCtx = ctx

	var persisted []*Task
	if err := e.store.Load(&persisted); err != nil {
		return fmt.Errorf("load tasks: %w", err)
	}

	e.mu.Lock()
	for _, t := range persisted {
		e.tasks[t.ID] = t
		e.order = append(e.order, t.ID)
		switch t.Status {
		case StatusDownloading, StatusQueued:
			// Interrupted last session — continue automatically.
			t.Status = StatusQueued
			e.queue = append(e.queue, t.ID)
		case StatusCompleted:
			// Repair entries whose FinalPath was recorded from a mangled yt-dlp
			// stdout line (pre-fix): the file is on disk under its true Unicode
			// name, so re-resolve by the output stem instead of showing "missing".
			if t.Kind == "ytdlp" && t.Title != "" && !fileExistsOnDisk(t.FinalPath) {
				if found := findProducedFile(t.Dir, SanitizeFileName(t.Title)); found != "" {
					t.FinalPath = found
					t.FileName = filepath.Base(found)
				}
			}
		}
	}
	n := len(persisted)
	if n > 0 {
		e.saveLocked() // flush any FinalPath repairs done above
	}
	e.mu.Unlock()
	if n > 0 {
		e.log.Info("loaded persisted tasks", "count", n)
	}

	go e.schedulerLoop(ctx)
	go e.samplerLoop(ctx)
	go e.provisionTools()
	e.poke()
	return nil
}

// provisionTools, on first run, keeps yt-dlp current (the biggest YouTube-speed
// factor — a stale yt-dlp gets a throttled URL) and makes aria2c available for
// multi-connection downloads. Both are background, best-effort, no-op without yt-dlp.
func (e *Engine) provisionTools() {
	if !ytdlp.Available() {
		return
	}
	e.updateYtdlp()
	e.provisionAria2c()
}

// updateYtdlp runs `yt-dlp -U` at most once per 12h (marker in DataDir). A
// current yt-dlp solves YouTube's signature throttle, the difference between a
// few MB/s and full link speed. May fail if a yt-dlp download holds the exe
// (Windows lock) — it just retries next launch.
func (e *Engine) updateYtdlp() {
	marker := filepath.Join(e.cfg.DataDir, ".ytdlp-updated")
	if fi, err := os.Stat(marker); err == nil && time.Since(fi.ModTime()) < 12*time.Hour {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := ytdlp.SelfUpdate(ctx)
	if err != nil {
		e.log.Warn("yt-dlp update failed — YouTube may stay throttled; replace yt-dlp.exe with the latest release manually", "err", err, "out", out)
		return
	}
	_ = os.WriteFile(marker, []byte(out), 0o644)
	e.log.Info("yt-dlp update check done", "result", out)
}

// provisionAria2c fetches aria2c on first run (once; skipped if already present)
// so yt-dlp downloads videos over many connections instead of one — YouTube
// throttles each connection, so a single stream crawls at a few hundred KB/s.
func (e *Engine) provisionAria2c() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := filepath.Join(e.downloadDir(), "Programs")
	switch p, err := ytdlp.EnsureAria2c(ctx, dir); {
	case err != nil:
		e.log.Warn("aria2c provisioning failed (video downloads stay single-connection)", "err", err)
	case p != "":
		e.log.Info("aria2c ready — video downloads will use multiple connections", "path", p)
	}
}

func (e *Engine) poke() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

func (e *Engine) schedulerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.kick:
			e.fillSlots()
		}
	}
}

func (e *Engine) fillSlots() {
	e.mu.Lock()
	for e.running < e.maxConcurrent() && len(e.queue) > 0 {
		id := e.queue[0]
		e.queue = e.queue[1:]
		t, ok := e.tasks[id]
		if !ok || t.Status != StatusQueued {
			continue
		}
		taskCtx, cancel := context.WithCancel(e.rootCtx)
		t.Status = StatusDownloading
		t.Error = ""
		t.intent = intentNone
		t.cancel = cancel
		t.prevBytes = t.downloaded()
		t.speed, t.speedMax = 0, 0
		t.recent = t.recent[:0]
		e.running++
		e.wg.Add(1)
		go e.runTask(taskCtx, t)
	}
	// After the queue drains and everything settles, honor "turn off when done":
	// fire once the whole batch (Start All / a playlist) is finished, not after a
	// single item. Runs on every scheduler kick, i.e. after each completion.
	fire := e.shouldAutoShutdownLocked()
	if fire {
		e.shutdownArmed = false
	}
	e.mu.Unlock()
	if fire {
		e.startAutoShutdown()
	}
}

// shouldAutoShutdownLocked reports whether an auto-shutdown should fire now:
// it's armed, at least one download has completed since it was armed, and
// nothing is still in flight or waiting. Caller holds e.mu.
func (e *Engine) shouldAutoShutdownLocked() bool {
	return e.shutdownArmed && e.completionsSinceArm > 0 && !e.downloadsPendingLocked()
}

// downloadsPendingLocked reports whether any download is still in flight or
// waiting — running, queued, scheduled for later, or an interrupted (non-Later)
// paused task the user may still resume. Held "Download Later" items and failed
// tasks don't count: they won't finish on their own, so they never block an
// auto-shutdown. Caller holds e.mu.
func (e *Engine) downloadsPendingLocked() bool {
	for _, t := range e.tasks {
		switch t.Status {
		case StatusDownloading, StatusQueued, StatusScheduled:
			return true
		case StatusPaused:
			if !t.Later {
				return true
			}
		}
	}
	return false
}

// SetShutdownWhenDone arms/disarms the "turn off the PC when all downloads
// finish" option (per session). Arming resets the completion counter so it only
// fires after a download completes from now on; disarming aborts any countdown
// the OS may have already started.
func (e *Engine) SetShutdownWhenDone(on bool) {
	e.mu.Lock()
	e.shutdownArmed = on
	if on {
		e.completionsSinceArm = 0
	}
	e.mu.Unlock()
	if !on {
		cancelSystemShutdown() // best-effort: abort a shutdown already scheduled
	}
	e.log.Info("auto shutdown when done", "armed", on)
}

// ShutdownWhenDone reports whether auto-shutdown is armed.
func (e *Engine) ShutdownWhenDone() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownArmed
}

// startAutoShutdown issues the OS shutdown (with a grace period the user can
// abort). Called outside e.mu since it spawns a process.
func (e *Engine) startAutoShutdown() {
	e.log.Info("all downloads finished — auto shutdown triggered")
	if err := systemShutdown(); err != nil {
		e.log.Error("auto shutdown failed", "err", err)
	}
}

// speedWindow is how many recent EMA samples feed the avg/min stats. At the
// 500ms sample tick that's a ~15s trailing window — long enough to be a stable
// average, short enough that the initial connection ramp-up ages out quickly
// and avg/min settle onto the real steady-state rate.
const speedWindow = 30

// samplerLoop refreshes speed EMAs twice a second and persists in-flight
// segment progress every 2s so a hard kill loses at most ~2s of bookkeeping.
func (e *Engine) samplerLoop(ctx context.Context) {
	const dt = 500 * time.Millisecond
	ticker := time.NewTicker(dt)
	defer ticker.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick++
			e.mu.Lock()
			// Promote any scheduled download whose start time has arrived (also
			// catches schedules that came due while the app was closed).
			due := e.promoteDueLocked(time.Now())
			anyRunning := false
			for _, t := range e.tasks {
				if t.Status != StatusDownloading {
					t.speed = 0
					continue
				}
				anyRunning = true
				cur := t.downloaded()
				inst := float64(cur-t.prevBytes) / dt.Seconds()
				t.prevBytes = cur
				if t.speed == 0 {
					t.speed = inst
				} else {
					t.speed = 0.65*t.speed + 0.35*inst
				}
				// Sample the smoothed rate into a rolling window, counting ONLY
				// ticks where data is actually flowing — this skips the
				// resolve/merge gaps (yt-dlp + ffmpeg) and the brief progress
				// reset between a video stream and its separate audio stream.
				// max is the all-time peak; avg/min are derived from the window
				// (see viewLocked) so they track the current steady-state speed
				// instead of being pinned to the slow multi-connection ramp-up.
				if t.speed > 1<<10 {
					if t.speed > t.speedMax {
						t.speedMax = t.speed
					}
					t.recent = append(t.recent, t.speed)
					if len(t.recent) > speedWindow {
						copy(t.recent, t.recent[1:])
						t.recent = t.recent[:speedWindow]
					}
				}
			}
			if due || (anyRunning && tick%4 == 0) {
				e.saveLocked()
			}
			e.mu.Unlock()
			if due {
				e.poke() // a newly-queued task needs a scheduler slot fill
			}
		}
	}
}

// AddOptions carries the optional parts of a new plain (HTTP) download.
type AddOptions struct {
	FileName    string    // user-chosen name; "" = resolve from the server
	Segments    int       // connections; out of range = default
	Dir         string    // destination folder; "" = default download dir
	ScheduledAt time.Time // future time = hold until then
	Later       bool      // hold in the Download Later queue
	Referer     string    // page the link came from (sent as Referer)
	Headers     map[string]string
	Title       string // naming hint (page / video title) when the server's name is generic
	Description string
}

// Add registers a new download and queues it. dir is the destination folder
// (e.g. a category folder); empty means the default download directory. A
// non-zero, future scheduledAt holds the task in StatusScheduled until that
// time; later=true holds it in the manual "Download Later" queue (paused, the
// user starts it). scheduledAt wins over later when both are set.
func (e *Engine) Add(rawURL, fileName string, segments int, dir string, scheduledAt time.Time, later bool) (TaskView, error) {
	return e.AddWithOptions(rawURL, AddOptions{FileName: fileName, Segments: segments, Dir: dir, ScheduledAt: scheduledAt, Later: later})
}

// AddWithOptions is Add with the full set of options (browser context, naming
// hint, description). The URL may be a file, an HLS/DASH stream or a web page —
// the probe decides how it is downloaded.
func (e *Engine) AddWithOptions(rawURL string, o AddOptions) (TaskView, error) {
	u, err := parseDownloadURL(rawURL)
	if err != nil {
		return TaskView{}, err
	}
	segments := o.Segments
	if segments < 1 || segments > 32 {
		segments = e.cfg.Segments
	}
	fileName := ""
	if strings.TrimSpace(o.FileName) != "" {
		fileName = SanitizeFileName(o.FileName)
	}
	dir := o.Dir
	if strings.TrimSpace(dir) == "" {
		dir = e.downloadDir()
	}

	t := &Task{
		ID:           newID(),
		URL:          u.String(),
		FileName:     fileName,
		Dir:          dir,
		Size:         -1,
		Status:       StatusQueued,
		WantSegments: segments,
		CreatedAt:    time.Now(),
		Referer:      cleanReferer(o.Referer),
		Headers:      CleanHeaders(o.Headers),
		Title:        strings.TrimSpace(o.Title),
		Description:  strings.TrimSpace(o.Description),
	}
	scheduled := setSchedule(t, o.ScheduledAt)
	if !scheduled && o.Later {
		t.Status = StatusPaused
		t.Later = true
	}
	queueNow := !scheduled && !t.Later

	e.mu.Lock()
	e.tasks[t.ID] = t
	e.order = append(e.order, t.ID)
	if queueNow {
		e.queue = append(e.queue, t.ID)
	}
	e.saveLocked()
	view := e.viewLocked(t)
	e.mu.Unlock()

	if queueNow {
		e.poke()
	}
	e.log.Info("task added", "id", t.ID, "url", t.URL, "referer", t.Referer, "scheduled", scheduled, "later", t.Later)
	return view, nil
}

// parseDownloadURL accepts an absolute http(s) URL. A bare "example.com/file"
// (no scheme, as people often paste) is read as https.
func parseDownloadURL(rawURL string) (*url.URL, error) {
	raw := strings.TrimSpace(rawURL)
	if raw != "" && !strings.Contains(raw, "://") && strings.Contains(raw, ".") && !strings.ContainsAny(raw, " \t") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid URL: must be absolute http(s)")
	}
	u.Fragment = "" // never sent to the server; keeps names/dedup clean
	// Share links with a documented direct-download form (Dropbox dl=0,
	// Drive's viewer) go straight to the file.
	return normalizeLink(u), nil
}

// cleanReferer keeps a Referer only if it is an absolute http(s) URL.
func cleanReferer(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

// setSchedule marks t as scheduled when at is in the future, returning whether
// it did. A zero or past time leaves the task as-is (queued now).
func setSchedule(t *Task, at time.Time) bool {
	if at.IsZero() || !at.After(time.Now()) {
		return false
	}
	when := at
	t.Status = StatusScheduled
	t.ScheduledAt = &when
	return true
}

// UserAgent exposes the configured UA (for yt-dlp probes/downloads).
func (e *Engine) UserAgent() string { return e.userAgent() }

// VideoOptions carries the optional parts of a yt-dlp (streaming-site) download.
type VideoOptions struct {
	Title       string
	Selector    string // yt-dlp -f expression; "" = best video+audio
	Ext         string // expected container; "" = mp4
	Dir         string
	Audio       bool // extract audio to mp3
	ScheduledAt time.Time
	Later       bool
	Referer     string
	Headers     map[string]string
	SizeHint    int64 // estimated total bytes from the probe (0 = unknown)
	// PlaylistItem picks one entry (1-based, ytdlp.ProbeResult.Recommended or
	// an entry's Index) of a page that holds several media; 0 = the page's only
	// video (a multi-media page is then resolved when the download starts).
	PlaylistItem int
}

// AddVideo registers a streaming-site download handled by yt-dlp. selector is a
// yt-dlp -f expression; ext is the expected container; audio extracts to mp3. A
// non-zero, future scheduledAt holds it in StatusScheduled until that time;
// later=true holds it in the manual "Download Later" queue.
func (e *Engine) AddVideo(rawURL, title, selector, ext, dir string, audio bool, scheduledAt time.Time, later bool) (TaskView, error) {
	return e.AddVideoWithOptions(rawURL, VideoOptions{Title: title, Selector: selector, Ext: ext, Dir: dir,
		Audio: audio, ScheduledAt: scheduledAt, Later: later})
}

// AddVideoWithOptions is AddVideo with the full option set.
func (e *Engine) AddVideoWithOptions(rawURL string, o VideoOptions) (TaskView, error) {
	u, err := parseDownloadURL(rawURL)
	if err != nil {
		return TaskView{}, err
	}
	if !ytdlp.Available() {
		return TaskView{}, ytdlp.ErrNotInstalled
	}
	referer := cleanReferer(o.Referer)
	title := cleanTitle(o.Title)
	if title == "" {
		title = streamBaseName("", u.String(), referer)
	}
	ext := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(o.Ext), "."))
	if ext == "" {
		ext = "mp4"
		if o.Audio {
			ext = "mp3"
		}
	}
	selector := o.Selector
	if selector == "" {
		selector = "bv*+ba/b"
		if o.Audio {
			selector = "ba/b"
		}
	}
	dir := o.Dir
	if strings.TrimSpace(dir) == "" {
		dir = e.downloadDir()
	}

	t := &Task{
		ID:        newID(),
		URL:       u.String(),
		FileName:  SanitizeFileName(title) + "." + ext,
		Dir:       dir,
		Size:      -1,
		Status:    StatusQueued,
		Probed:    true, // no HTTP probe for yt-dlp tasks
		CreatedAt: time.Now(),
		Kind:      "ytdlp",
		Selector:  selector,
		Title:     title,
		Audio:     o.Audio,
		Referer:   referer,
		Headers:   CleanHeaders(o.Headers),
		Segments:  []*Segment{{Start: 0, End: -1}}, // one synthetic segment for the UI
	}
	if o.PlaylistItem > 0 {
		t.PlaylistItem = o.PlaylistItem
	}
	if o.SizeHint > 0 {
		t.Size, t.SizeEstimated = o.SizeHint, true
		t.Segments[0].End = o.SizeHint - 1
	}
	scheduled := setSchedule(t, o.ScheduledAt)
	if !scheduled && o.Later {
		t.Status = StatusPaused
		t.Later = true
	}
	queueNow := !scheduled && !t.Later

	e.mu.Lock()
	e.tasks[t.ID] = t
	e.order = append(e.order, t.ID)
	if queueNow {
		e.queue = append(e.queue, t.ID)
	}
	e.saveLocked()
	view := e.viewLocked(t)
	e.mu.Unlock()

	if queueNow {
		e.poke()
	}
	e.log.Info("video task added", "id", t.ID, "url", t.URL, "fmt", selector, "scheduled", scheduled, "later", t.Later)
	return view, nil
}

// SetSkip marks (or clears) a Download Later task to be skipped by "Start All".
func (e *Engine) SetSkip(id string, skip bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return ErrNotFound
	}
	t.Skip = skip
	e.saveLocked()
	return nil
}

// SetDescription stores the user's note from the New Download dialog. Purely
// display metadata, so it's a post-Add setter rather than an Add parameter.
func (e *Engine) SetDescription(id, desc string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return ErrNotFound
	}
	t.Description = desc
	e.saveLocked()
	return nil
}

// Schedule moves a queued / paused / failed / already-scheduled task to
// StatusScheduled with a future start time. A downloading or completed task
// can't be scheduled (pause it first). A zero/past time reschedules to "now"
// by routing through the normal queue (same as Resume).
func (e *Engine) Schedule(id string, at time.Time) error {
	if at.IsZero() || !at.After(time.Now()) {
		return e.startNow(id)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return ErrNotFound
	}
	switch t.Status {
	case StatusDownloading, StatusCompleted:
		return fmt.Errorf("%w: cannot schedule a %s task", ErrConflict, t.Status)
	}
	e.queue = slices.DeleteFunc(e.queue, func(q string) bool { return q == id })
	when := at
	t.Status = StatusScheduled
	t.ScheduledAt = &when
	t.Error = ""
	e.saveLocked()
	e.log.Info("task scheduled", "id", id, "at", at)
	return nil
}

// Unschedule cancels a pending schedule, parking the task as paused so the user
// can start or remove it.
func (e *Engine) Unschedule(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return ErrNotFound
	}
	if t.Status != StatusScheduled {
		return fmt.Errorf("%w: task is not scheduled", ErrConflict)
	}
	t.ScheduledAt = nil
	t.Status = StatusPaused
	e.saveLocked()
	return nil
}

// startNow clears any schedule and queues the task immediately. Used by
// "Start now" in the scheduler and by Schedule() when given a past time.
func (e *Engine) startNow(id string) error {
	e.mu.Lock()
	t, ok := e.tasks[id]
	if !ok {
		e.mu.Unlock()
		return ErrNotFound
	}
	switch t.Status {
	case StatusDownloading, StatusQueued:
		e.mu.Unlock()
		return nil // already on its way
	case StatusCompleted:
		e.mu.Unlock()
		return fmt.Errorf("%w: task already completed", ErrConflict)
	}
	t.ScheduledAt = nil
	t.Status = StatusQueued
	t.Error = ""
	t.Skip = false // explicit "start now" un-skips, same as Resume
	e.queue = append(e.queue, id)
	e.saveLocked()
	e.mu.Unlock()
	e.poke()
	return nil
}

// promoteDueLocked queues any scheduled task whose time has arrived. Caller
// holds e.mu; returns whether anything was promoted (so the caller pokes the
// scheduler). Persisted by the caller alongside its own save.
func (e *Engine) promoteDueLocked(now time.Time) bool {
	promoted := false
	for _, t := range e.tasks {
		if t.Status == StatusScheduled && t.ScheduledAt != nil && !now.Before(*t.ScheduledAt) {
			t.Status = StatusQueued
			t.ScheduledAt = nil
			e.queue = append(e.queue, t.ID)
			promoted = true
			e.log.Info("scheduled task is due — queued", "id", t.ID)
		}
	}
	return promoted
}

// CategoryDir resolves an IDM-style category name to its destination folder,
// falling back to the default download directory.
func (e *Engine) CategoryDir(name string) string {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	if d, ok := e.cfg.Categories[name]; ok && strings.TrimSpace(d) != "" {
		return d
	}
	return e.cfg.DownloadDir
}

// Categories returns a copy of the configured category->folder map for the UI.
func (e *Engine) Categories() map[string]string {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	out := make(map[string]string, len(e.cfg.Categories)+1)
	for k, v := range e.cfg.Categories {
		out[k] = v
	}
	if len(out) == 0 {
		out["General"] = e.cfg.DownloadDir
	}
	return out
}

// downloadDir returns the current base download folder (lock-safe).
func (e *Engine) downloadDir() string {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	return e.cfg.DownloadDir
}

// ---- persistent folder settings (settings.json in the data dir) -------------
//
// settings.json overrides what the app ships with: a custom base download folder,
// plus any categories the user pinned to a specific folder via "remember this
// path". It's written only when the user changes something — a fresh install just
// uses the defaults.

type persistedSettings struct {
	DownloadDir      string            `json:"downloadDir,omitempty"`
	Categories       map[string]string `json:"categories,omitempty"`       // category -> remembered custom folder
	MaxConcurrent    int               `json:"maxConcurrent,omitempty"`    // simultaneous downloads (0 = keep default)
	StartWithWindows *bool             `json:"startWithWindows,omitempty"` // nil = never chosen
	HideDonePopup    bool              `json:"hideDonePopup,omitempty"`    // completion popup turned off
	AutoDownload     *autoDownload     `json:"autoDownload,omitempty"`     // nil = off
}

func (e *Engine) settingsPath() string { return filepath.Join(e.cfg.DataDir, "settings.json") }

// loadSettings applies any saved base folder + remembered category folders over
// the config defaults. Called once at startup.
func (e *Engine) loadSettings() {
	b, err := os.ReadFile(e.settingsPath())
	if err != nil {
		return // no saved settings — keep the config defaults
	}
	var s persistedSettings
	if json.Unmarshal(b, &s) != nil {
		return
	}
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	if strings.TrimSpace(s.DownloadDir) != "" {
		e.cfg.DownloadDir = s.DownloadDir
		e.dirChosen = true
	}
	if s.MaxConcurrent > 0 {
		e.cfg.MaxConcurrent = s.MaxConcurrent
		e.concChosen = true
	}
	e.startWithWindows = s.StartWithWindows
	e.hideDone = s.HideDonePopup
	if s.AutoDownload != nil && s.AutoDownload.active(autoNow()) {
		e.auto = *s.AutoDownload
	}
	e.cfg.Categories = config.DefaultCategories(e.cfg.DownloadDir)
	e.overrides = map[string]string{}
	for k, v := range s.Categories {
		if strings.TrimSpace(v) != "" {
			e.overrides[k] = v
			e.cfg.Categories[k] = v
		}
	}
	ytdlp.SetSearchDirs(e.cfg.DownloadDir)
}

// saveSettingsLocked persists the base folder + overrides. Caller holds settingsMu.
// The folder and the simultaneous limit are written only once chosen (saved
// before or set in Options), so saving another setting never pins the -dir /
// -concurrent values this launch happened to use.
func (e *Engine) saveSettingsLocked() {
	s := persistedSettings{Categories: e.overrides, StartWithWindows: e.startWithWindows, HideDonePopup: e.hideDone}
	if e.auto.active(autoNow()) {
		a := e.auto
		s.AutoDownload = &a
	}
	if e.dirChosen {
		s.DownloadDir = e.cfg.DownloadDir
	}
	if e.concChosen {
		s.MaxConcurrent = e.cfg.MaxConcurrent
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := e.settingsPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, e.settingsPath())
	}
}

// Settings reports the base download folder, the effective category->folder map,
// which categories are pinned to a remembered custom folder, the app-default
// base (so the UI can offer a Reset), and the simultaneous-download limit.
func (e *Engine) Settings() (downloadDir string, categories map[string]string, custom map[string]bool, defaultDir string, maxConcurrent int) {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	categories = make(map[string]string, len(e.cfg.Categories))
	for k, v := range e.cfg.Categories {
		categories[k] = v
	}
	custom = make(map[string]bool, len(e.overrides))
	for k := range e.overrides {
		custom[k] = true
	}
	mc := e.cfg.MaxConcurrent
	if mc < 1 {
		mc = 1
	}
	return e.cfg.DownloadDir, categories, custom, e.defaultDir, mc
}

// maxConcurrent is the live simultaneous-download limit (guarded by settingsMu,
// the same lock that protects the configurable folder settings). fillSlots reads
// it while holding e.mu, so the lock order is always e.mu -> settingsMu.
func (e *Engine) maxConcurrent() int {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	if e.cfg.MaxConcurrent < 1 {
		return 1
	}
	return e.cfg.MaxConcurrent
}

// StartWithWindows reports the saved "start D BOX when Windows starts" choice;
// nil means the user never chose (the app's default applies).
func (e *Engine) StartWithWindows() *bool {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	if e.startWithWindows == nil {
		return nil
	}
	on := *e.startWithWindows
	return &on
}

// SetStartWithWindows saves the "start D BOX when Windows starts" choice
// (settings.json is only rewritten when it changes).
func (e *Engine) SetStartWithWindows(on bool) {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	if e.startWithWindows != nil && *e.startWithWindows == on {
		return
	}
	e.startWithWindows = &on
	e.saveSettingsLocked()
}

// SetMaxConcurrent changes how many downloads run at once (1..16), persists it,
// and pokes the scheduler so a raised limit starts queued tasks immediately.
func (e *Engine) SetMaxConcurrent(n int) error {
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	e.settingsMu.Lock()
	e.cfg.MaxConcurrent = n
	e.concChosen = true
	e.saveSettingsLocked()
	e.settingsMu.Unlock()
	e.poke()
	e.log.Info("max concurrent downloads set", "n", n)
	return nil
}

// SetDownloadDir changes the base download folder. Categories still tracking the
// base move under the new base; categories pinned via "remember this path" keep
// their folder. Only affects NEW downloads — existing files aren't moved.
func (e *Engine) SetDownloadDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("%w: empty folder", ErrConflict)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	e.cfg.DownloadDir = dir
	e.dirChosen = true
	e.cfg.Categories = config.DefaultCategories(dir)
	for k, v := range e.overrides {
		e.cfg.Categories[k] = v
	}
	ytdlp.SetSearchDirs(dir)
	e.saveSettingsLocked()
	return nil
}

// SetCategoryDir pins one category to a custom folder ("remember this path"),
// preserved across base-folder changes until Reset.
func (e *Engine) SetCategoryDir(name, dir string) error {
	name, dir = strings.TrimSpace(name), strings.TrimSpace(dir)
	if name == "" || dir == "" {
		return fmt.Errorf("%w: missing category or folder", ErrConflict)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	e.overrides[name] = dir
	e.cfg.Categories[name] = dir
	e.saveSettingsLocked()
	return nil
}

// ResetPaths clears every remembered custom folder and returns the base download
// folder to the app default. Existing files are left where they are.
func (e *Engine) ResetPaths() error {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	e.cfg.DownloadDir = e.defaultDir
	e.dirChosen = true // Reset is a choice too: back to the app default, kept
	e.cfg.Categories = config.DefaultCategories(e.defaultDir)
	e.overrides = map[string]string{}
	ytdlp.SetSearchDirs(e.defaultDir)
	os.MkdirAll(e.defaultDir, 0o755)
	e.saveSettingsLocked()
	return nil
}

// Pause stops a queued or downloading task, keeping segment progress.
func (e *Engine) Pause(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return ErrNotFound
	}
	switch t.Status {
	case StatusQueued:
		e.queue = slices.DeleteFunc(e.queue, func(q string) bool { return q == id })
		t.Status = StatusPaused
		e.saveLocked()
		return nil
	case StatusDownloading:
		t.intent = intentPause
		t.cancel()
		return nil // runTask finalizes to paused and persists
	default:
		return fmt.Errorf("%w: cannot pause a %s task", ErrConflict, t.Status)
	}
}

// Resume re-queues a paused or failed task. Ranged tasks continue from their
// persisted segment offsets; non-ranged ones restart from zero.
func (e *Engine) Resume(id string) error {
	e.mu.Lock()
	t, ok := e.tasks[id]
	if !ok {
		e.mu.Unlock()
		return ErrNotFound
	}
	if t.Status != StatusPaused && t.Status != StatusFailed {
		e.mu.Unlock()
		return fmt.Errorf("%w: cannot resume a %s task", ErrConflict, t.Status)
	}
	t.Status = StatusQueued
	t.Error = ""
	t.Skip = false // explicitly starting a skip-tagged item un-skips it
	e.queue = append(e.queue, id)
	e.saveLocked()
	e.mu.Unlock()
	e.poke()
	return nil
}

// Delete removes the task; removeFile also deletes downloaded data.
func (e *Engine) Delete(id string, removeFile bool) error {
	e.mu.Lock()
	t, ok := e.tasks[id]
	if !ok {
		e.mu.Unlock()
		return ErrNotFound
	}
	delete(e.tasks, id)
	e.order = slices.DeleteFunc(e.order, func(q string) bool { return q == id })
	e.queue = slices.DeleteFunc(e.queue, func(q string) bool { return q == id })

	if t.Status == StatusDownloading {
		// Worker owns the file handle; it cleans up on exit.
		t.intent = intentDelete
		t.deleteFile = removeFile
		t.cancel()
		e.saveLocked()
		e.mu.Unlock()
		return nil
	}
	e.saveLocked()
	e.mu.Unlock()

	os.Remove(e.partPath(t))
	e.removeBurstTmp(t)
	e.removeHLSWork(t)
	if removeFile && t.FinalPath != "" {
		os.Remove(t.FinalPath)
	}
	e.log.Info("task deleted", "id", id, "removedFile", removeFile)
	return nil
}

func (e *Engine) List() []TaskView {
	e.mu.Lock()
	defer e.mu.Unlock()
	views := make([]TaskView, 0, len(e.order))
	for i := len(e.order) - 1; i >= 0; i-- { // newest first
		if t, ok := e.tasks[e.order[i]]; ok {
			views = append(views, e.viewLocked(t))
		}
	}
	return views
}

func (e *Engine) Get(id string) (TaskView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return TaskView{}, ErrNotFound
	}
	return e.viewLocked(t), nil
}

// FilePath returns the on-disk path of a completed download.
func (e *Engine) FilePath(id string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return "", ErrNotFound
	}
	if t.Status != StatusCompleted || t.FinalPath == "" {
		return "", fmt.Errorf("%w: download not completed", ErrConflict)
	}
	return t.FinalPath, nil
}

// RenameFile renames a completed download's file on disk (within its folder).
func (e *Engine) RenameFile(id, newName string) (TaskView, error) {
	newName = SanitizeFileName(newName)
	if newName == "" {
		return TaskView{}, fmt.Errorf("%w: empty file name", ErrConflict)
	}

	e.mu.Lock()
	t, ok := e.tasks[id]
	if !ok {
		e.mu.Unlock()
		return TaskView{}, ErrNotFound
	}
	if t.Status != StatusCompleted || t.FinalPath == "" {
		e.mu.Unlock()
		return TaskView{}, fmt.Errorf("%w: only completed downloads can be renamed", ErrConflict)
	}
	oldPath := t.FinalPath
	e.mu.Unlock()

	if filepath.Base(oldPath) == newName {
		return e.Get(id) // no-op
	}
	newPath := filepath.Join(filepath.Dir(oldPath), newName)
	if _, err := os.Stat(newPath); err == nil {
		return TaskView{}, fmt.Errorf("%w: a file named %q already exists in that folder", ErrConflict, newName)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return TaskView{}, fmt.Errorf("rename failed: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok = e.tasks[id]
	if !ok {
		return TaskView{}, ErrNotFound
	}
	t.FinalPath = newPath
	t.FileName = newName
	e.saveLocked()
	e.log.Info("file renamed", "id", id, "name", newName)
	return e.viewLocked(t), nil
}

// MoveFile moves a completed download's file to another folder, keeping its
// name. Moving into a category folder re-buckets it in the UI (category is
// derived from the folder). Falls back to copy+delete across volumes.
func (e *Engine) MoveFile(id, newDir string) (TaskView, error) {
	newDir = strings.TrimSpace(newDir)
	if newDir == "" {
		return TaskView{}, fmt.Errorf("%w: empty destination folder", ErrConflict)
	}

	e.mu.Lock()
	t, ok := e.tasks[id]
	if !ok {
		e.mu.Unlock()
		return TaskView{}, ErrNotFound
	}
	if t.Status != StatusCompleted || t.FinalPath == "" {
		e.mu.Unlock()
		return TaskView{}, fmt.Errorf("%w: only completed downloads can be moved", ErrConflict)
	}
	oldPath, name := t.FinalPath, t.FileName
	e.mu.Unlock()

	newPath := filepath.Join(newDir, name)
	if filepath.Clean(newPath) == filepath.Clean(oldPath) {
		return e.Get(id) // already there
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		return TaskView{}, fmt.Errorf("create folder: %w", err)
	}
	if _, err := os.Stat(newPath); err == nil {
		return TaskView{}, fmt.Errorf("%w: %q already exists in that folder", ErrConflict, name)
	}
	if err := moveFile(oldPath, newPath); err != nil {
		return TaskView{}, fmt.Errorf("move failed: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok = e.tasks[id]
	if !ok {
		return TaskView{}, ErrNotFound
	}
	t.FinalPath = newPath
	t.Dir = newDir
	e.saveLocked()
	e.log.Info("file moved", "id", id, "dir", newDir)
	return e.viewLocked(t), nil
}

// moveFile renames within a volume, else copies then removes the source.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	in.Close()
	return os.Remove(src)
}

// Shutdown interrupts running tasks (they persist as queued and auto-resume
// next launch) and flushes state.
func (e *Engine) Shutdown(timeout time.Duration) {
	e.mu.Lock()
	for _, t := range e.tasks {
		if t.Status == StatusDownloading && t.cancel != nil {
			t.intent = intentShutdown
			t.cancel()
		}
	}
	e.mu.Unlock()

	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		e.log.Warn("shutdown timeout waiting for workers")
	}

	e.mu.Lock()
	e.saveLocked()
	e.mu.Unlock()
}

// runTask owns a task from downloading to a terminal state.
func (e *Engine) runTask(ctx context.Context, t *Task) {
	defer e.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("panic in task worker", "id", t.ID, "panic", r)
			e.finishTask(t, fmt.Errorf("internal error: %v", r))
		}
	}()
	e.log.Info("task starting", "id", t.ID, "url", t.URL)

	switch t.Kind {
	case "ytdlp":
		e.runYtdlp(ctx, t)
	case "hls":
		e.runHLS(ctx, t)
	default:
		e.runHTTP(ctx, t)
	}
}

// runHTTP downloads a plain file with the segmented engine. On first run it
// probes the URL, and a probe can re-route the task: an HLS playlist goes to the
// native HLS downloader, a DASH manifest or a web page to yt-dlp.
func (e *Engine) runHTTP(ctx context.Context, t *Task) {
	var (
		rangeRestarts int  // re-probes after the server ignored a range
		expired       int  // re-probes after the resolved address expired
		asFile        bool // yt-dlp said the page is a plain file (errRetryAsFile)
	)
	for {
		if !t.Probed {
			e.setNote(t, "Connecting…")
			e.mu.Lock()
			prev := snapshotEntity(t)
			e.mu.Unlock()
			o := downloadProbe
			o.title, o.name = t.Title, t.FileName
			pr, err := e.probeWith(ctx, t.URL, t.reqInfo(), o)
			if err != nil {
				if ctx.Err() != nil {
					e.finishInterrupted(t)
					return
				}
				if e.handToYtdlp(ctx, t, err) {
					return
				}
				e.finishTask(t, fmt.Errorf("probe: %w", err))
				return
			}
			switch pr.Kind {
			case kindHLS:
				e.adoptStream(t, pr)
				e.log.Info("probed: HLS stream", "id", t.ID, "file", t.FileName)
				e.runHLS(ctx, t)
				return
			case kindDASH, kindPage:
				if asFile {
					// yt-dlp resolved the page to a file, and that address answers
					// with a page too: going back to yt-dlp would loop.
					e.finishTask(t, newLandingError(pr.FinalURL, false))
					return
				}
				if err := e.convertToVideo(ctx, t, pr); err != nil {
					if ctx.Err() != nil {
						e.finishInterrupted(t)
						return
					}
					if errors.Is(err, errRetryAsFile) {
						asFile = true
						e.mu.Lock()
						t.Kind, t.Probed, t.FetchURL = "", false, ""
						e.saveLocked()
						e.mu.Unlock()
						e.log.Info("yt-dlp found a plain file behind the page", "id", t.ID, "url", t.URL)
						continue
					}
					if pr.Kind == kindPage && pr.Expected != "" {
						// A file link answered with a page yt-dlp can't use either:
						// say that, not "no video found".
						e.log.Info("page instead of file; yt-dlp found nothing", "id", t.ID, "err", err)
						err = newLandingError(pr.FinalURL, false)
					}
					e.finishTask(t, err)
					return
				}
				e.log.Info("probed: handing to yt-dlp", "id", t.ID, "kind", pr.Kind, "file", t.FileName)
				e.runYtdlp(ctx, t)
				return
			}
			e.mu.Lock()
			keep := prev.sameFile(pr)
			t.Size = pr.Size
			t.Ranged = pr.Ranged
			t.ETag = pr.ETag
			t.LastModified = pr.LastModified
			t.ContentType = pr.ContentType
			t.Referer = pr.Referer
			t.FetchURL = fetchURLFor(t.URL, pr)
			if rangeRestarts > 0 {
				// A restart after the server stopped honoring If-Range: don't send
				// the validators again.
				t.ETag, t.LastModified = "", ""
			}
			if rangeRestarts > 1 {
				t.Ranged = false // ranges keep failing: one plain stream
			}
			if t.FileName == "" {
				t.FileName = pr.FileName
			} else {
				t.FileName = fixUserExt(t.FileName, pr.FileName, pr.Magic)
			}
			if !keep {
				if prev.progress {
					os.Remove(e.partPath(t)) // another file now: nothing on disk is reusable
				}
				t.Segments = planSegments(t.Size, t.Ranged, t.WantSegments, e.minSegmentSize())
			}
			t.Probed = true
			t.note = ""
			e.saveLocked()
			e.mu.Unlock()
			e.log.Info("probed", "id", t.ID, "size", t.Size, "ranged", t.Ranged, "segments", len(t.Segments),
				"file", t.FileName, "fetch", t.FetchURL, "landing", pr.Landing, "kept", keep)
		}

		err := e.transferHTTP(ctx, t)
		switch {
		case ctx.Err() != nil: // pause / delete / shutdown
			e.finishInterrupted(t)
			return
		case err == nil:
			e.finalizeHTTP(t)
			return
		case errors.Is(err, errServerIgnoredRange) && rangeRestarts < 2:
			// The server sent the whole file instead of a range (or another range,
			// or another file's): the file changed or its validators are
			// unreliable. Start over from a fresh probe rather than failing
			// forever on every resume.
			rangeRestarts++
			e.log.Info("server ignored range — restarting download", "id", t.ID, "attempt", rangeRestarts)
			os.Remove(e.partPath(t))
			e.mu.Lock()
			t.Probed, t.FetchURL, t.Segments = false, "", nil
			e.saveLocked()
			e.mu.Unlock()
			continue
		case errors.Is(err, errFetchExpired) && expired < 2:
			// The address the probe resolved stopped working (signed CDN links
			// expire): ask the original link again. The re-probe keeps the
			// progress when it provably finds the same file.
			expired++
			e.log.Info("resolved address expired — probing the link again", "id", t.ID, "err", err)
			e.mu.Lock()
			t.Probed, t.FetchURL = false, ""
			e.saveLocked()
			e.mu.Unlock()
			continue
		default:
			e.finishTask(t, err)
			return
		}
	}
}

// fixUserExt gives a user-chosen name the detected extension when it has none
// (the New Download box pre-fills bare URL names like "videoplayback"), and
// corrects one the file's magic bytes contradict across content families: the
// browser suggests "clip.mp4" for what is really MP3 audio. magic is the
// probe's sniffExt ("" = unrecognized).
func fixUserExt(user, resolved, magic string) string {
	if _, ext := splitExt(user); meaningfulExt(ext) {
		return reconcileExt(user, magic)
	}
	if _, ext := splitExt(resolved); meaningfulExt(ext) {
		return SanitizeFileName(user + "." + ext)
	}
	return user
}

func (e *Engine) minSegmentSize() int64 {
	if e.cfg != nil && e.cfg.MinSegmentSize > 0 {
		return e.cfg.MinSegmentSize
	}
	return 512 << 10
}

// transferHTTP runs one worker per incomplete segment into the .part file.
func (e *Engine) transferHTTP(ctx context.Context, t *Task) error {
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(e.partPath(t), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if t.Ranged && t.Size > 0 {
		if err := f.Truncate(t.Size); err != nil { // preallocate
			return err
		}
	}

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	for _, seg := range t.Segments {
		if seg.Remaining() == 0 {
			continue
		}
		wg.Add(1)
		go func(s *Segment) {
			defer wg.Done()
			if err := e.downloadSegment(wctx, t, s, f); err != nil && !errors.Is(err, context.Canceled) {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				wcancel() // one segment failing hard stops the rest
			}
		}(seg)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if firstErr != nil {
		return firstErr
	}
	return f.Sync()
}

// finalizeHTTP renames the finished .part file to a collision-free final name
// and marks the task completed — unless the bytes are an empty answer or a web
// page under a binary name, which fails the task (and discards the junk, so a
// retry probes afresh) instead of completing it.
func (e *Engine) finalizeHTTP(t *Task) {
	if err := verifyPart(e.partPath(t), t.FileName, t.fetchURL()); err != nil {
		os.Remove(e.partPath(t))
		e.mu.Lock()
		t.Probed, t.FetchURL, t.Segments = false, "", nil
		e.mu.Unlock()
		e.finishTask(t, err)
		return
	}
	finalPath := uniquePath(filepath.Join(t.Dir, t.FileName))
	if err := os.Rename(e.partPath(t), finalPath); err != nil {
		e.finishTask(t, fmt.Errorf("finalize: %w", err))
		return
	}

	e.mu.Lock()
	if t.Size <= 0 {
		t.Size = t.downloaded() // learned size of a chunked transfer
	}
	now := time.Now()
	t.Status = StatusCompleted
	t.CompletedAt = &now
	t.FinalPath = finalPath
	t.FileName = filepath.Base(finalPath)
	t.SizeEstimated = false
	t.note = ""
	t.cancel = nil
	e.running--
	e.completionsSinceArm++ // a real completion (not a pause/fail) counts toward auto-shutdown
	e.saveLocked()
	e.mu.Unlock()
	e.poke()
	e.log.Info("task completed", "id", t.ID, "file", finalPath)
	e.notifyCompleted(t.ID)
}

// setNote sets the short status note the UI shows while a task works.
func (e *Engine) setNote(t *Task, note string) {
	e.mu.Lock()
	t.note = note
	e.mu.Unlock()
}

// splitLinesCR is a bufio.SplitFunc that breaks on \r OR \n, so progress lines a
// downloader overwrites in place with a carriage return (aria2c, curl, ffmpeg)
// are read incrementally instead of buffering until the final newline.
func splitLinesCR(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil // need more data
}

// runYtdlp runs a video task. The fast path (runYtdlpBurst) has yt-dlp resolve
// the direct googlevideo URLs, then MyIDM downloads them itself with the burst
// technique — many small parallel range requests, each landing in googlevideo's
// fast burst window — and ffmpeg muxes. Measured ~82 MB/s vs ~12 for yt-dlp's
// own aria2c path (IDM parity). Anything the fast path can't handle falls back
// to runYtdlpNative (yt-dlp + aria2c), so behavior never regresses.
func (e *Engine) runYtdlp(ctx context.Context, t *Task) {
	e.ensureFFmpeg(ctx, t)
	if ctx.Err() != nil {
		e.finishInterrupted(t)
		return
	}
	if e.runYtdlpBurst(ctx, t) {
		return
	}
	e.runYtdlpNative(ctx, t)
}

// ytdlpOpts is the browser context a task hands to yt-dlp. A User-Agent the
// browser supplied (with its cookies) wins over ours: Cloudflare ties its
// clearance cookie to the exact agent that earned it.
func (e *Engine) ytdlpOpts(t *Task) ytdlp.Opts {
	o := ytdlp.Opts{UserAgent: e.userAgent(), Referer: t.Referer, PlaylistItem: t.PlaylistItem}
	for k, v := range t.Headers {
		if strings.EqualFold(k, "User-Agent") {
			o.UserAgent = v
			continue
		}
		if o.Headers == nil {
			o.Headers = map[string]string{}
		}
		o.Headers[k] = v
	}
	return o
}

// ensureFFmpeg makes ffmpeg available for merging/converting, fetching it once
// on first need. Best-effort: without it yt-dlp falls back to single-file
// formats (see ytdlp.SelectorWithoutFFmpeg) and HLS captures stay .ts.
func (e *Engine) ensureFFmpeg(ctx context.Context, t *Task) {
	if ytdlp.HasFFmpeg() {
		return
	}
	e.setNote(t, "Getting ffmpeg (one-time)…")
	_, err := provisionFFmpeg(ctx, func(done, total int64) {
		if total > 0 {
			e.setNote(t, fmt.Sprintf("Getting ffmpeg (one-time)… %d%%", done*100/total))
		}
	})
	e.setNote(t, "")
	if err != nil && ctx.Err() == nil {
		e.log.Warn("ffmpeg unavailable — videos needing a merge fall back to single-file formats", "err", err)
	}
}

// runYtdlpBurst is the fast video path. It returns true once it has driven the
// task to a terminal state (completed, or interruption finalized); false to tell
// the caller to fall back to runYtdlpNative — in which case it has removed any
// temp files and left the task's status untouched, so the fallback starts clean.
func (e *Engine) runYtdlpBurst(ctx context.Context, t *Task) bool {
	// Audio-only (mp3) needs yt-dlp's extractor postprocessor; muxing needs
	// ffmpeg. Neither qualifies, so let yt-dlp handle them.
	if t.Audio || !ytdlp.HasFFmpeg() {
		return false
	}
	// Only YouTube pages resolve to googlevideo URLs the burst trick speeds up;
	// elsewhere the `yt-dlp -g` run is a wasted extraction (~20 s on Odysee).
	if !ytdlp.IsYouTubeURL(t.URL) {
		return false
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return false
	}

	rctx, rcancel := context.WithTimeout(ctx, 90*time.Second)
	urls, err := ytdlp.ResolveURLs(rctx, t.URL, t.Selector, e.ytdlpOpts(t))
	rcancel()
	// Only handle the clean 1-stream (progressive) or 2-stream (video+audio)
	// googlevideo case; anything else (errors, playlists, >2 streams, other
	// hosts) goes to yt-dlp.
	if err != nil || len(urls) == 0 || len(urls) > 2 {
		return false
	}
	for _, u := range urls {
		if !isGoogleVideo(u) {
			return false
		}
	}

	// Probe stream sizes up front so the row shows a real total/percentage.
	var total int64
	for _, u := range urls {
		pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
		n, perr := e.burstProbeLen(pctx, u, e.userAgent())
		pcancel()
		if perr != nil || n <= 0 {
			return false
		}
		total += n
	}
	e.mu.Lock()
	t.Size = total
	t.SizeEstimated = false
	t.Ranged = true // burst uses HTTP range requests, so this download resumes
	if len(t.Segments) > 0 {
		t.Segments[0].End = total - 1
		t.Segments[0].SetDone(0)
	}
	e.mu.Unlock()

	// Cumulative progress across video+audio into the one synthetic segment.
	var (
		done   int64
		doneMu sync.Mutex
	)
	onBytes := func(n int64) {
		doneMu.Lock()
		done += n
		cur := done
		doneMu.Unlock()
		if len(t.Segments) > 0 {
			t.Segments[0].SetDone(cur)
		}
	}

	tmp := make([]string, len(urls))
	cleanup := func() {
		for _, p := range tmp {
			if p != "" {
				os.Remove(p)
				os.Remove(progPath(p))
			}
		}
	}
	for i, u := range urls {
		tmp[i] = filepath.Join(t.Dir, fmt.Sprintf("%s.%s.f%d.part", t.FileName, t.ID, i))
		if ferr := e.burstFetch(ctx, u, tmp[i], e.userAgent(), onBytes); ferr != nil {
			if ctx.Err() != nil { // paused / deleted / shutdown — keep partials so resume continues
				e.finishInterrupted(t)
				return true
			}
			cleanup() // genuine failure: drop partials and fall back to yt-dlp
			e.log.Info("burst fetch failed; falling back to yt-dlp", "id", t.ID, "err", ferr)
			return false
		}
	}

	finalPath := uniquePath(filepath.Join(t.Dir, t.FileName))
	if len(urls) == 1 {
		if rerr := os.Rename(tmp[0], finalPath); rerr != nil {
			cleanup()
			return false
		}
		cleanup() // .part is now the final file; drop the .prog resume sidecar
	} else {
		e.setNote(t, "Merging…")
		mctx, mcancel := context.WithTimeout(ctx, 5*time.Minute)
		muxErr := ytdlp.MuxCmd(mctx, tmp[0], tmp[1], finalPath).Run()
		mcancel()
		cleanup()
		if muxErr != nil {
			os.Remove(finalPath)
			if ctx.Err() != nil {
				e.finishInterrupted(t)
				return true
			}
			e.log.Info("mux failed; falling back to yt-dlp", "id", t.ID, "err", muxErr)
			return false
		}
	}

	e.completeTask(t, finalPath)
	e.log.Info("video task completed (burst)", "id", t.ID, "file", finalPath, "size", total)
	e.notifyCompleted(t.ID)
	return true
}

// completeTask finalizes a successful download to the completed state and fires
// the completion popup. Used by the burst fast path (the segmented HTTP and
// yt-dlp paths inline their own equivalent).
func (e *Engine) completeTask(t *Task, finalPath string) {
	e.mu.Lock()
	now := time.Now()
	if st, err := os.Stat(finalPath); err == nil {
		t.Size = st.Size()
		if len(t.Segments) > 0 {
			t.Segments[0].End = t.Size - 1
			t.Segments[0].SetDone(t.Size)
		}
	}
	t.Status = StatusCompleted
	t.CompletedAt = &now
	t.FinalPath = finalPath
	t.FileName = filepath.Base(finalPath)
	t.SizeEstimated = false
	t.Live = false
	t.note = ""
	t.cancel = nil
	e.running--
	e.completionsSinceArm++ // a real completion (not a pause/fail) counts toward auto-shutdown
	e.saveLocked()
	e.mu.Unlock()
	e.poke()
}

// runYtdlpNative, ytdlpAttempt and the yt-dlp output helpers live in ytdlprun.go.

// finishTask transitions a running task to failed.
func (e *Engine) finishTask(t *Task, err error) {
	e.mu.Lock()
	t.Status = StatusFailed
	t.Error = err.Error()
	t.note = ""
	t.cancel = nil
	e.running--
	e.saveLocked()
	e.mu.Unlock()
	e.poke()
	e.log.Warn("task failed", "id", t.ID, "err", err)
}

// finishInterrupted resolves a cancelled run according to the recorded intent.
func (e *Engine) finishInterrupted(t *Task) {
	e.mu.Lock()
	intent := t.intent
	t.intent = intentNone
	t.note = ""
	t.cancel = nil
	e.running--

	switch intent {
	case intentDelete:
		// Task already removed from registry by Delete; clean files here.
		e.mu.Unlock()
		os.Remove(e.partPath(t))
		e.removeBurstTmp(t)
		e.removeHLSWork(t)
		if t.deleteFile && t.FinalPath != "" {
			os.Remove(t.FinalPath)
		}
		e.poke()
		e.log.Info("task deleted while running", "id", t.ID)
		return
	case intentShutdown:
		t.Status = StatusQueued // auto-continue next session
	default: // pause (or root context cancelled)
		t.Status = StatusPaused
	}
	state := t.Status // read under the lock: a Resume may flip it right after
	e.saveLocked()
	e.mu.Unlock()
	e.poke()
	e.log.Info("task interrupted", "id", t.ID, "state", state)
}

func (e *Engine) partPath(t *Task) string {
	name := t.FileName
	if name == "" {
		name = "download"
	}
	return filepath.Join(t.Dir, fmt.Sprintf("%s.%s.part", name, t.ID))
}

// removeBurstTmp deletes a video task's burst working files and their resume
// sidecars (at most two streams: video + audio). A safe no-op for non-video
// tasks or ones already cleaned up; successful downloads clean up inline.
func (e *Engine) removeBurstTmp(t *Task) {
	for i := 0; i < 2; i++ {
		p := filepath.Join(t.Dir, fmt.Sprintf("%s.%s.f%d.part", t.FileName, t.ID, i))
		os.Remove(p)
		os.Remove(progPath(p))
	}
	// yt-dlp's own work files ("<title>.<id>.*"); a finished download has been
	// renamed away from that name (or, if the rename failed, is kept).
	if t.Kind == "ytdlp" && t.Title != "" && t.Status != StatusCompleted {
		removeWorkFiles(t)
	}
}

// removeHLSWork deletes an HLS task's segment folder (no-op for other kinds).
func (e *Engine) removeHLSWork(t *Task) {
	if t.Kind == "hls" {
		os.RemoveAll(e.hlsWorkDir(t))
	}
}

// viewLocked snapshots a task for the API. Caller holds e.mu.
func (e *Engine) viewLocked(t *Task) TaskView {
	v := TaskView{
		ID:          t.ID,
		URL:         t.URL,
		FileName:    t.FileName,
		Dir:         t.Dir,
		Size:        t.Size,
		Ranged:      t.Ranged,
		Status:      t.Status,
		Kind:        t.Kind,
		Error:       t.Error,
		Note:        t.note,
		Referer:     t.Referer,
		SizeEst:     t.SizeEstimated,
		Live:        t.Live,
		Downloaded:  t.downloaded(),
		Speed:       t.speed,
		SpeedMax:    t.speedMax,
		ETA:         -1,
		Progress:    -1,
		CreatedAt:   t.CreatedAt,
		CompletedAt: t.CompletedAt,
		ScheduledAt: t.ScheduledAt,
		Later:       t.Later,
		Skip:        t.Skip,
		Description: t.Description,
	}
	// avg/min over the rolling window (max stays all-time, set above). All three
	// come from the same smoothed EMA series, so min <= avg <= max always holds.
	if n := len(t.recent); n > 0 {
		sum, mn := 0.0, t.recent[0]
		for _, s := range t.recent {
			sum += s
			if s < mn {
				mn = s
			}
		}
		v.SpeedAvg = sum / float64(n)
		v.SpeedMin = mn
	}
	for _, s := range t.Segments {
		v.Segments = append(v.Segments, SegmentView{Start: s.Start, End: s.End, Done: s.Done()})
	}
	if t.Size > 0 && !t.Live {
		v.Progress = float64(v.Downloaded) / float64(t.Size)
		if v.Progress > 1 {
			v.Progress = 1
		}
		if t.Status == StatusDownloading && v.Speed > 0 && v.Downloaded < t.Size {
			v.ETA = float64(t.Size-v.Downloaded) / v.Speed
		}
	}
	if t.Status == StatusCompleted {
		v.Progress = 1
		v.FilePath = t.FinalPath
		if st, err := os.Stat(t.FinalPath); t.FinalPath != "" && err == nil && !st.IsDir() {
			v.FileExists = true
			// Changes whenever the file on disk is replaced, so the UI's icon URL
			// (and the server's icon cache) never shows a previous file's icon.
			v.FileVersion = fmt.Sprintf("%x-%x", st.ModTime().UnixNano(), st.Size())
		}
	}
	return v
}

// fileExistsOnDisk reports whether p names an existing regular file.
func fileExistsOnDisk(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// saveLocked persists the registry. Caller holds e.mu.
func (e *Engine) saveLocked() {
	tasks := make([]*Task, 0, len(e.order))
	for _, id := range e.order {
		if t, ok := e.tasks[id]; ok {
			tasks = append(tasks, t)
		}
	}
	if err := e.store.Save(tasks); err != nil {
		e.log.Error("persist failed", "err", err)
	}
}

// uniquePath returns p, or "name (n).ext" if p already exists.
func uniquePath(p string) string {
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); errors.Is(err, os.ErrNotExist) {
			return cand
		}
	}
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// provisionFFmpeg fetches ffmpeg on first need (a ~190 MB one-time download).
// A variable so tests can run offline without it.
var provisionFFmpeg = ytdlp.EnsureFFmpeg
