package ytdlp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func parseInfo(t *testing.T, js string) *info {
	t.Helper()
	var in info
	if err := json.Unmarshal([]byte(js), &in); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return &in
}

func optionWith(res []DownOption, sub string) *DownOption {
	for i := range res {
		if strings.Contains(res[i].Label, sub) {
			return &res[i]
		}
	}
	return nil
}

// YouTube often omits filesize on adaptive formats but reports tbr; distil must
// still produce a size for every option (estimated from tbr × duration), and a
// video-only stream's total must include the audio it gets muxed with.
func TestDistilEstimatesSizes(t *testing.T) {
	in := &info{
		Title:    "Clip",
		Duration: 600, // 10 minutes
		Formats: []Format{
			{ID: "248", Ext: "webm", Height: 1080, VCodec: "vp9", ACodec: "none", TBR: 2000},               // video-only, no filesize
			{ID: "136", Ext: "mp4", Height: 720, VCodec: "avc1", ACodec: "none", FileSizeApprox: 50 << 20}, // video-only, reported size
			{ID: "251", Ext: "webm", Height: 0, VCodec: "none", ACodec: "opus", TBR: 128},                  // audio-only
		},
	}
	res := buildResult(in)
	if len(res.Options) == 0 || !res.HasVideo {
		t.Fatal("no options")
	}
	for _, o := range res.Options {
		if o.Size <= 0 {
			t.Errorf("option %q has no size", o.Label)
		}
	}
	// 1080 video-only (~143MB from tbr) must include the audio (~9MB), so > 150MB.
	o := optionWith(res.Options, "1080p")
	if o == nil || o.Size < 150<<20 {
		t.Fatalf("1080p option %+v should include audio (>150MB)", o)
	}
	if res.Options[0].Selector != "248+ba/248/bv*+ba/b" || res.Options[0].Ext != "mp4" {
		t.Errorf("best = %+v, want the 1080p stream + audio merged to mp4", res.Options[0])
	}
}

// archive.org: formats with null codecs (only ext/height/filesize) used to be
// dropped, leaving "Best (size 0)" that then downloaded the OGV.
func TestDistilNullCodecsPrefersMP4(t *testing.T) {
	in := parseInfo(t, `{"_type":"video","id":"Popeye_forPresident","title":"Popeye for President","duration":370.2,
	 "extractor_key":"ArchiveOrg","ext":"ogv","url":"https://archive.org/download/P/P.ogv",
	 "formats":[
	  {"format_id":"0","ext":"mp4","height":240,"width":320,"vcodec":null,"acodec":null,"filesize":26989036,"protocol":"https","url":"https://archive.org/download/P/P_512kb.mp4"},
	  {"format_id":"1","ext":"ogv","height":304,"width":400,"vcodec":null,"acodec":null,"filesize":26138707,"protocol":"https","url":"https://archive.org/download/P/P.ogv"}]}`)
	res := buildResult(in)
	best := res.Options[0]
	if best.Ext != "mp4" || best.Size != 26989036 || !strings.HasPrefix(best.Selector, "0/") || best.Estimated {
		t.Errorf("best = %+v, want the exact-size MP4", best)
	}
	if optionWith(res.Options, "OGV - 304p") == nil || optionWith(res.Options, "MP4 - 240p") == nil {
		t.Errorf("per-quality options missing: %+v", res.Options)
	}
	if a := res.Options[len(res.Options)-1]; !a.Audio || a.Selector != "ba/b" || a.Size <= 0 {
		t.Errorf("audio option = %+v, want ba/b with an estimate", a)
	}
}

// Streamable / Twitch clips: one format with a height but vcodec=null.
func TestDistilSingleUnknownCodecFormat(t *testing.T) {
	in := parseInfo(t, `{"title":"me irl","duration":12,"formats":[
	  {"format_id":"mp4-high","ext":"mp4","height":480,"width":852,"vcodec":null,"acodec":null,"filesize":3044857,"url":"https://cdn.test/a.mp4","protocol":"https"}]}`)
	res := buildResult(in)
	if !res.HasVideo || res.Options[0].Size != 3044857 || !strings.Contains(res.Options[0].Label, "480p MP4") {
		t.Errorf("best = %+v", res.Options[0])
	}
	if len(res.Options) != 2 || !res.Options[1].Audio {
		t.Errorf("want Best + Audio only, got %+v", res.Options)
	}
}

