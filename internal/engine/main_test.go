package engine

import (
	"context"
	"os"
	"testing"

	"myidm/internal/ytdlp"
)

// TestMain keeps the suite offline: an engine that finds no ffmpeg would
// otherwise download it (~190 MB) mid-test. Without it HLS captures stay .ts,
// which the stream tests accept.
func TestMain(m *testing.M) {
	provisionFFmpeg = func(context.Context, func(done, total int64)) (string, error) {
		return "", ytdlp.ErrNoFFmpeg
	}
	os.Exit(m.Run())
}
