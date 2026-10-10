package engine

// Link Grabber, part 2: the job runner.
//
// A grab job takes one pasted page address (or a block of text with links)
// and fills a list of everything downloadable on it, the way JDownloader's
// LinkGrabber does: the page's own files, media and streams (extract.go reads
// the HTML), the videos yt-dlp can extract from it or from its embedded
// players, and one level of iframes. Every candidate is then probed with the
// same Inspect the New Download dialog uses — same User-Agent, cookies and
// Referer rules as the download itself — so the list shows real names, sizes
// and types, and addresses that turn out to be web pages are dropped.
//
// Jobs run in the background and are polled: items appear as soon as they are
// found (pending → probing → ok/error), so a slow yt-dlp run or a hung CDN
// never holds the rest of the list back. A job has a hard budget (grabBudget);
// whatever is still unchecked when it runs out stays in the list, marked
// "skipped", rather than being lost.

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"myidm/internal/ytdlp"
)

const (
	grabBudget      = 90 * time.Second // whole job
	grabItemTimeout = 15 * time.Second // one candidate's probe
	grabPageTimeout = 20 * time.Second // the page itself, a frame, a landing page
	grabVideoWait   = 75 * time.Second // one yt-dlp probe
	grabWorkers     = 6                // concurrent candidate probes
	grabPageMax     = 4 << 20          // bytes of HTML read from a page
	grabMaxFrames   = 5                // iframes / landing pages read one level deep
	grabMaxEmbeds   = 6                // embedded players handed to yt-dlp
	grabMaxItems    = 300              // list length cap (image galleries…)
	grabJobTTL      = 10 * time.Minute // a finished job is forgotten after this
	grabMaxJobs     = 32               // live jobs kept at once
	grabSmallBytes  = 8 << 10          // images below this are "small" (hidden by default)
	grabSmallPixels = 64               // …or narrower/shorter than this
)

// Grab job states.
const (
	GrabProbing  = "probing"  // checking what the pasted address is
	GrabScanning = "scanning" // it's a page: collecting and probing candidates
	GrabDone     = "done"
	GrabError    = "error" // nothing found and the page could not be read
)

// ExtCategory maps a lowercase file extension to its D BOX category (the
// folder a download lands in). Unknown extensions are "General".
var ExtCategory = map[string]string{
	"zip": "Compressed", "rar": "Compressed", "7z": "Compressed", "tar": "Compressed",
	"gz": "Compressed", "bz2": "Compressed", "xz": "Compressed", "zst": "Compressed",
	"iso": "Compressed", "cab": "Compressed", "tgz": "Compressed", "lz": "Compressed",
	"lzma": "Compressed",
	"pdf":  "Documents", "doc": "Documents", "docx": "Documents", "xls": "Documents",
	"xlsx": "Documents", "ppt": "Documents", "pptx": "Documents", "txt": "Documents",
	"odt": "Documents", "csv": "Documents", "epub": "Documents", "rtf": "Documents",
	"ods": "Documents", "odp": "Documents", "mobi": "Documents", "djvu": "Documents",
	"cbz": "Documents", "cbr": "Documents",
	"mp3": "Music", "wav": "Music", "flac": "Music", "aac": "Music", "ogg": "Music",
	"m4a": "Music", "wma": "Music", "opus": "Music", "m4b": "Music", "oga": "Music",
	"weba": "Music", "aiff": "Music", "alac": "Music",
	"mp4": "Video", "mkv": "Video", "avi": "Video", "mov": "Video", "webm": "Video",
	"flv": "Video", "wmv": "Video", "m4v": "Video", "mpg": "Video", "mpeg": "Video",
	"ts": "Video", "3gp": "Video", "ogv": "Video", "f4v": "Video",
	"exe": "Programs", "msi": "Programs", "apk": "Programs", "dmg": "Programs",
	"deb": "Programs", "rpm": "Programs", "appimage": "Programs", "msix": "Programs",
	"bat": "Programs", "jar": "Programs", "xapk": "Programs", "apks": "Programs",
	"aab": "Programs", "appx": "Programs", "pkg": "Programs",
	"jpg": "Images", "jpeg": "Images", "png": "Images", "gif": "Images", "webp": "Images",
	"bmp": "Images", "svg": "Images", "tiff": "Images", "ico": "Images", "heic": "Images",
	"psd": "Images", "avif": "Images", "tif": "Images", "jfif": "Images",
}

var (
	grabArchiveExts  = grabExtSet("zip rar 7z tar gz tgz bz2 xz zst iso cab lz lzma arj z")
	grabDocExts      = grabExtSet("pdf doc docx xls xlsx ppt pptx txt odt ods odp csv epub mobi rtf cbz cbr djvu")
	grabProgramExts  = grabExtSet("exe msi apk xapk apks aab dmg deb rpm appimage msix appx jar bat pkg")
	grabSubtitleExts = grabExtSet("srt vtt ass ssa sub ttml")
)

// GrabRequest starts a job: URL crawls that page; Text/URLs probe the links
// pasted as text (no crawl). Referer/Headers are the browser context the
// page (and every download from it) is fetched with.
type GrabRequest struct {
	URL     string
	Text    string
	URLs    []string
	Referer string
	Headers map[string]string
	Title   string // the page's title if the caller knows it (browser tab)

	// ProbeVideo asks yt-dlp what a page offers. The server passes its shared
	// probe cache here; nil means ytdlp.Probe when yt-dlp is installed.
	ProbeVideo func(ctx context.Context, rawURL string, o ytdlp.Opts) (*ytdlp.ProbeResult, error) `json:"-"`
}