// SoundCloud: audio-only media must not default to "video + audio" in .mp4;
// the 128k MP3 beats the 96k AAC.
func TestDistilAudioOnlyMedia(t *testing.T) {
	in := parseInfo(t, `{"title":"Song","duration":136,"formats":[
	  {"format_id":"hls_aac_96k","ext":"m4a","vcodec":"none","acodec":"aac","abr":96,"protocol":"m3u8_native","url":"https://x.test/a.m3u8"},
	  {"format_id":"http_mp3_128","ext":"mp3","vcodec":"none","acodec":"mp3","abr":128,"protocol":"http","url":"https://x.test/a.mp3"},
	  {"format_id":"hls_opus_64","ext":"opus","vcodec":"none","acodec":"opus","abr":64,"protocol":"m3u8_native","url":"https://x.test/b.m3u8"}]}`)
	res := buildResult(in)
	if res.HasVideo {
		t.Error("audio-only media reported as video")
	}
	o := res.Options[0]
	if o.Ext != "mp3" || !o.Audio || !strings.HasPrefix(o.Selector, "http_mp3_128/") || o.Size <= 0 {
		t.Errorf("first option = %+v, want the MP3 stream", o)
	}
	for _, o := range res.Options {
		if strings.Contains(o.Label, "video") || o.Ext == "mp4" {
			t.Errorf("audio-only media offers %+v", o)
		}
	}
	// Only AAC: the original stream first (no conversion), MP3 as the extra.
	in = parseInfo(t, `{"title":"Song","formats":[{"format_id":"a","ext":"m4a","vcodec":"none","acodec":"aac","abr":96,"url":"https://x.test/a"}]}`)
	res = buildResult(in)
	if len(res.Options) != 2 || res.Options[0].Ext != "m4a" || res.Options[0].Audio || !res.Options[1].Audio {
		t.Errorf("aac-only options = %+v", res.Options)
	}
}

// Video formats that are all known to be silent get no "Audio only".
func TestDistilNoAudioHidesAudioOption(t *testing.T) {
	in := parseInfo(t, `{"title":"x","formats":[{"format_id":"v","ext":"mp4","height":720,"vcodec":"avc1","acodec":"none","url":"https://x.test/v"}]}`)
	res := buildResult(in)
	for _, o := range res.Options {
		if o.Audio {
			t.Errorf("silent video offers %+v", o)
		}
	}
	if res.Options[0].Selector != "v/bv*+ba/b" || res.Options[0].Ext != "mp4" {
		t.Errorf("best = %+v", res.Options[0])
	}
}

