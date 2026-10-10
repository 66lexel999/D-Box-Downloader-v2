package engine

// Share links: some hosts' share links open a preview page by design and have a
// documented switch for the file itself. Rewriting them before the probe saves
// a landing-page round trip — and for Dropbox, whose preview is rendered by
// script, it is the only way to reach the file at all.

import (
	"net/url"
	"regexp"
	"strings"
)

// normalizeLink rewrites known share-page links to their direct-download form;
// anything else comes back unchanged.
func normalizeLink(u *url.URL) *url.URL {
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch {
	case host == "dropbox.com" && dropboxShareRE.MatchString(u.Path):
		// ?dl=0 (or no dl at all) is the preview page; dl=1 is the file (a
		// folder link downloads as one zip). raw=1 already serves the file.
		q := u.Query()
		if q.Get("dl") == "1" || q.Get("raw") == "1" {
			return u
		}
		v := *u
		v.RawQuery = setQueryParam(u.RawQuery, "dl", "1")
		return &v
	case host == "drive.google.com":
		if id, rk := driveFileID(u); id != "" {
			q := "export=download&id=" + url.QueryEscape(id)
			if rk != "" {
				q += "&resourcekey=" + url.QueryEscape(rk)
			}
			return &url.URL{Scheme: "https", Host: "drive.google.com", Path: "/uc", RawQuery: q}
		}
	}
	return u
}

// dropboxShareRE matches Dropbox share links: /s/<key>/<name>, /sh/… (a file in
// a shared folder, or the folder), /scl/fi/… (files) and /scl/fo/… (folders).
var dropboxShareRE = regexp.MustCompile(`^/(s|sh|scl/fi|scl/fo)/`)

// driveFileRE matches a Drive file's viewer page: /file/d/<id>/view (or /edit,
// /preview, or nothing after the id).
var driveFileRE = regexp.MustCompile(`^/file/d/([A-Za-z0-9_-]{10,})(?:/(?:view|edit|preview)?)?$`)

// driveFileID extracts the file id (and resource key) from a Google Drive
// viewer link: /file/d/<id>/view or /open?id=<id>. "" for other Drive pages
// (folders, uc links, which are already downloads).
func driveFileID(u *url.URL) (id, resourceKey string) {
	q := u.Query()
	if m := driveFileRE.FindStringSubmatch(u.Path); m != nil {
		return m[1], q.Get("resourcekey")
	}
	if u.Path == "/open" && q.Get("id") != "" {
		return q.Get("id"), q.Get("resourcekey")
	}
	return "", ""
}

// setQueryParam sets key=value in a raw query, keeping every other parameter's
// order and encoding as it was (url.Values.Encode would re-sort them).
func setQueryParam(raw, key, value string) string {
	parts := strings.Split(raw, "&")
	out := parts[:0]
	set := false
	for _, p := range parts {
		if p == "" {
			continue
		}
		k, _, _ := strings.Cut(p, "=")
		if dk, err := url.QueryUnescape(k); err == nil && dk == key {
			if !set {
				out = append(out, key+"="+url.QueryEscape(value))
				set = true
			}
			continue
		}
		out = append(out, p)
	}
	if !set {
		out = append(out, key+"="+url.QueryEscape(value))
	}
	return strings.Join(out, "&")
}