// GrabOption is one quality choice of a yt-dlp item (same shape as ytdlp's).
type GrabOption struct {
	Label    string `json:"label"`
	Selector string `json:"selector"`
	Ext      string `json:"ext"`
	Audio    bool   `json:"audio"`
	Size     int64  `json:"size"`
}

// GrabVideo carries what POST /api/video needs for a yt-dlp item.
type GrabVideo struct {
	Title        string       `json:"title"`
	Options      []GrabOption `json:"options"`
	PlaylistItem int          `json:"playlistItem,omitempty"` // 1-based entry of a multi-video page
}

// GrabItem is one row of the grabber's list.
type GrabItem struct {
	ID            string     `json:"id"`
	URL           string     `json:"url"`
	Name          string     `json:"name"`
	Ext           string     `json:"ext"`
	Kind          string     `json:"kind"`     // video audio image archive document program stream subtitle torrent page other
	Category      string     `json:"category"` // D BOX category (folder)
	Size          int64      `json:"size"`     // -1 unknown
	SizeEstimated bool       `json:"sizeEstimated,omitempty"`
	ContentType   string     `json:"contentType,omitempty"`
	Width         int        `json:"width,omitempty"`
	Height        int        `json:"height,omitempty"`
	Duration      float64    `json:"duration,omitempty"`
	Live          bool       `json:"live,omitempty"`
	Small         bool       `json:"small,omitempty"`       // tiny image: hidden by default
	Recommended   bool       `json:"recommended,omitempty"` // the page's main video: preselect it
	Source        string     `json:"source"`                // direct ytdlp video-tag audio-tag source-tag img link meta jsonld script iframe player-config text
	FoundOn       string     `json:"foundOn"`               // page it was found on: its Referer
	Status        string     `json:"status"`                // pending probing ok error skipped
	Error         string     `json:"error,omitempty"`
	Video         *GrabVideo `json:"video,omitempty"` // yt-dlp item: queue via /api/video
	PlaylistItem  int        `json:"playlistItem,omitempty"`
	Thumb         string     `json:"thumb,omitempty"`
}

// GrabSnapshot is a job's state as the UI polls it.
type GrabSnapshot struct {
	ID         string         `json:"id"`
	URL        string         `json:"url"`
	Mode       string         `json:"mode"` // "page" or "text"
	Status     string         `json:"status"`
	Direct     bool           `json:"direct"` // the address itself is a file/stream: use the New Download dialog
	Error      string         `json:"error,omitempty"`
	Title      string         `json:"title"`     // best title: the page's, cleaned
	PageTitle  string         `json:"pageTitle"` // <title> / og:title as the page gives it
	Ytdlp      string         `json:"ytdlp"`     // "", running, done, error, none (not installed)
	YtdlpError string         `json:"ytdlpError,omitempty"`
	Pending    int            `json:"pending"` // items still waiting for their probe
	Items      []GrabItem     `json:"items"`
	Counts     map[string]int `json:"counts"`
}

// Grabber owns the running and recently finished grab jobs.
type Grabber struct {
	e    *Engine
	mu   sync.Mutex
	jobs map[string]*grabJob
}

// NewGrabber returns a job registry that probes with e.
func NewGrabber(e *Engine) *Grabber {
	return &Grabber{e: e, jobs: map[string]*grabJob{}}
}

// Start validates req and starts a job in the background; the id is what
// Get and Cancel take.
func (g *Grabber) Start(req GrabRequest) (string, error) {
	j := &grabJob{e: g.e, req: req, seen: map[string]bool{}}
	j.q.cond = sync.NewCond(&j.q.mu)
	j.ri = reqInfo{Referer: cleanReferer(req.Referer), Headers: CleanHeaders(req.Headers)}
	j.probeVideo = req.ProbeVideo
	if j.probeVideo == nil && ytdlp.Available() {
		j.probeVideo = ytdlp.Probe
	}
	if strings.TrimSpace(req.URL) != "" {
		u, err := parseDownloadURL(req.URL)
		if err != nil {
			return "", err
		}
		j.pageURL = u.String()
		j.snap = GrabSnapshot{URL: j.pageURL, Mode: "page", Status: GrabProbing}
	} else {
		j.links = grabTextLinks(req.Text, req.URLs)
		if len(j.links) == 0 {
			return "", errors.New("no http(s) links found in the text")
		}
		j.snap = GrabSnapshot{Mode: "text", Status: GrabScanning}
	}
	b := make([]byte, 8)
	rand.Read(b)
	j.snap.ID = "g" + hex.EncodeToString(b)

	parent := g.e.rootCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, grabBudget)
	j.cancel = cancel

	g.mu.Lock()
	g.pruneLocked()
	g.jobs[j.snap.ID] = j
	g.mu.Unlock()
	go j.run(ctx)
	return j.snap.ID, nil
}

// Get returns a job's current snapshot.
func (g *Grabber) Get(id string) (GrabSnapshot, bool) {
	g.mu.Lock()
	g.pruneLocked()
	j := g.jobs[id]
	g.mu.Unlock()
	if j == nil {
		return GrabSnapshot{}, false
	}
	return j.snapshot(), true
}

// Cancel stops a job and forgets it.
func (g *Grabber) Cancel(id string) bool {
	g.mu.Lock()
	j := g.jobs[id]
	delete(g.jobs, id)
	g.mu.Unlock()
	if j == nil {
		return false
	}
	j.cancel()
	return true
}