// A generic page (arabic-toons, NASA SVS, a podcast): the first entries are a
// sound effect and an ad; the episode is the third. Sizes come from the
// servers because the generic extractor reports none.
func TestProbeGenericPlaylistRecommendsMainVideo(t *testing.T) {
	sizes := map[string]int64{"/sfx.mp3": 40 << 10, "/ads/pre.mp4": 1100 << 10, "/ep.mp4": 73 << 20, "/ep.mov": 4 << 30}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/page.mp4" {
			w.Header().Set("Content-Type", "text/html")
			return
		}
		n, ok := sizes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodHead {
			http.Error(w, "HEAD only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Length", itoa(n))
	}))
	defer srv.Close()
	s := srv.URL
	in := parseInfo(t, `{"_type":"playlist","title":"Episode 12 | Toons","webpage_url":"https://toons.test/ep12","extractor_key":"Generic","entries":[
	  {"_type":"video","id":"ep12-1","title":"Episode 12 | Toons (1)","extractor_key":"HTML5MediaEmbed","playlist_index":1,"url":"`+s+`/sfx.mp3","ext":"mp3",
	   "formats":[{"format_id":"0","ext":"mp3","vcodec":"none","acodec":"mp3","url":"`+s+`/sfx.mp3","protocol":"http"}]},
	  {"_type":"video","id":"ep12-2","title":"Episode 12 | Toons (2)","extractor_key":"HTML5MediaEmbed","playlist_index":2,"url":"`+s+`/ads/pre.mp4","ext":"mp4",
	   "formats":[{"format_id":"0","ext":"mp4","url":"`+s+`/ads/pre.mp4","protocol":"http"}]},
	  {"_type":"video","id":"ep12-3","title":"Episode 12 | Toons (3)","extractor_key":"HTML5MediaEmbed","playlist_index":3,"url":"`+s+`/ep.mp4","ext":"mp4",
	   "formats":[{"format_id":"0","ext":"mov","url":"`+s+`/ep.mov","protocol":"https"},{"format_id":"1","ext":"mp4","url":"`+s+`/ep.mp4","protocol":"https"}]},
	  {"_type":"video","id":"ep12-4","title":"Episode 12 | Toons (4)","extractor_key":"HTML5MediaEmbed","playlist_index":4,"url":"`+s+`/page.mp4","ext":"mp4",
	   "formats":[{"format_id":"0","ext":"mp4","url":"`+s+`/page.mp4","protocol":"https"}]}]}`)
	normalize(in)
	fillSizes(context.Background(), in, Opts{UserAgent: "UA"})
	res := buildResult(in)
	if len(res.Entries) != 4 {
		t.Fatalf("entries = %d, want all 4 listed", len(res.Entries))
	}
	if res.Recommended != 3 || !res.HasVideo {
		t.Fatalf("recommended = %d (hasVideo %v), want the episode (3)", res.Recommended, res.HasVideo)
	}
	best := res.Options[0]
	if best.PlaylistItem != 3 || best.Size != 73<<20 || best.Ext != "mp4" || !strings.HasPrefix(best.Selector, "1/") {
		t.Errorf("best = %+v, want entry 3's 73 MB MP4", best)
	}
	if optionWith(res.Options, "MOV") == nil {
		t.Errorf("the ProRes MOV should stay pickable: %+v", res.Options)
	}
	if e := res.Entries[0]; e.Title != "Episode 12 | Toons" || e.HasVideo || e.URL != s+"/sfx.mp3" {
		t.Errorf("entry 1 = %+v", e)
	}
	if !res.Entries[1].ad || res.Entries[1].Size != 1100<<10 {
		t.Errorf("entry 2 should be a sized ad: %+v", res.Entries[1])
	}
	if len(res.Entries[3].Options) != 0 {
		t.Errorf("a URL answered with HTML is not media: %+v", res.Entries[3].Options)
	}
}

// Without the episode, a tiny clip still loses to a longer one, and an audio
// entry is recommended only when nothing has a picture.
func TestRecommendRanking(t *testing.T) {
	opt := []DownOption{{Label: "x"}}
	es := []ProbeEntry{
		{Index: 1, HasVideo: true, Duration: 6, Options: opt},
		{Index: 2, HasVideo: false, Duration: 3000, Options: opt},
		{Index: 3, HasVideo: true, Duration: 1400, Options: opt},
		{Index: 4, HasVideo: true, Duration: 900, Options: opt},
	}
	if got := recommend(es); got != 2 {
		t.Errorf("recommend = entry %d, want 3", got+1)
	}
	if got := recommend(es[1:2]); got != 0 {
		t.Errorf("a lone audio entry must still be recommended, got %d", got)
	}
}

// Dropbox's "video" of an archive, a generic page's .exe: a plain file.
func TestProbeDirectFile(t *testing.T) {
	in := parseInfo(t, `{"_type":"video","title":"backup.tar","extractor_key":"Dropbox","formats":[
	  {"format_id":"original","ext":"gz","vcodec":"none","acodec":"none","url":"https://dl.dropboxusercontent.test/s/abc/backup.tar.gz?dl=1"}]}`)
	res := buildResult(in)
	if res.DirectFile != "https://dl.dropboxusercontent.test/s/abc/backup.tar.gz?dl=1" || len(res.Options) != 0 {
		t.Errorf("dropbox archive: %+v", res)
	}
	in = parseInfo(t, `{"_type":"video","title":"setup","extractor_key":"Generic","ext":"unknown_video","url":"https://mirror.test/files/setup-1.2.exe"}`)
	res = buildResult(in)
	if res.DirectFile != "https://mirror.test/files/setup-1.2.exe" || len(res.Options) != 0 {
		t.Errorf("generic exe: %+v", res)
	}
	// A direct media link stays with yt-dlp.
	in = parseInfo(t, `{"_type":"video","title":"clip","extractor_key":"Generic","ext":"mp4","url":"https://cdn.test/clip.mp4","direct":true}`)
	res = buildResult(in)
	if res.DirectFile != "" || len(res.Options) == 0 || !res.HasVideo {
		t.Errorf("direct mp4: %+v", res)
	}
}

