package ytdlp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"myidm/internal/procutil"
)

// PlaylistEntry is one video in a playlist (resolved lazily at download time).
type PlaylistEntry struct {
	URL       string  `json:"url"`
	Title     string  `json:"title"`
	Index     int     `json:"index,omitempty"` // 1-based position in the playlist
	Duration  float64 `json:"duration,omitempty"`
	Thumbnail string  `json:"thumbnail,omitempty"`
	Size      int64   `json:"size,omitempty"` // yt-dlp's estimate, when the flat listing has one
}

// PlaylistInfo is a playlist's title + its videos, enumerated without download.
type PlaylistInfo struct {
	Title   string          `json:"title"`
	Entries []PlaylistEntry `json:"entries"`
	Mix     bool            `json:"mix"` // a YouTube Mix/radio (endless) — the count was capped
}

// mixCap limits how many videos we pull from a YouTube Mix. Mixes (list=RD…) are
// endless auto-radios — yt-dlp will otherwise follow them for hundreds of
// videos (a different count every run) — so we take only the leading queue,
// which is about the size YouTube shows in the mix panel (~25).
const mixCap = 25

// isMixURL reports whether a URL is a YouTube Mix/radio (its list= id starts
// with RD), which has no fixed size and must be capped.
func isMixURL(u string) bool {
	return strings.Contains(u, "?list=RD") || strings.Contains(u, "&list=RD")
}

type flatEntry struct {
	ID         string  `json:"id"`
	URL        string  `json:"url"`
	WebpageURL string  `json:"webpage_url"`
	IEKey      string  `json:"ie_key"`
	Title      string  `json:"title"`
	Duration   float64 `json:"duration"`
	Size       jsonInt `json:"filesize_approx"`
	Thumbnail  string  `json:"thumbnail"`
	Thumbnails []struct {
		URL string `json:"url"`
	} `json:"thumbnails"`
}

type flatPlaylist struct {
	Type    string       `json:"_type"`
	Title   string       `json:"title"`
	Entries []*flatEntry `json:"entries"`
}

// ProbePlaylist lists a playlist's videos WITHOUT downloading them, using
// `yt-dlp --flat-playlist -J` (fast — it doesn't fetch each video's formats).
// Returns nil (not an error) when the URL isn't a playlist, so callers can fall
// back to the single-video path. Works for any site yt-dlp lists (Bilibili
// parts, Dailymotion playlists …), not only YouTube.
func ProbePlaylist(ctx context.Context, rawURL string, o Opts) (*PlaylistInfo, error) {
	bin := Locate()
	if bin == "" {
		return nil, ErrNotInstalled
	}
	args := []string{"--flat-playlist", "-J", "--no-warnings"}
	mix := isMixURL(rawURL)
	if mix {
		args = append(args, "--playlist-end", fmt.Sprint(mixCap)) // endless radio — take the leading queue only
	}
	args = append(args, requestArgs(rawURL, o)...)
	args = append(args, rawURL)
	cmd := exec.CommandContext(ctx, bin, args...)
	procutil.Hidden(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp playlist probe failed: %s", FriendlyError(ytdlpErr(err)))
	}
	var fp flatPlaylist
	if err := json.Unmarshal(out, &fp); err != nil {
		return nil, fmt.Errorf("parse yt-dlp playlist: %w", err)
	}
	pl := parseFlatPlaylist(&fp, rawURL)
	if pl != nil {
		pl.Mix = mix
	}
	return pl, nil
}

func parseFlatPlaylist(fp *flatPlaylist, pageURL string) *PlaylistInfo {
	if (fp.Type != "playlist" && fp.Type != "multi_video") || len(fp.Entries) == 0 {
		return nil // a single video, not a playlist
	}
	pl := &PlaylistInfo{Title: strings.TrimSpace(fp.Title)}
	youtubePage := isYouTubeURL(pageURL)
	seen := map[string]bool{}
	for i, e := range fp.Entries {
		if e == nil {
			continue
		}
		u := strings.TrimSpace(e.URL)
		if !strings.HasPrefix(u, "http") {
			u = strings.TrimSpace(e.WebpageURL)
		}
		if !strings.HasPrefix(u, "http") {
			// A flat YouTube entry is often a bare video id; other sites' bare ids
			// can't be turned into an address safely.
			id := e.ID
			if id == "" {
				id = strings.TrimSpace(e.URL)
			}
			if id == "" || !(strings.EqualFold(e.IEKey, "Youtube") || (e.IEKey == "" && youtubePage)) {
				continue
			}
			u = "https://www.youtube.com/watch?v=" + id
		}
		key := u
		if e.ID != "" { // dedup on the video ID (radio/mix playlists can repeat videos)
			key = e.IEKey + "|" + e.ID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		title := strings.TrimSpace(e.Title)
		if title == "" {
			// Flat listings of many sites (Bilibili parts, Dailymotion) carry no
			// titles; number the entries after the playlist so their files differ.
			if pl.Title != "" {
				title = fmt.Sprintf("%s - part %d", pl.Title, i+1)
			} else {
				title = fmt.Sprintf("video %d", i+1)
			}
		}
		thumb := e.Thumbnail
		if thumb == "" && len(e.Thumbnails) > 0 {
			thumb = e.Thumbnails[len(e.Thumbnails)-1].URL // yt-dlp lists them smallest first
		}
		pl.Entries = append(pl.Entries, PlaylistEntry{URL: u, Title: title, Index: i + 1,
			Duration: e.Duration, Thumbnail: thumb, Size: int64(e.Size)})
	}
	if len(pl.Entries) == 0 {
		return nil
	}
	return pl
}

// isYouTubeURL reports a YouTube page (watch, shorts, playlist, youtu.be).
func isYouTubeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return h == "youtu.be" || h == "youtube.com" || strings.HasSuffix(h, ".youtube.com") ||
		h == "youtube-nocookie.com" || strings.HasSuffix(h, ".youtube-nocookie.com")
}

// IsYouTubeURL is isYouTubeURL for the engine: only YouTube pages resolve to
// googlevideo URLs the burst downloader speeds up.
func IsYouTubeURL(raw string) bool { return isYouTubeURL(raw) }