// pruneLocked forgets jobs that finished more than grabJobTTL ago, and the
// oldest finished ones when too many pile up.
func (g *Grabber) pruneLocked() {
	now := time.Now()
	var oldest string
	var oldestAt time.Time
	for id, j := range g.jobs {
		at := j.finishedAt()
		if at.IsZero() {
			continue
		}
		if now.Sub(at) > grabJobTTL {
			delete(g.jobs, id)
			continue
		}
		if oldest == "" || at.Before(oldestAt) {
			oldest, oldestAt = id, at
		}
	}
	if len(g.jobs) >= grabMaxJobs && oldest != "" {
		delete(g.jobs, oldest)
	}
}

// ---- the job ------------------------------------------------------------------------

type grabJob struct {
	e          *Engine
	req        GrabRequest
	ri         reqInfo
	pageURL    string   // page mode
	links      []string // text mode
	probeVideo func(context.Context, string, ytdlp.Opts) (*ytdlp.ProbeResult, error)
	cancel     context.CancelFunc

	q    grabQueue
	prod sync.WaitGroup // background producers (yt-dlp runs, frame reads)

	mu         sync.Mutex
	snap       GrabSnapshot // Items/Counts/Pending are built by snapshot()
	items      []*GrabItem
	seen       map[string]bool // grabKey of every address listed or ruled out
	nextID     int
	frames     int
	embeds     int
	ytRunning  int
	pageErr    error // why the page itself couldn't be read
	archiveHit bool  // archive.org's file list answered: skip its yt-dlp run
	doneAt     time.Time
}

// grabWork is one candidate waiting for its probe.
type grabWork struct {
	item     *GrabItem
	cand     grabCand
	depth    int  // 0 = found on the page, 1 = inside a frame / landing page
	keepPage bool // text mode: a pasted page link stays in the list (a video site's page)
}

func (j *grabJob) run(ctx context.Context) {
	defer j.cancel()
	go func() {
		<-ctx.Done()
		j.q.close()
	}()
	var workers sync.WaitGroup
	for i := 0; i < grabWorkers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				w, ok := j.q.pop()
				if !ok {
					return
				}
				j.probeItem(ctx, w)
				j.q.release()
			}
		}()
	}
	j.q.hold()
	if j.pageURL != "" {
		j.crawlPage(ctx)
	} else {
		j.probeLinks()
	}
	j.q.release()
	workers.Wait()
	j.prod.Wait()
	j.finish(ctx)
}

// spawn runs a producer (yt-dlp, a frame read) that may still add work.
func (j *grabJob) spawn(f func()) {
	j.prod.Add(1)
	j.q.hold()
	go func() {
		defer j.prod.Done()
		defer j.q.release()
		f()
	}()
}

// crawlPage: probe the address; a file/stream is "direct", a page is read.
func (j *grabJob) crawlPage(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, grabPageTimeout)
	res, perr := j.e.Inspect(pctx, j.pageURL, j.ri.Referer, j.ri.Headers, j.req.Title)
	cancel()
	if perr == nil && res.Kind != kindPage {
		j.mu.Lock()
		j.snap.Direct = true
		j.mu.Unlock()
		if it := j.addItem(grabCand{URL: res.URL, Source: grabSrcDirect, Linked: true}, j.ri.Referer); it != nil {
			j.applyInspect(it, grabCand{}, res)
		}
		return
	}
	j.setStatus(GrabScanning)
	if t := strings.TrimSpace(j.req.Title); t != "" {
		j.setTitles(t, "")
	}
	if id := archiveItemID(j.pageURL); id != "" {
		j.archiveFiles(ctx, id)
	}
	if j.probeVideo != nil && !j.archiveHit {
		j.spawn(func() { j.videoItems(ctx, j.pageURL, j.pageURL, j.ri, false) })
	} else if j.probeVideo == nil {
		j.mu.Lock()
		j.snap.Ytdlp = "none"
		j.mu.Unlock()
	}

	fctx, fcancel := context.WithTimeout(ctx, grabPageTimeout)
	doc, final, ferr := j.e.fetchPage(fctx, j.pageURL, j.ri)
	fcancel()
	if ferr != nil {
		if perr == nil {
			perr = ferr
		}
		j.mu.Lock()
		j.pageErr = perr
		j.mu.Unlock()
		return
	}
	scan := extractCandidates(final, doc)
	j.setTitles(scan.Title, scan.OGTitle)
	j.addCands(ctx, scan.Cands, final, 0, false)
}

// probeLinks: text mode — every pasted link is a candidate.
func (j *grabJob) probeLinks() {
	for _, l := range j.links {
		c := grabCand{URL: l, Source: grabSrcText, Linked: true}
		if it := j.addItem(c, j.ri.Referer); it != nil {
			j.q.push(grabWork{item: it, cand: c, keepPage: true}, false)
		}
	}
}

// addCands lists a page's candidates: embedded players go to yt-dlp, frames
// are read once, everything else is queued for a probe. A landing page (a
// "Download" button's target) contributes only its file links and media.
func (j *grabJob) addCands(ctx context.Context, cands []grabCand, foundOn string, depth int, landing bool) {
	for _, c := range cands {
		switch {
		case landing && !c.Linked && c.Kind != "video" && c.Kind != "audio" && c.Kind != "stream":
		case c.Embed:
			j.addEmbed(ctx, c, foundOn)
		case c.Frame:
			if depth > 0 || !j.claim(grabKey(c.URL), &j.frames, grabMaxFrames) {
				continue
			}
			c := c
			j.spawn(func() { j.readFrame(ctx, c, foundOn) })
		default:
			if it := j.addItem(c, foundOn); it != nil {
				j.q.push(grabWork{item: it, cand: c, depth: depth}, c.Kind == "image" || grabImageExts[grabURLExt(c.URL)])
			}
		}
	}
}

