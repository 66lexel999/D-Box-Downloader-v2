package ytdlp

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseProgressVariants(t *testing.T) {
	p, ok := ParseProgress("[download]  45.2% of ~ 100.00MiB at  2.50MiB/s ETA 00:30 (frag 12/40)")
	if !ok || p.Percent != 45.2 || p.Total != 100<<20 || !p.Estimated || p.SpeedBPS != 2.5*(1<<20) || p.Frag != 12 || p.Frags != 40 {
		t.Errorf("fragment line: %+v %v", p, ok)
	}
	p, ok = ParseProgress("[download]  10.0% of    1.20GiB at  512.00KiB/s ETA 20:00")
	if !ok || p.Total != scaled(1.2, 30) || p.Estimated || p.SpeedBPS != 512*1024 {
		t.Errorf("GiB line: %+v", p)
	}
	p, ok = ParseProgress("[download]  50.0% of  900.00B at  100.00B/s ETA 00:05")
	if !ok || p.Total != 900 || p.SpeedBPS != 100 {
		t.Errorf("bytes line: %+v", p)
	}
	p, ok = ParseProgress("[download]   12.34MiB at    1.23MiB/s (00:00:10)")
	if !ok || p.Percent != -1 || p.Downloaded != scaled(12.34, 20) {
		t.Errorf("unknown-total line: %+v %v", p, ok)
	}
	if _, ok := ParseProgress("[youtube] abc: Downloading webpage"); ok {
		t.Error("non-progress line parsed")
	}
	f, ok := ParseFFmpegProgress("frame= 1200 fps=60 q=-1.0 size=   10240kB time=00:00:40.00 bitrate=2097.2kbits/s speed=2x")
	if !ok || f.Downloaded != 10240<<10 {
		t.Errorf("ffmpeg line: %+v %v", f, ok)
	}
}

func TestMatchCookies(t *testing.T) {
	jar := strings.Join([]string{
		"# Netscape HTTP Cookie File",
		".example.com\tTRUE\t/\tTRUE\t0\tsid\tabc",
		"#HttpOnly_.example.com\tTRUE\t/\tFALSE\t0\tauth\txyz",
		"www.example.com\tFALSE\t/private\tFALSE\t0\tscoped\t1",
		".example.com\tTRUE\t/\tFALSE\t1000\told\tgone",
		".other.com\tTRUE\t/\tFALSE\t0\tnope\t1",
	}, "\n")
	u, _ := url.Parse("https://cdn.example.com/file.zip")
	if got := matchCookies(jar, u, time.Unix(5000, 0)); got != "sid=abc; auth=xyz" {
		t.Errorf("subdomain match = %q", got)
	}
	u, _ = url.Parse("http://www.example.com/private/x")
	if got := matchCookies(jar, u, time.Unix(5000, 0)); got != "auth=xyz; scoped=1" {
		t.Errorf("host-only + path + insecure = %q", got)
	}
}

func TestOptsArgs(t *testing.T) {
	a := Opts{UserAgent: "UA", Referer: "https://page.test/", Headers: map[string]string{"Origin": "https://page.test", "Cookie": "a=1"}}.args()
	// Cookies never ride as a header (requestArgs turns them into a jar).
	want := "--user-agent|UA|--referer|https://page.test/|--add-headers|Origin:https://page.test"
	if got := strings.Join(a, "|"); got != want {
		t.Errorf("args = %s", got)
	}
}

// scaled mirrors the parser's float math at run time (constants can't truncate).
func scaled(v float64, shift uint) int64 { return int64(v * float64(uint64(1)<<shift)) }

func TestNeedsImpersonation(t *testing.T) {
	yes := `ERROR: [generic] Got HTTP Error 403 caused by Cloudflare anti-bot challenge; try again with --extractor-args "generic:impersonate"`
	if !NeedsImpersonation(yes) || NeedsImpersonation("ERROR: Unsupported URL: https://x.test") {
		t.Error("NeedsImpersonation misclassified")
	}
	a := strings.Join(Opts{UserAgent: "UA", Impersonate: true}.args(), " ")
	if a != "--impersonate chrome --extractor-args generic:impersonate" {
		t.Errorf("impersonate args = %q (the browser fingerprint must not get our UA)", a)
	}
}