// Formats with has_drm or FairPlay ids can't be saved and aren't offered.
func TestDistilSkipsDRM(t *testing.T) {
	in := parseInfo(t, `{"title":"x","formats":[
	  {"format_id":"hls-fairplay-720","ext":"mp4","height":720,"vcodec":"avc1","acodec":"aac","url":"https://x.test/a"},
	  {"format_id":"dash-720","ext":"mp4","height":720,"vcodec":"avc1","acodec":"aac","has_drm":true,"url":"https://x.test/b"},
	  {"format_id":"http-360","ext":"mp4","height":360,"vcodec":"avc1","acodec":"aac","has_drm":"maybe","url":"https://x.test/c"}]}`)
	res := buildResult(in)
	if !strings.HasPrefix(res.Options[0].Selector, "http-360/") {
		t.Errorf("best = %+v, want the clear 360p", res.Options[0])
	}
}

func TestFriendlyError(t *testing.T) {
	cases := map[string]string{
		"ERROR: [ok.ru] 123: the JSON object must be str, bytes or bytearray, not dict; please report this issue on  https://github.com/yt-dlp/yt-dlp/issues?q= , filling out the appropriate issue template. Confirm you are on the latest version using  yt-dlp -U": "can't read this site",
		"ERROR: [vimeo] 76979871: The web client only works when logged-in. Use --cookies":         "signed in",
		"ERROR: [RumbleEmbed] vb0ofn: Unable to download JSON metadata: HTTP Error 403: Forbidden": "Unable to download JSON metadata: HTTP Error 403: Forbidden",
		"ERROR: [vimeo] 1: This format is DRM protected; Try selecting another format":             "DRM-protected",
		"ERROR: Unsupported URL: https://plyr.io/":                                                 "doesn't support this site",
	}
	for in, want := range cases {
		got := FriendlyError(in)
		if !strings.Contains(got, want) || strings.Contains(got, "report this issue") || strings.Contains(got, "\n") {
			t.Errorf("FriendlyError(%.50q) = %q, want it to contain %q", in, got, want)
		}
	}
	if l := ErrorLine("[generic] x: Downloading webpage\nERROR: real reason\nTraceback (most recent call last):\n  File x"); l != "real reason" {
		t.Errorf("ErrorLine = %q", l)
	}
	if !NeedsImpersonation("ERROR: [RumbleEmbed] vb0ofn: Unable to download JSON metadata: HTTP Error 403: Forbidden") {
		t.Error("rumble's 403 should retry with impersonation")
	}
	if got := AltURL("https://vimeo.com/76979871", "The web client only works when logged-in"); got != "https://player.vimeo.com/video/76979871" {
		t.Errorf("AltURL = %q", got)
	}
	if got := AltURL("https://vimeo.com/76979871", "HTTP Error 404"); got != "" {
		t.Errorf("AltURL for another error = %q", got)
	}
}

func TestParseFlatPlaylist(t *testing.T) {
	var fp flatPlaylist
	json.Unmarshal([]byte(`{"_type":"playlist","title":"Bili parts","entries":[
	  {"url":"https://www.bilibili.com/video/BV1?p=1","ie_key":"BiliBili"},
	  {"url":"https://www.bilibili.com/video/BV1?p=2","ie_key":"BiliBili","duration":61},
	  {"id":"x9","url":"x9","ie_key":"Dailymotion"},
	  null]}`), &fp)
	pl := parseFlatPlaylist(&fp, "https://www.bilibili.com/video/BV1")
	if pl == nil || len(pl.Entries) != 2 {
		t.Fatalf("playlist = %+v", pl)
	}
	if pl.Entries[0].Title != "Bili parts - part 1" || pl.Entries[1].Title != "Bili parts - part 2" || pl.Entries[1].Duration != 61 {
		t.Errorf("entries = %+v", pl.Entries)
	}
	json.Unmarshal([]byte(`{"_type":"playlist","title":"yt","entries":[{"id":"abc","url":"abc","title":"A","ie_key":"Youtube"}]}`), &fp)
	pl = parseFlatPlaylist(&fp, "https://www.youtube.com/playlist?list=PL1")
	if pl == nil || pl.Entries[0].URL != "https://www.youtube.com/watch?v=abc" {
		t.Errorf("youtube bare id: %+v", pl)
	}
}