// claim reserves one of a limited number of reads (frames, embeds, landing
// pages) under a dedup key not seen before.
func (j *grabJob) claim(key string, counter *int, max int) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.seen[key] || *counter >= max {
		return false
	}
	j.seen[key] = true
	*counter++
	return true
}

// addEmbed hands a known video site's player to yt-dlp. Without yt-dlp the
// player is still listed, so the user sees there is a video.
func (j *grabJob) addEmbed(ctx context.Context, c grabCand, foundOn string) {
	if j.probeVideo == nil {
		if it := j.addItem(c, foundOn); it != nil {
			j.mu.Lock()
			it.Kind, it.Category, it.Status = "video", "Video", "skipped"
			it.Error = "yt-dlp is not installed"
			j.mu.Unlock()
		}
		return
	}
	if !j.claim(grabKey(c.URL), &j.embeds, grabMaxEmbeds) {
		return
	}
	j.spawn(func() { j.videoItems(ctx, c.URL, foundOn, j.riFor(c.URL, foundOn), true) })
}

// readFrame reads an iframe one level deep (the parent page as Referer).
func (j *grabJob) readFrame(ctx context.Context, c grabCand, parent string) {
	fctx, cancel := context.WithTimeout(ctx, grabPageTimeout)
	doc, final, err := j.e.fetchPage(fctx, c.URL, j.riFor(c.URL, parent))
	cancel()
	if errors.Is(err, errGrabNotPage) {
		// The "frame" is a file (<embed src=clip.mp4>, <object data=x.pdf>).
		c.Frame = false
		j.mu.Lock()
		delete(j.seen, grabKey(c.URL))
		j.mu.Unlock()
		if it := j.addItem(c, parent); it != nil {
			j.q.push(grabWork{item: it, cand: c, depth: 1}, false)
		}
		return
	}
	if err != nil {
		return
	}
	scan := extractCandidates(final, doc)
	j.addCands(ctx, scan.Cands, final, 1, false)
}

// probeItem fills one candidate from Inspect. Pages are dropped — unless the
// candidate was a "Download" button, whose landing page is read for the file.
func (j *grabJob) probeItem(ctx context.Context, w grabWork) {
	it, c := w.item, w.cand
	if ctx.Err() != nil {
		j.setItemStatus(it, "skipped", "not checked (time limit)")
		return
	}
	j.setItemStatus(it, "probing", "")
	ri := j.riFor(c.URL, it.FoundOn)
	title := ""
	if c.nameQ >= 1 {
		title = c.Name
	}
	if k := grabFirst(c.Kind, grabMediaKind(c.URL)); k == "stream" || k == "video" || k == "audio" {
		// Streams and "videoplayback"-style media are named after the page;
		// the title only replaces names that are generic.
		title = grabFirst(title, j.titleHint())
	}
	ictx, cancel := context.WithTimeout(ctx, grabItemTimeout)
	res, err := j.e.Inspect(ictx, c.URL, ri.Referer, ri.Headers, title)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			j.setItemStatus(it, "skipped", "not checked (time limit)")
		} else {
			j.setItemStatus(it, "error", grabShortErr(err))
		}
		return
	}
	if res.Kind == kindPage {
		switch {
		case w.keepPage:
			j.applyInspect(it, c, res)
			return
		case c.Follow && w.depth == 0 && j.claim("landing "+grabKey(res.URL), &j.frames, grabMaxFrames):
			j.removeItem(it)
			fctx, fcancel := context.WithTimeout(ctx, grabPageTimeout)
			doc, final, ferr := j.e.fetchPage(fctx, res.URL, ri)
			fcancel()
			if ferr == nil {
				j.addCands(ctx, extractCandidates(final, doc).Cands, final, 1, true)
			}
			return
		}
		j.removeItem(it)
		return
	}
	j.applyInspect(it, c, res)
}

// applyInspect copies a probe's answer into an item.
func (j *grabJob) applyInspect(it *GrabItem, c grabCand, res InspectResult) {
	name := res.FileName
	stem, ext := splitExt(name)
	if c.nameQ >= 2 && isGenericStem(stem) {
		if t := SanitizeFileName(cleanTitle(c.Name)); t != "" {
			name = t
			if ext != "" && !strings.HasSuffix(strings.ToLower(t), "."+ext) {
				name += "." + ext
			}
		}
	}
	kind := grabKindOf(res.Kind, ext, res.ContentType, c.Kind)
	j.mu.Lock()
	defer j.mu.Unlock()
	it.URL = res.URL
	it.Name, it.Ext, it.Kind = name, ext, kind
	it.Category = grabCategory(ext, kind)
	it.Size, it.SizeEstimated = res.Size, res.SizeEstimated
	it.ContentType = res.ContentType
	it.Duration, it.Live = res.Duration, res.Live
	it.Status, it.Error = "ok", ""
	switch res.Kind {
	case "drm":
		it.Status, it.Error = "error", "DRM-protected stream"
	case kindPage:
		it.Name, it.Ext = strings.TrimSuffix(stem, "."), ""
		it.Kind, it.Category = "page", "General"
		if isGrabVideoHost(hostOf(res.URL)) {
			it.Kind, it.Category = "video", "Video" // a pasted video-site link: the download runs yt-dlp
		}
	}
	it.Small = grabIsSmall(it)
}

