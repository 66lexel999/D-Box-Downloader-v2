package engine

// Integrity: a download that "completes" with the wrong bytes is worse than one
// that fails. These helpers check that what the server sends is the file the
// probe saw — the right range of the right entity, decoded if the server
// compressed it anyway, named for what it really is — and that the finished
// file is not an error page wearing the file's name.

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// errFetchExpired: the address the probe resolved (a redirect's target, a
// landing page's link) stopped answering; the original link must be probed
// again for a fresh one.
var errFetchExpired = errors.New("the download address the server gave out has expired")

// segmentStatusError is a non-success answer to a segment request.
type segmentStatusError struct {
	code       int
	status     string
	retryAfter time.Duration // the server's Retry-After (0 = none)
}

func (e *segmentStatusError) Error() string { return "unexpected status " + e.status }

// newSegmentStatusError wraps resp's status, noting how long the server asked
// us to wait (rate limits and maintenance windows say so in Retry-After).
func newSegmentStatusError(resp *http.Response) error {
	se := &segmentStatusError{code: resp.StatusCode, status: resp.Status}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		se.retryAfter, _ = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	return se
}

// maxRetryAfter caps how long one segment waits on a server's Retry-After.
const maxRetryAfter = 2 * time.Minute

// parseRetryAfter reads a Retry-After value — seconds or an HTTP date — as a
// wait from now, capped at maxRetryAfter.
func parseRetryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	var d time.Duration
	if s, err := strconv.Atoi(h); err == nil {
		if s < 0 {
			return 0, false
		}
		d = time.Duration(s) * time.Second
	} else if at, err := http.ParseTime(h); err == nil {
		d = at.Sub(now)
		if d < 0 {
			d = 0
		}
	} else {
		return 0, false
	}
	return min(d, maxRetryAfter), true
}

// parseContentRange reads "bytes start-end/total" (total -1 for "*").
func parseContentRange(h string) (start, end, total int64, ok bool) {
	h = strings.TrimSpace(h)
	unit, rest, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(unit, "bytes") {
		return 0, 0, 0, false
	}
	rng, tot, found := strings.Cut(strings.TrimSpace(rest), "/")
	if !found {
		return 0, 0, 0, false
	}
	a, b, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, 0, false
	}
	var err error
	if start, err = strconv.ParseInt(strings.TrimSpace(a), 10, 64); err != nil || start < 0 {
		return 0, 0, 0, false
	}
	if end, err = strconv.ParseInt(strings.TrimSpace(b), 10, 64); err != nil || end < start {
		return 0, 0, 0, false
	}
	total = -1
	if tot = strings.TrimSpace(tot); tot != "*" {
		if total, err = strconv.ParseInt(tot, 10, 64); err != nil || total <= end {
			return 0, 0, 0, false
		}
	}
	return start, end, total, true
}

// checkContentRange verifies a 206 answer covers the bytes we asked for, of the
// file the probe measured. A CDN that always answers "bytes 0-…", or serves a
// different file for later ranges (a random-image redirector), would otherwise
// be stitched into a corrupt file without any error.
func checkContentRange(resp *http.Response, cur, size int64) error {
	start, _, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	switch {
	case !ok, start != cur:
		return errServerIgnoredRange
	case total >= 0 && size > 0 && total != size:
		return errServerIgnoredRange
	}
	return nil
}

// keepsEncoding reports a target that IS compressed data (a .gz served with
// "Content-Encoding: gzip" by a misconfigured server): its bytes are saved as
// sent, never decoded.
func keepsEncoding(ext string) bool {
	switch strings.ToLower(ext) {
	case "gz", "tgz", "svgz", "z", "taz":
		return true
	}
	return false
}

// decodeBody undoes a Content-Encoding the server applied although we asked
// for identity (bilibili and some PHP scripts gzip everything). ok is false
// for codings the standard library can't undo (br, zstd); the reader then
// yields the bytes as sent. A header that lies (says gzip, sends plain bytes)
// is caught by peeking at the stream's magic.
func decodeBody(r io.Reader, coding string) (io.Reader, bool) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(2)
	switch strings.ToLower(strings.TrimSpace(coding)) {
	case "", "identity", "none":
		return br, true
	case "gzip", "x-gzip":
		if len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
			if zr, err := gzip.NewReader(br); err == nil {
				return zr, true
			}
		}
		return br, true
	case "deflate":
		// "deflate" is meant to be zlib-wrapped; some servers send raw DEFLATE.
		if len(head) == 2 && head[0]&0x0f == 8 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
			if zr, err := zlib.NewReader(br); err == nil {
				return zr, true
			}
		}
		return flate.NewReader(br), true
	}
	return br, false
}

