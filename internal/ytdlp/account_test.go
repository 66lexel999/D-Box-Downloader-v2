package ytdlp

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestRequestArgsYouTubeDropsBrowserContext(t *testing.T) {
	o := Opts{UserAgent: "Mozilla/5.0 Chrome/140", Referer: "https://www.youtube.com/",
		Headers: map[string]string{"Cookie": "SID=abc; HSID=def", "Origin": "https://www.youtube.com"}}
	for _, u := range []string{"https://www.youtube.com/watch?v=pyBNEX5gHYA", "https://youtu.be/pyBNEX5gHYA",
		"https://m.youtube.com/shorts/abc"} {
		args := requestArgs(u, o)
		for _, bad := range []string{"--user-agent", "--referer", "--add-headers", "--cookies"} {
			if slices.Contains(args, bad) {
				t.Errorf("%s: %s passed to YouTube by default: %v", u, bad, args)
			}
		}
	}
}

func TestRequestArgsYouTubeAccountUsesJar(t *testing.T) {
	o := Opts{UserAgent: "UA", UseAccount: true, Headers: map[string]string{"cookie": "SAPISID=xyz; SID=abc"}}
	args := requestArgs("https://www.youtube.com/watch?v=x", o)
	if strings.Contains(strings.Join(args, " "), "Cookie:") {
		t.Fatalf("cookies passed as a header: %v", args)
	}
	jar := argAfter(args, "--cookies")
	if jar == "" {
		t.Fatalf("no --cookies jar: %v", args)
	}
	b, err := os.ReadFile(jar)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "# Netscape HTTP Cookie File") ||
		!strings.Contains(s, ".youtube.com\tTRUE\t/\tFALSE\t") || !strings.Contains(s, "\tSAPISID\txyz\n") ||
		!strings.Contains(s, "\tSID\tabc\n") {
		t.Fatalf("bad jar:\n%s", s)
	}
}

func TestRequestArgsOtherSitesKeepContextButCookiesAsJar(t *testing.T) {
	o := Opts{UserAgent: "UA", Referer: "https://site.test/watch/1",
		Headers: map[string]string{"Cookie": "sess=1", "Origin": "https://site.test"}}
	args := requestArgs("https://cdn.site.test/v/1", o)
	if argAfter(args, "--user-agent") != "UA" || argAfter(args, "--referer") != "https://site.test/watch/1" {
		t.Fatalf("browser context dropped for a normal site: %v", args)
	}
	if argAfter(args, "--add-headers") != "Origin:https://site.test" {
		t.Fatalf("Origin header missing: %v", args)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "Cookie:") || argAfter(args, "--cookies") == "" {
		t.Fatalf("cookies must go as a jar, not a header: %v", args)
	}
}

func TestAccountRetry(t *testing.T) {
	withCookies := Opts{Headers: map[string]string{"Cookie": "SID=1"}}
	yt := "https://www.youtube.com/watch?v=x"
	if !AccountRetry(yt, withCookies, "ERROR: [youtube] x: Sign in to confirm your age. This video may be inappropriate for some users.") {
		t.Error("age gate should retry signed in")
	}
	if AccountRetry(yt, withCookies, "ERROR: [youtube] x: The page needs to be reloaded.") {
		t.Error("reload error is not an account problem")
	}
	if AccountRetry(yt, Opts{}, "Sign in to confirm your age") {
		t.Error("no cookies → nothing to retry with")
	}
	used := withCookies
	used.UseAccount = true
	if AccountRetry(yt, used, "Sign in to confirm your age") {
		t.Error("already signed in → no second retry")
	}
	if AccountRetry("https://vimeo.com/1", withCookies, "login required") {
		t.Error("account retry is YouTube-only")
	}
}

func TestFriendlyReloadError(t *testing.T) {
	got := FriendlyError("ERROR: [youtube] pyBNEX5gHYA: The page needs to be reloaded.")
	if !strings.Contains(got, "YouTube refused") {
		t.Fatalf("got %q", got)
	}
}