func grabIsSmall(it *GrabItem) bool {
	if it.Kind != "image" {
		return false
	}
	return (it.Size >= 0 && it.Size < grabSmallBytes) ||
		(it.Width > 0 && it.Width < grabSmallPixels) || (it.Height > 0 && it.Height < grabSmallPixels)
}

// finish settles the job: unchecked items are marked skipped, the main video
// is recommended when nothing else chose one, and an empty result from an
// unreadable page becomes an error.
func (j *grabJob) finish(ctx context.Context) {
	j.mu.Lock()
	defer j.mu.Unlock()
	late := ctx.Err() != nil
	var best *GrabItem
	hasRec := false
	for _, it := range j.items {
		if it.Status == "pending" || it.Status == "probing" {
			it.Status = "skipped"
			if late {
				it.Error = "not checked (time limit)"
			}
		}
		hasRec = hasRec || it.Recommended
		if (it.Kind == "video" || it.Kind == "stream") && it.Status == "ok" && (best == nil || it.Size > best.Size) {
			best = it
		}
	}
	if !hasRec && best != nil && !j.snap.Direct {
		best.Recommended = true
	}
	j.snap.Status = GrabDone
	if len(j.items) == 0 && j.pageErr != nil {
		j.snap.Status, j.snap.Error = GrabError, j.pageErr.Error()
	}
	j.doneAt = time.Now()
}

func (j *grabJob) finishedAt() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.doneAt
}

func (j *grabJob) snapshot() GrabSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.snap
	s.Items = make([]GrabItem, 0, len(j.items))
	s.Counts = map[string]int{"video": 0, "audio": 0, "image": 0, "archive": 0, "document": 0,
		"program": 0, "stream": 0, "other": 0, "small": 0}
	for _, it := range j.items {
		s.Items = append(s.Items, *it)
		if it.Status == "pending" || it.Status == "probing" {
			s.Pending++
		}
		if it.Small {
			s.Counts["small"]++
			continue
		}
		switch it.Kind {
		case "video", "audio", "image", "archive", "document", "program", "stream":
			s.Counts[it.Kind]++
		default:
			s.Counts["other"]++
		}
	}
	return s
}

// ---- items ----------------------------------------------------------------------------

// addItem lists a candidate as a pending row; nil when it's a duplicate or
// the list is full.
func (j *grabJob) addItem(c grabCand, foundOn string) *GrabItem {
	key := grabKey(c.URL)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.seen[key] || len(j.items) >= grabMaxItems {
		return nil
	}
	j.seen[key] = true
	return j.appendLocked(grabItemFor(c, foundOn))
}

func (j *grabJob) appendLocked(it *GrabItem) *GrabItem {
	j.nextID++
	it.ID = strconv.Itoa(j.nextID)
	j.items = append(j.items, it)
	return it
}

// grabItemFor is a candidate's row before its probe: a name and type guessed
// from the address and whatever the page said.
func grabItemFor(c grabCand, foundOn string) *GrabItem {
	name := ""
	if c.nameQ >= 2 {
		name = SanitizeFileName(c.Name)
	}
	if name == "" {
		name = SanitizeFileName(pctDecodeName(pathName(c.URL)))
	}
	if name == "" {
		name = hostOf(c.URL)
	}
	ext := grabURLExt(c.URL)
	kind := grabKindOf("", ext, "", c.Kind)
	it := &GrabItem{URL: c.URL, Name: name, Ext: ext, Kind: kind, Category: grabCategory(ext, kind), Size: -1,
		Width: c.Width, Height: c.Height, Duration: c.Duration, Thumb: c.Thumb,
		Source: c.Source, FoundOn: foundOn, Status: "pending"}
	it.Small = it.Width > 0 && it.Width < grabSmallPixels || it.Height > 0 && it.Height < grabSmallPixels
	it.Small = it.Small && kind == "image"
	return it
}

func (j *grabJob) removeItem(it *GrabItem) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, x := range j.items {
		if x == it {
			j.items = append(j.items[:i], j.items[i+1:]...)
			return
		}
	}
}

func (j *grabJob) setItemStatus(it *GrabItem, status, msg string) {
	j.mu.Lock()
	it.Status, it.Error = status, msg
	j.mu.Unlock()
}

func (j *grabJob) setStatus(s string) {
	j.mu.Lock()
	j.snap.Status = s
	j.mu.Unlock()
}

// setTitles records the page's title (the first one found wins).
func (j *grabJob) setTitles(title, ogTitle string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.snap.PageTitle == "" {
		j.snap.PageTitle = grabFirst(ogTitle, title)
	}
	if j.snap.Title == "" {
		j.snap.Title = grabFirst(cleanTitleFor(ogTitle, j.pageURL), cleanTitleFor(title, j.pageURL))
	}
}

func (j *grabJob) titleHint() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return grabFirst(j.snap.Title, j.snap.PageTitle)
}

