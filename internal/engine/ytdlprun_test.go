package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"myidm/internal/ytdlp"
)

func touch(t *testing.T, p string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		os.Chtimes(p, mod, mod)
	}
}

// yt-dlp writes under a task-unique name, so two tasks with the same title (or
// a re-download in another quality) never meet an "already downloaded" file,
// and a title's own "%" can't be read as a template field.
func TestYtdlpWorkNames(t *testing.T) {
	dir := t.TempDir()
	task := &Task{ID: "ab12cd34", Title: "100% Real Episode 1", Dir: dir}
	stem := ytdlpStem(task)
	tmpl := ytdlpTemplate(task)
	if !strings.HasSuffix(tmpl, strings.ReplaceAll(stem, "%", "%%")+".ab12cd34.%(ext)s") || !strings.Contains(tmpl, "100%% Real") {
		t.Fatalf("template = %q", tmpl)
	}
	ws := ytdlpWorkStem(task)
	if n, ok := ytdlpDisplayName(task, filepath.Join(dir, ws+".webm")); !ok || n != stem+".webm" {
		t.Errorf("display name = %q, %v", n, ok)
	}
	if _, ok := ytdlpDisplayName(task, filepath.Join(dir, ws+".f137.mp4")); ok {
		t.Error("a per-format part must not rename the row")
	}
	if _, ok := ytdlpDisplayName(task, filepath.Join(dir, "Other.mp4")); ok {
		t.Error("another task's file must not rename the row")
	}

	// The finished file moves to a free clean name.
	touch(t, filepath.Join(dir, stem+".mp4"), time.Time{}) // an earlier download of the same title
	produced := filepath.Join(dir, ws+".mp4")
	touch(t, produced, time.Time{})
	final := placeYtdlpOutput(task, produced)
	if filepath.Base(final) != stem+" (1).mp4" || !fileExistsOnDisk(final) || fileExistsOnDisk(produced) {
		t.Errorf("placed at %q", final)
	}

	// Leftovers of an aborted run are this task's alone to remove.
	touch(t, filepath.Join(dir, ws+".mp3.part"), time.Time{})
	touch(t, filepath.Join(dir, ws+".f140.m4a"), time.Time{})
	removeWorkFiles(task)
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("after cleanup: %v, want only the two finished files", names)
	}
}

// findProducedFile matches the exact stem (or "stem (N)"), never a longer title
// that merely starts with it, and a run only claims files written since it began.
func TestProducedFileExactAndRecent(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	touch(t, filepath.Join(dir, "Episode 1.mp4"), old)
	touch(t, filepath.Join(dir, "Episode 10.mp4"), time.Time{})
	touch(t, filepath.Join(dir, "Episode 1.mkv.part"), time.Time{})
	if got := filepath.Base(findProducedFile(dir, "Episode 1")); got != "Episode 1.mp4" {
		t.Errorf("findProducedFile = %q, want the exact name (not Episode 10)", got)
	}
	if got := producedFile(dir, "Episode 1", time.Now()); got != "" {
		t.Errorf("an hour-old file was claimed by a new run: %q", got)
	}
	touch(t, filepath.Join(dir, "Episode 1 (1).webm"), time.Time{})
	if got := filepath.Base(producedFile(dir, "Episode 1", time.Now())); got != "Episode 1 (1).webm" {
		t.Errorf("producedFile = %q", got)
	}
}

func TestMultiEntryLine(t *testing.T) {
	for line, want := range map[string]bool{
		"[generic] Playlist HTML Video: Downloading 2 items of 2":         true,
		"[download] Downloading item 1 of 4":                              true,
		"[download] Downloading item 1 of 1":                              false,
		"[youtube] Downloading just the video X because of --no-playlist": false,
	} {
		if multiEntryLine(line) != want {
			t.Errorf("multiEntryLine(%q) = %v", line, !want)
		}
	}
}

// A page whose only media is a short sound is not downloaded as "the video".
func TestOnlyClip(t *testing.T) {
	if !onlyClip(ytdlp.DownOption{Size: 1100 << 10}, 0) || !onlyClip(ytdlp.DownOption{}, 12) {
		t.Error("a 1.1 MB / 12 s clip should be refused")
	}
	if onlyClip(ytdlp.DownOption{Size: 24 << 20}, 0) || onlyClip(ytdlp.DownOption{}, 0) {
		t.Error("a podcast-sized or unsized audio should download")
	}
}
