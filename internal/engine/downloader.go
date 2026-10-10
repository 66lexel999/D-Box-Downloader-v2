package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const copyBufSize = 256 << 10

var errServerIgnoredRange = errors.New("server ignored range request (file may have changed on the server)")

// planSegments splits size bytes into up to want ranges, none smaller than
// minSeg. Non-ranged or unknown-size downloads get a single open segment.
func planSegments(size int64, ranged bool, want int, minSeg int64) []*Segment {
	if !ranged || size <= 0 {
		end := size - 1 // -1 when size unknown
		if size <= 0 {
			end = -1
		}
		return []*Segment{{Start: 0, End: end}}
	}
	n := int64(want)
	if n < 1 {
		n = 1
	}
	if minSeg > 0 && size/minSeg < n {
		n = size / minSeg
		if n < 1 {
			n = 1
		}
	}
	base, rem := size/n, size%n
	segs := make([]*Segment, 0, n)
	var start int64
	for i := int64(0); i < n; i++ {
		length := base
		if i < rem {
			length++
		}
		segs = append(segs, &Segment{Start: start, End: start + length - 1})
		start += length
	}
	return segs
}

// downloadSegment drives one segment to completion, retrying transient errors
// with exponential backoff. The retry budget resets whenever bytes flow.
func (e *Engine) downloadSegment(ctx context.Context, t *Task, seg *Segment, f *os.File) error {
	backoff := time.Second
	retries := 0
	for {
		before := seg.Done()
		var err error
		if t.Ranged {
			err = e.streamRanged(ctx, t, seg, f)
		} else {
			err = e.streamWhole(ctx, t, seg, f)
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errServerIgnoredRange) || errors.Is(err, errFetchExpired) {
			return err // not transient: source changed under us / needs a fresh probe
		}
		if seg.Done() > before {
			retries, backoff = 0, time.Second
		}
		retries++
		if retries > e.cfg.MaxRetries {
			return fmt.Errorf("segment %d-%d: %w (after %d retries)", seg.Start, seg.End, err, e.cfg.MaxRetries)
		}
		// A rate-limited or busy server says when to come back; asking sooner
		// only earns another 429 and burns the retry budget.
		wait := backoff
		var se *segmentStatusError
		if errors.As(err, &se) && se.retryAfter > wait {
			wait = se.retryAfter
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if backoff *= 2; backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
}

// streamRanged resumes the segment at Start+Done and copies until End. A
// segment with End < 0 (server honors ranges but never said the total size)
// is fetched open-ended ("bytes=N-") until the server closes the stream.
func (e *Engine) streamRanged(ctx context.Context, t *Task, seg *Segment, f *os.File) error {
	cur := seg.Start + seg.Done()
	open := seg.End < 0
	if !open && cur > seg.End {
		return nil // already complete
	}

	req, err := e.newRequest(ctx, http.MethodGet, t.fetchURL(), t.reqInfo())
	if err != nil {
		return err
	}
	if open {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", cur))
	} else {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", cur, seg.End))
	}
	// If-Range makes the server fall back to 200 when the entity changed,
	// which we surface as errServerIgnoredRange instead of corrupting the file.
	// Only STRONG validators qualify: a weak ETag in If-Range always mismatches.
	if et := strongETag(t.ETag); et != "" {
		req.Header.Set("If-Range", et)
	} else if t.LastModified != "" {
		req.Header.Set("If-Range", t.LastModified)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if err := checkContentRange(resp, cur, t.Size); err != nil {
			return err
		}
	case http.StatusOK:
		// At offset 0 with no validator sent, a full-body reply is exactly the
		// bytes we asked for. With If-Range it means the file changed.
		if cur != 0 || req.Header.Get("If-Range") != "" {
			return errServerIgnoredRange
		}
	case http.StatusRequestedRangeNotSatisfiable:
		if open && cur > 0 {
			return nil // asked past the end of an unknown-size file: it's complete
		}
		return newSegmentStatusError(resp)
	default:
		return t.statusError(resp)
	}

	buf := make([]byte, copyBufSize)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			// Clamp anything past our segment end (defensive against sloppy servers).
			if !open {
				if over := cur + int64(n) - 1 - seg.End; over > 0 {
					n -= int(over)
				}
			}
			if n > 0 {
				if err := e.limiter.Take(ctx, n); err != nil {
					return err
				}
				if _, werr := f.WriteAt(buf[:n], cur); werr != nil {
					return werr
				}
				cur += int64(n)
				seg.SetDone(cur - seg.Start)
			}
			if !open && cur > seg.End {
				return nil
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				if !open && cur <= seg.End {
					return fmt.Errorf("connection closed early at byte %d of %d", cur, seg.End+1)
				}
				return nil
			}
			return rerr
		}
	}
}

// streamWhole handles servers without range support: one connection, written
// sequentially from offset 0. A retry or resume restarts from scratch.
func (e *Engine) streamWhole(ctx context.Context, t *Task, seg *Segment, f *os.File) error {
	seg.SetDone(0)
	if err := f.Truncate(0); err != nil {
		return err
	}

	req, err := e.newRequest(ctx, http.MethodGet, t.fetchURL(), t.reqInfo())
	if err != nil {
		return err
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return t.statusError(resp)
	}
	// A server that compresses despite "Accept-Encoding: identity" would leave
	// gzip bytes under the file's name: undo it, unless the file IS a .gz.
	body := io.Reader(resp.Body)
	if ce := resp.Header.Get("Content-Encoding"); !isIdentityEncoding(ce) && !keepsEncoding(nameExt(t.FileName)) {
		var ok bool
		if body, ok = decodeBody(resp.Body, ce); !ok {
			return fmt.Errorf("the server compressed the file with %q, which D BOX can't decode", ce)
		}
	}

	var cur int64
	buf := make([]byte, copyBufSize)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if err := e.limiter.Take(ctx, n); err != nil {
				return err
			}
			if _, werr := f.WriteAt(buf[:n], cur); werr != nil {
				return werr
			}
			cur += int64(n)
			seg.SetDone(cur)
		}
		if rerr != nil {
			if rerr == io.EOF {
				if t.Size > 0 && cur != t.Size {
					return fmt.Errorf("connection closed early at byte %d of %d", cur, t.Size)
				}
				return nil
			}
			return rerr
		}
	}
}

// fetchURL is the address segment requests go to: the one the probe resolved
// (a landing page's link, or a validator-less redirect's target) when set.
func (t *Task) fetchURL() string {
	if t.FetchURL != "" {
		return t.FetchURL
	}
	return t.URL
}

// statusError turns a failed segment answer into an error. When the task
// fetches from a resolved address, 403/404/410 mean that address expired and
// the original link must be probed again.
func (t *Task) statusError(resp *http.Response) error {
	if t.FetchURL != "" {
		switch resp.StatusCode {
		case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
			return fmt.Errorf("%w (%s)", errFetchExpired, resp.Status)
		}
	}
	return newSegmentStatusError(resp)
}