// riFor is the request context for one address: explicit cookies and
// credentials the caller gave for the page are sent only to the page's own
// site (the shared cookie jar still adds each site's own login cookies).
func (j *grabJob) riFor(rawURL, referer string) reqInfo {
	ri := reqInfo{Referer: referer, Headers: j.ri.Headers}
	if referer == "" {
		ri.Referer = j.ri.Referer
	}
	pu, err1 := url.Parse(rawURL)
	base := grabFirst(j.pageURL, j.ri.Referer) // text mode: the page the links came from
	if base == "" {
		return ri
	}
	bu, err2 := url.Parse(base)
	if err1 != nil || err2 != nil || grabSameSite(pu, bu) {
		return ri
	}
	h := map[string]string{}
	for k, v := range j.ri.Headers {
		switch strings.ToLower(k) {
		case "cookie", "authorization":
		default:
			h[k] = v
		}
	}
	ri.Headers = CleanHeaders(h)
	return ri
}

// ---- yt-dlp ---------------------------------------------------------------------------

// grabProbe mirrors ytdlp.ProbeResult's JSON, including the multi-entry
// fields (entries, recommended, hasVideo, directFile …); reading it through
// JSON keeps the grabber working with a yt-dlp package that has only some.
type grabProbe struct {
	Title       string       `json:"title"`
	Options     []GrabOption `json:"options"`
	Entries     []grabEntry  `json:"entries"`
	Recommended int          `json:"recommended"`
	Duration    float64      `json:"duration"`
	IsLive      bool         `json:"isLive"`
	HasVideo    *bool        `json:"hasVideo"`
	Thumbnail   string       `json:"thumbnail"`
	DirectFile  string       `json:"directFile"`
}

type grabEntry struct {
	Index         int          `json:"index"`
	Title         string       `json:"title"`
	URL           string       `json:"url"`
	Duration      float64      `json:"duration"`
	Thumbnail     string       `json:"thumbnail"`
	Ext           string       `json:"ext"`
	HasVideo      *bool        `json:"hasVideo"`
	Size          int64        `json:"size"`
	SizeEstimated bool         `json:"sizeEstimated"`
	Options       []GrabOption `json:"options"`
}

func decodeGrabProbe(r *ytdlp.ProbeResult) grabProbe {
	var p grabProbe
	if b, err := json.Marshal(r); err == nil {
		json.Unmarshal(b, &p)
	}
	return p
}

// recommendedEntry finds the entry yt-dlp recommends (Recommended is its
// 1-based Index): by index, else the one entry whose options are the
// top-level ones, else the first.
func (p grabProbe) recommendedEntry() int {
	if len(p.Entries) == 0 {
		return -1
	}
	for i, en := range p.Entries {
		if p.Recommended > 0 && en.Index == p.Recommended {
			return i
		}
	}
	match := -1
	for i, en := range p.Entries {
		if len(p.Options) > 0 && sameGrabOptions(en.Options, p.Options) {
			if match >= 0 {
				return 0 // ambiguous
			}
			match = i
		}
	}
	if match >= 0 {
		return match
	}
	return 0
}

func grabHasVideoOption(opts []GrabOption) bool {
	for _, o := range opts {
		if !o.Audio {
			return true
		}
	}
	return false
}

func sameGrabOptions(a, b []GrabOption) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Selector != b[i].Selector || a[i].Size != b[i].Size || a[i].Audio != b[i].Audio {
			return false
		}
	}
	return true
}

// videoItems asks yt-dlp about a page (or an embedded player) and lists what
// it found: every entry of a multi-video page, or one video plus its
// audio-only variant.
func (j *grabJob) videoItems(ctx context.Context, target, foundOn string, ri reqInfo, embed bool) {
	j.mu.Lock()
	j.ytRunning++
	j.snap.Ytdlp = "running"
	j.mu.Unlock()
	vctx, cancel := context.WithTimeout(ctx, grabVideoWait)
	res, err := j.probeVideo(vctx, target, ytdlp.Opts{UserAgent: j.e.userAgent(), Referer: ri.Referer, Headers: ri.Headers})
	cancel()
	j.mu.Lock()
	j.ytRunning--
	if j.ytRunning == 0 {
		j.snap.Ytdlp = "done"
	}
	if err != nil && !embed {
		j.snap.YtdlpError = grabShortErr(err)
	}
	j.mu.Unlock()
	if err != nil || res == nil {
		return
	}
	j.addVideoResult(decodeGrabProbe(res), target, foundOn)
}

