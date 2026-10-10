package ytdlp

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"

	"myidm/internal/procutil"
)

// MediaStreams reports what a finished file really holds, read with ffprobe
// (extracted next to ffmpeg). A cover picture in an MP3 is not video. ok is
// false when ffprobe is missing or can't read the file — callers then trust
// the download as before.
func MediaStreams(ctx context.Context, path string) (video, audio bool, duration float64, ok bool) {
	bin := find("ffprobe")
	if bin == "" {
		return false, false, 0, false
	}
	cmd := exec.CommandContext(ctx, bin, "-v", "error",
		"-show_entries", "stream=codec_type:stream_disposition=attached_pic:format=duration", "-of", "json", path)
	procutil.Hidden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false, false, 0, false
	}
	return parseStreams(out)
}

func parseStreams(out []byte) (video, audio bool, duration float64, ok bool) {
	var r struct {
		Streams []struct {
			CodecType   string `json:"codec_type"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &r) != nil {
		return false, false, 0, false
	}
	for _, s := range r.Streams {
		switch s.CodecType {
		case "video":
			if s.Disposition.AttachedPic == 0 {
				video = true
			}
		case "audio":
			audio = true
		}
	}
	duration, _ = strconv.ParseFloat(r.Format.Duration, 64)
	return video, audio, duration, len(r.Streams) > 0
}
