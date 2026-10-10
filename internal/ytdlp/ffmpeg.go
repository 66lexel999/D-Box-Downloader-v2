package ytdlp

// ffmpeg on demand. The installer ships yt-dlp but not ffmpeg (it's big), yet
// without ffmpeg most modern video can't be finished: YouTube-style sites serve
// video and audio as separate streams that must be merged, audio extraction to
// MP3 needs it, and HLS captures are MPEG-TS that should become MP4. Instead of
// failing (or silently saving a video with no sound), the first download that
// needs ffmpeg fetches a static build once into the app's tools folder.

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"myidm/internal/procutil"
)

// ffmpegWin64URL is yt-dlp's own static ffmpeg build (the one yt-dlp's docs
// recommend, with its patches), stable "latest" asset name.
const ffmpegWin64URL = "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip"

var (
	toolsMu  sync.RWMutex
	toolsDir string

	ffmpegMu sync.Mutex // one provisioning download at a time
)

// SetToolsDir registers the app-private folder where on-demand tools (ffmpeg)
// are installed. It is also searched when locating binaries.
func SetToolsDir(dir string) {
	toolsMu.Lock()
	toolsDir = dir
	toolsMu.Unlock()
}

func getToolsDir() string {
	toolsMu.RLock()
	defer toolsMu.RUnlock()
	return toolsDir
}

// ErrNoFFmpeg reports that ffmpeg is missing and couldn't be provisioned.
var ErrNoFFmpeg = errors.New("ffmpeg is not installed")

// EnsureFFmpeg returns ffmpeg's path, downloading a static build into the tools
// folder first if it isn't installed anywhere we look. progress (optional)
// receives bytes downloaded and the total (-1 if unknown).
func EnsureFFmpeg(ctx context.Context, progress func(done, total int64)) (string, error) {
	if p := ffmpegPath(); p != "" {
		return p, nil
	}
	ffmpegMu.Lock()
	defer ffmpegMu.Unlock()
	if p := ffmpegPath(); p != "" { // another task provisioned it while we waited
		return p, nil
	}
	dir := getToolsDir()
	if runtime.GOOS != "windows" || dir == "" {
		return "", ErrNoFFmpeg
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	zipPath := filepath.Join(dir, "ffmpeg-download.zip")
	defer os.Remove(zipPath)
	if err := fetchToFile(ctx, ffmpegWin64URL, zipPath, progress); err != nil {
		return "", fmt.Errorf("download ffmpeg: %w", err)
	}
	if err := extractBins(zipPath, dir, exeName("ffmpeg"), exeName("ffprobe")); err != nil {
		return "", err
	}
	if p := ffmpegPath(); p != "" {
		return p, nil
	}
	return "", ErrNoFFmpeg
}

// fetchToFile streams url to dest (via a .part file), reporting progress.
func fetchToFile(ctx context.Context, url, dest string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	var done int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				os.Remove(tmp)
				return werr
			}
			done += int64(n)
			if progress != nil {
				progress(done, resp.ContentLength)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// extractBins copies the named files (matched by base name, anywhere in the
// archive) from a zip into dir. archive/zip verifies each file's CRC.
func extractBins(zipPath, dir string, names ...string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer zr.Close()
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(n)] = true
	}
	found := 0
	for _, f := range zr.File {
		base := strings.ToLower(filepath.Base(f.Name))
		if !want[base] || f.FileInfo().IsDir() {
			continue
		}
		if err := extractOne(f, filepath.Join(dir, filepath.Base(f.Name))); err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("%s not found in archive", strings.Join(names, ", "))
	}
	return nil
}

func extractOne(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// RemuxCmd builds an ffmpeg command that rewraps a captured stream (MPEG-TS or
// fragmented MP4) into a regular MP4 without re-encoding. Only the audio and
// video tracks are kept: HLS often carries timed-ID3 data tracks the MP4 muxer
// rejects.
func RemuxCmd(ctx context.Context, in, out string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, ffmpegPath(),
		"-y", "-loglevel", "error",
		"-i", in,
		"-map", "0:v?", "-map", "0:a?", "-c", "copy",
		"-movflags", "+faststart", out)
	procutil.Hidden(cmd)
	return cmd
}

// MergeCmd joins separate format files (a video-only and an audio-only stream
// that yt-dlp couldn't merge) into one container, stream copy.
func MergeCmd(ctx context.Context, inputs []string, out string) *exec.Cmd {
	args := []string{"-y", "-loglevel", "error"}
	for _, in := range inputs {
		args = append(args, "-i", in)
	}
	for i := range inputs {
		args = append(args, "-map", fmt.Sprintf("%d:v?", i), "-map", fmt.Sprintf("%d:a?", i))
	}
	args = append(args, "-c", "copy")
	if strings.HasSuffix(strings.ToLower(out), ".mp4") {
		args = append(args, "-movflags", "+faststart")
	}
	cmd := exec.CommandContext(ctx, ffmpegPath(), append(args, out)...)
	procutil.Hidden(cmd)
	return cmd
}