func (j *grabJob) addVideoResult(p grabProbe, target, foundOn string) {
	if p.DirectFile != "" {
		// yt-dlp's "video" is a plain file: a native download is better.
		c := grabCand{URL: p.DirectFile, Source: grabSrcYtdlp, Name: p.Title, nameQ: 2, Linked: true}
		if it := j.addItem(c, foundOn); it != nil {
			j.q.push(grabWork{item: it, cand: c}, false)
		} else {
			j.markRecommended(p.DirectFile)
		}
		return
	}
	title := cleanTitleFor(p.Title, target)
	if title == "" {
		title = j.titleHint()
	}
	if len(p.Entries) > 1 {
		rec := p.recommendedEntry()
		for i, en := range p.Entries {
			if en.URL != "" && grabMediaKind(en.URL) != "stream" && j.known(en.URL) {
				if i == rec {
					j.markRecommended(en.URL)
				}
				continue // the page's own <video> already lists this file
			}
			opts := en.Options
			if len(opts) == 0 && i == rec {
				opts = p.Options
			}
			et := grabFirst(cleanTitleFor(en.Title, target), title)
			idx := en.Index
			if idx <= 0 {
				idx = i + 1
			}
			hasVideo := en.HasVideo == nil || *en.HasVideo || grabHasVideoOption(opts)
			it := j.videoItem(target, foundOn, et, en.Ext, opts, hasVideo, idx)
			j.mu.Lock()
			if en.Size > 0 {
				it.Size, it.SizeEstimated = en.Size, true
			}
			it.Duration, it.Thumb, it.Recommended = en.Duration, en.Thumbnail, i == rec
			j.appendLocked(it)
			j.mu.Unlock()
		}
		return
	}
	opts := p.Options
	idx := 0
	if len(p.Entries) == 1 {
		if len(opts) == 0 {
			opts = p.Entries[0].Options
		}
		p.Title = grabFirst(p.Title, p.Entries[0].Title)
	}
	var vopts, aopts []GrabOption
	for _, o := range opts {
		if o.Audio {
			aopts = append(aopts, o)
		} else {
			vopts = append(vopts, o)
		}
	}
	// A video quality means video whatever the flag says; with no options at
	// all, trust hasVideo (an audio-only page: podcast, SoundCloud).
	hasVideo := len(vopts) > 0 || (len(opts) == 0 && (p.HasVideo == nil || *p.HasVideo))
	j.mu.Lock()
	defer j.mu.Unlock()
	if hasVideo {
		it := j.videoItem(target, foundOn, title, "", vopts, true, idx)
		it.Duration, it.Live, it.Thumb, it.Recommended = p.Duration, p.IsLive, p.Thumbnail, true
		j.appendLocked(it)
	}
	if len(aopts) > 0 {
		it := j.videoItem(target, foundOn, title, "", aopts, false, idx)
		it.Duration, it.Thumb = p.Duration, p.Thumbnail
		it.Recommended = !hasVideo
		j.appendLocked(it)
	}
}

// videoItem builds a yt-dlp row (not yet appended).
func (j *grabJob) videoItem(target, foundOn, title, ext string, opts []GrabOption, hasVideo bool, playlistItem int) *GrabItem {
	kind, cat := "video", "Video"
	if !hasVideo {
		kind, cat = "audio", "Music"
	}
	if ext == "" && len(opts) > 0 {
		ext = opts[0].Ext
	}
	if ext == "" {
		ext = "mp4"
		if !hasVideo {
			ext = "mp3"
		}
	}
	size := int64(-1)
	for _, o := range opts {
		if o.Size > size {
			size = o.Size
		}
	}
	if size == 0 {
		size = -1
	}
	stem := SanitizeFileName(title)
	if stem == "" {
		stem = streamBaseName("", target, foundOn)
	}
	return &GrabItem{URL: target, Name: stem + "." + ext, Ext: ext, Kind: kind, Category: cat,
		Size: size, SizeEstimated: size > 0, Source: grabSrcYtdlp, FoundOn: foundOn, Status: "ok",
		Video: &GrabVideo{Title: title, Options: opts, PlaylistItem: playlistItem}, PlaylistItem: playlistItem}
}

// known reports an address already listed.
func (j *grabJob) known(rawURL string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seen[grabKey(rawURL)]
}

func (j *grabJob) markRecommended(rawURL string) {
	key := grabKey(rawURL)
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, it := range j.items {
		if grabKey(it.URL) == key {
			it.Recommended = true
		}
	}
}

// ---- archive.org --------------------------------------------------------------------------