// zipContainers are formats stored as ZIP archives: "PK" bytes under these
// names are right.
var zipContainers = map[string]bool{
	"docx": true, "xlsx": true, "pptx": true, "docm": true, "xlsm": true, "pptm": true, "odt": true, "ods": true,
	"odp": true, "odg": true, "epub": true, "apk": true, "xapk": true, "apks": true, "aab": true, "jar": true,
	"war": true, "ear": true, "ipa": true, "xpi": true, "vsix": true, "nupkg": true, "whl": true, "appx": true,
	"appxbundle": true, "msix": true, "msixbundle": true, "cbz": true, "kmz": true, "3mf": true, "sketch": true,
	"zipx": true, "crx": true, "mcpack": true, "mcworld": true, "pk3": true, "pk4": true,
}

// sameContainer groups extensions that share one container format, so a name
// from the group is right for bytes of any member (an .m4a in an "isom" MP4
// box, a QuickTime .mov, a Matroska .mka).
var sameContainer = func() map[string]string {
	m := map[string]string{}
	for group, exts := range map[string]string{
		"isobmff":  "mp4 m4v mov m4a m4b m4p 3gp 3g2 f4v f4a heic heif avif qt",
		"matroska": "mkv webm mka weba mk3d",
		"ogg":      "ogg oga ogv opus spx",
		"mpegts":   "ts m2ts mts tsv",
		"tiff":     "tiff tif cr2 nef arw dng",
	} {
		for _, e := range strings.Fields(exts) {
			m[e] = group
		}
	}
	return m
}()

// reconcileExt corrects a name whose extension contradicts the file's magic
// bytes across content families — MP3 audio a browser suggested as
// "clip.mp4", a PNG named .exe — by swapping in the extension the bytes
// prove. Refinements of one container (.docx/.apk on ZIP bytes, .m4a on MP4,
// .mka on Matroska) and names with unknown extensions are left alone, as is
// anything text-like (an HTML answer is a refusal, not a rename).
func reconcileExt(name, magic string) string {
	stem, ext := splitExt(name)
	if ext == "" || magic == "" || strings.EqualFold(ext, magic) {
		return name
	}
	fam, mfam := extFamily(ext), extFamily(magic)
	switch {
	case fam == "" || mfam == "" || fam == mfam || mfam == famText || fam == famText:
		return name
	case magic == "zip" && zipContainers[strings.ToLower(ext)]:
		return name
	case sameContainer[strings.ToLower(ext)] != "" && sameContainer[strings.ToLower(ext)] == sameContainer[magic]:
		return name
	case (magic == "mp3" || magic == "aac" || magic == "ts") && fam != famVideo && fam != famAudio:
		// Frame-sync and 0x47 signatures are short: trust them only against
		// another media name.
		return name
	}
	return stem + "." + magic
}

// verifyPart checks a finished download before it gets its final name: an
// empty file, or an HTML/JSON/text body under a binary name (an error page the
// server sent with a 200 after the probe had seen the real file), fails the
// task instead of completing as junk.
func verifyPart(partPath, name, sourceURL string) error {
	f, err := os.Open(partPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	_, ext := splitExt(name)
	if fi.Size() == 0 {
		if extFamily(ext) == famText {
			return nil
		}
		return fmt.Errorf("the server sent an empty file")
	}
	head := make([]byte, sniffLen)
	n, _ := io.ReadFull(f, head)
	if binaryExt(ext) && errorBody(head[:n], fi.Size()) {
		return newLandingError(sourceURL, false)
	}
	return nil
}

// fetchURLFor picks the address segments download from (see Task.FetchURL):
// the landing page's link when the probe followed one, and a redirect's
// target when nothing (no ETag, no Last-Modified) would tell us that the
// redirector sent a later request to a different file. "" = the link itself.
func fetchURLFor(link string, pr probeResult) string {
	u := pr.URL
	if u == "" {
		u = link
	}
	if pr.FinalURL != "" && pr.FinalURL != u && pr.ETag == "" && pr.LastModified == "" {
		u = pr.FinalURL
	}
	if u == link {
		return ""
	}
	return u
}

// entitySnapshot is what a task knew about its file before a re-probe.
type entitySnapshot struct {
	size         int64
	ranged       bool
	etag, lastMo string
	progress     bool // bytes already on disk
}

// snapshotEntity records t's file identity; call with e.mu held.
func snapshotEntity(t *Task) entitySnapshot {
	s := entitySnapshot{size: t.Size, ranged: t.Ranged, etag: strongETag(t.ETag), lastMo: t.LastModified}
	for _, seg := range t.Segments {
		if seg.Done() > 0 {
			s.progress = true
			break
		}
	}
	return s
}

// sameFile reports that a re-probe found provably the same file, so the bytes
// on disk and the segment plan stay valid: same size, same ranged-ness, and a
// matching validator. Without a validator nothing proves it (a redirector may
// now serve another file), and the download restarts.
func (s entitySnapshot) sameFile(pr probeResult) bool {
	if !s.progress || s.size <= 0 || pr.Size != s.size || pr.Ranged != s.ranged {
		return false
	}
	return (s.etag != "" && s.etag == strongETag(pr.ETag)) || (s.lastMo != "" && s.lastMo == pr.LastModified)
}