func TestDownloadArgsPlaylistItemAndDownloader(t *testing.T) {
	a := strings.Join(downloadArgs("https://p.test/", "1/b", "o.%(ext)s", Opts{PlaylistItem: 3}, false, 4, "", false), " ")
	if !strings.Contains(a, "--playlist-items 3") || strings.Contains(a, "--no-playlist") {
		t.Errorf("args = %s", a)
	}
	if !strings.Contains(a, "--concurrent-fragments 4") || strings.Contains(a, "aria2c") {
		t.Errorf("native downloader args = %s", a)
	}
	a = strings.Join(downloadArgs("https://p.test/", "b", "o", Opts{}, false, 4, "", true), " ")
	if !strings.Contains(a, "--no-playlist") || !strings.Contains(a, "--downloader aria2c") {
		t.Errorf("args = %s", a)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// An MP3's cover picture is a "video" stream to ffprobe, but not a video.
func TestParseStreams(t *testing.T) {
	v, a, d, ok := parseStreams([]byte(`{"streams":[{"codec_type":"audio"},{"codec_type":"video","disposition":{"attached_pic":1}}],"format":{"duration":"65.2"}}`))
	if !ok || v || !a || d != 65.2 {
		t.Errorf("mp3 with cover: video=%v audio=%v dur=%v ok=%v", v, a, d, ok)
	}
	if v, _, _, ok := parseStreams([]byte(`{"streams":[{"codec_type":"video","disposition":{"attached_pic":0}},{"codec_type":"audio"}],"format":{}}`)); !ok || !v {
		t.Error("a real video stream was missed")
	}
}

// YouTube lists HLS formats (itags 229-232, 269-270: fragmented, sized from
// bitrate) next to the real DASH files at every height. The direct files must
// win: D BOX's burst path fetches them ~10x faster, and their sizes are real.
func TestDistilPrefersDirectOverHLS(t *testing.T) {
	in := parseInfo(t, `{"title":"ASMR","duration":3600,"extractor_key":"Youtube","formats":[
	 {"format_id":"270","ext":"mp4","protocol":"m3u8_native","vcodec":"avc1.640028","acodec":"none","width":1920,"height":1080,"tbr":3850},
	 {"format_id":"137","ext":"mp4","protocol":"https","vcodec":"avc1.640028","acodec":"none","width":1920,"height":1080,"filesize":560000000},
	 {"format_id":"248","ext":"webm","protocol":"https","vcodec":"vp9","acodec":"none","width":1920,"height":1080,"filesize":520000000},
	 {"format_id":"232","ext":"mp4","protocol":"m3u8_native","vcodec":"avc1.4d401f","acodec":"none","width":1280,"height":720,"tbr":1530},
	 {"format_id":"136","ext":"mp4","protocol":"https","vcodec":"avc1.4d401f","acodec":"none","width":1280,"height":720,"filesize":280000000},
	 {"format_id":"251","ext":"webm","protocol":"https","vcodec":"none","acodec":"opus","abr":130,"filesize":60000000}
	]}`)
	res := buildResult(in)
	if len(res.Options) == 0 {
		t.Fatal("no options")
	}
	best := res.Options[0]
	if !strings.HasPrefix(best.Selector, "137+") || best.Estimated || best.Size != 620000000 {
		t.Fatalf("best = %+v, want the direct 137 (+audio, exact 620 MB)", best)
	}
	for _, o := range res.Options {
		if strings.HasPrefix(o.Selector, "270") || strings.HasPrefix(o.Selector, "232") {
			t.Errorf("HLS format offered: %+v", o)
		}
	}
	if o := optionWith(res.Options, "720p"); o == nil || !strings.HasPrefix(o.Selector, "136+") {
		t.Errorf("720p option = %+v, want direct 136", o)
	}
}