// archiveItemID returns the item id of an archive.org /details/<id> page.
func archiveItemID(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if h != "archive.org" || !strings.HasPrefix(u.Path, "/details/") {
		return ""
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/details/"), "/")
	return id
}

// archiveFiles lists an archive.org item's files from its metadata API: the
// complete list with exact sizes, which the page's HTML only partly shows.
func (j *grabJob) archiveFiles(ctx context.Context, id string) {
	actx, cancel := context.WithTimeout(ctx, grabPageTimeout)
	defer cancel()
	api := "https://archive.org/metadata/" + url.PathEscape(id)
	req, err := j.e.newRequest(actx, http.MethodGet, api, reqInfo{Referer: j.pageURL})
	if err != nil {
		return
	}
	resp, err := j.e.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var meta struct {
		Files []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
			Size   string `json:"size"`
			Length string `json:"length"`
			Width  string `json:"width"`
			Height string `json:"height"`
		} `json:"files"`
		Metadata struct {
			Title any `json:"title"`
		} `json:"metadata"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, grabPageMax)).Decode(&meta) != nil {
		return
	}
	if t, ok := meta.Metadata.Title.(string); ok {
		j.setTitles(t, "")
	}
	for _, f := range meta.Files {
		n := f.Name
		low := strings.ToLower(n)
		if n == "" || f.Source == "metadata" || strings.Contains(low, ".thumbs/") || strings.HasPrefix(low, "__ia_thumb") ||
			strings.HasSuffix(low, "_meta.xml") || strings.HasSuffix(low, "_files.xml") || strings.HasSuffix(low, "_meta.sqlite") ||
			strings.HasSuffix(low, "_reviews.xml") {
			continue
		}
		segs := strings.Split(n, "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		c := grabCand{URL: "https://archive.org/download/" + url.PathEscape(id) + "/" + strings.Join(segs, "/"),
			Source: grabSrcLink, Linked: true, Width: grabAtoi(f.Width), Height: grabAtoi(f.Height)}
		it := j.addItem(c, j.pageURL)
		if it == nil {
			continue
		}
		size, err := strconv.ParseInt(f.Size, 10, 64)
		j.mu.Lock()
		j.archiveHit = true
		it.Name = SanitizeFileName(path.Base(n))
		if err == nil {
			it.Size = size
			it.Status = "ok"
			it.Duration = grabArchiveLength(f.Length)
			it.Small = grabIsSmall(it)
		}
		j.mu.Unlock()
		if err != nil {
			j.q.push(grabWork{item: it, cand: c}, it.Kind == "image")
		}
	}
}

// grabArchiveLength reads archive.org's "length": seconds or [h:]mm:ss.
func grabArchiveLength(s string) float64 {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	var total float64
	for _, p := range strings.Split(s, ":") {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0
		}
		total = total*60 + v
	}
	return total
}

// ---- fetching pages -------------------------------------------------------------------------

// errGrabNotPage: the address answered with a file, not HTML.
var errGrabNotPage = errors.New("not a web page")

// fetchPage GETs a page's HTML with the download's browser context (at most
// grabPageMax bytes, gzip decoded) and returns it with the final URL after
// redirects — the base its relative links resolve against.
func (e *Engine) fetchPage(ctx context.Context, rawURL string, ri reqInfo) (doc, finalURL string, err error) {
	req, err := e.newRequest(ctx, http.MethodGet, rawURL, ri)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip") // only what we can decode ourselves
	resp, err := e.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", "", &probeStatusError{code: resp.StatusCode, status: resp.Status, challenge: isChallenge(resp)}
	}
	var body io.Reader = resp.Body
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip") {
		gz, gerr := gzip.NewReader(resp.Body)
		if gerr != nil {
			return "", "", gerr
		}
		defer gz.Close()
		body = gz
	}
	b, err := io.ReadAll(io.LimitReader(body, grabPageMax))
	if err != nil && len(b) == 0 {
		return "", "", err
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	markup := strings.Contains(ct, "html") || (strings.Contains(ct, "xml") && !strings.Contains(ct, "dash"))
	if !markup && !(textualSample(b) && looksHTML(leadingText(b))) {
		return "", "", errGrabNotPage
	}
	finalURL = rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return string(b), finalURL, nil
}

// ---- classification ---------------------------------------------------------------------------

// grabKindOf types an item from its probe kind, extension, content type and
// what the page said it was.
func grabKindOf(probeKind, ext, contentType, hint string) string {
	switch probeKind {
	case kindHLS, kindDASH, "drm":
		return "stream"
	}
	switch {
	case ext == "m3u8" || ext == "mpd":
		return "stream"
	case grabVideoExts[ext]:
		return "video"
	case grabAudioExts[ext]:
		return "audio"
	case grabImageExts[ext]:
		return "image"
	case grabArchiveExts[ext]:
		return "archive"
	case grabDocExts[ext]:
		return "document"
	case grabProgramExts[ext]:
		return "program"
	case grabSubtitleExts[ext]:
		return "subtitle"
	case ext == "torrent":
		return "torrent"
	}
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.Contains(ct, "mpegurl") || strings.Contains(ct, "dash+xml"):
		return "stream"
	}
	switch hint {
	case "video", "audio", "image", "subtitle", "stream":
		return hint
	}
	return "other"
}

// grabCategory is the D BOX category (download folder) of an item.
func grabCategory(ext, kind string) string {
	if c := ExtCategory[ext]; c != "" {
		return c
	}
	switch kind {
	case "video", "stream":
		return "Video"
	case "audio":
		return "Music"
	case "image":
		return "Images"
	case "archive":
		return "Compressed"
	case "document":
		return "Documents"
	case "program":
		return "Programs"
	}
	return "General"
}

// grabTextLinks collects the links of a text-mode job, in order, once each.
func grabTextLinks(text string, urls []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range append(append([]string{}, urls...), extractTextLinks(text)...) {
		u, err := parseDownloadURL(l)
		if err != nil {
			continue
		}
		k := grabKey(u.String())
		if seen[k] || len(out) >= grabMaxItems {
			continue
		}
		seen[k] = true
		out = append(out, u.String())
	}
	return out
}

// grabShortErr keeps an error message short enough for a list row.
func grabShortErr(err error) string {
	msg := err.Error()
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	return msg
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// ---- work queue -------------------------------------------------------------------------------

// grabQueue feeds the probe workers: media and links before images, and
// "busy" counts queued items, items being probed and running producers, so
// workers stop only when nothing can add more work. close (the job's context
// ended) drops whatever is still queued.
type grabQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	hi, lo []grabWork
	busy   int
	closed bool
}

func (q *grabQueue) hold() {
	q.mu.Lock()
	q.busy++
	q.mu.Unlock()
}

func (q *grabQueue) release() {
	q.mu.Lock()
	q.busy--
	if q.busy <= 0 {
		q.cond.Broadcast()
	}
	q.mu.Unlock()
}

func (q *grabQueue) push(w grabWork, low bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return // the job's time is up: the item stays pending and finish() marks it
	}
	q.busy++
	if low {
		q.lo = append(q.lo, w)
	} else {
		q.hi = append(q.hi, w)
	}
	q.cond.Signal()
}

func (q *grabQueue) pop() (grabWork, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		switch {
		case q.closed:
			return grabWork{}, false
		case len(q.hi) > 0:
			w := q.hi[0]
			q.hi = q.hi[1:]
			return w, true
		case len(q.lo) > 0:
			w := q.lo[0]
			q.lo = q.lo[1:]
			return w, true
		case q.busy <= 0:
			return grabWork{}, false
		}
		q.cond.Wait()
	}
}

func (q *grabQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.busy -= len(q.hi) + len(q.lo)
	q.hi, q.lo = nil, nil
	q.cond.Broadcast()
	q.mu.Unlock()
}
