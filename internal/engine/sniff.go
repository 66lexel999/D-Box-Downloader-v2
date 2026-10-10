package engine

// Content detection: what IS this download? Servers routinely lie or say nothing
// (application/octet-stream for everything, text/plain for HLS playlists, a
// "video.php" URL serving an MP4), so we combine the Content-Type header with
// the file's magic bytes. The result drives both the file extension and how the
// download is performed (plain file, HLS stream, DASH manifest, or a web page
// whose video yt-dlp should extract).

import (
	"bytes"
	"mime"
	"strings"
	"unicode/utf8"
)

// Download kinds decided at probe time.
const (
	kindFile = "file"
	kindHLS  = "hls"
	kindDASH = "dash"
	kindPage = "page"
)

// contentTypeExts maps MIME types to the extension users expect. Curated rather
// than mime.ExtensionsByType, whose Windows registry answers are unstable
// (".jfif" for JPEG, ".m1v" for MPEG, …).
var contentTypeExts = map[string]string{
	"video/mp4": "mp4", "video/x-m4v": "m4v", "video/webm": "webm", "video/x-matroska": "mkv",
	"video/quicktime": "mov", "video/x-msvideo": "avi", "video/avi": "avi", "video/x-flv": "flv",
	"video/mp2t": "ts", "video/mpeg": "mpg", "video/3gpp": "3gp", "video/x-ms-wmv": "wmv",
	"video/ogg":  "ogv",
	"audio/mpeg": "mp3", "audio/mp3": "mp3", "audio/mp4": "m4a", "audio/x-m4a": "m4a", "audio/aac": "aac",
	"audio/x-aac": "aac", "audio/ogg": "ogg", "audio/opus": "opus", "audio/webm": "weba",
	"audio/flac": "flac", "audio/x-flac": "flac", "audio/wav": "wav", "audio/x-wav": "wav",
	"audio/wave": "wav", "audio/x-ms-wma": "wma",
	"image/jpeg": "jpg", "image/jpg": "jpg", "image/png": "png", "image/gif": "gif", "image/webp": "webp",
	"image/avif": "avif", "image/heic": "heic", "image/bmp": "bmp", "image/svg+xml": "svg",
	"image/tiff": "tiff", "image/x-icon": "ico", "image/vnd.microsoft.icon": "ico",
	"application/pdf": "pdf", "application/zip": "zip", "application/x-zip-compressed": "zip",
	"application/x-rar-compressed": "rar", "application/vnd.rar": "rar", "application/x-rar": "rar",
	"application/x-7z-compressed": "7z", "application/gzip": "gz", "application/x-gzip": "gz",
	"application/x-bzip2": "bz2", "application/x-xz": "xz", "application/zstd": "zst",
	"application/x-tar": "tar", "application/vnd.ms-cab-compressed": "cab",
	"application/x-iso9660-image": "iso",
	"application/x-msdownload":    "exe", "application/x-msdos-program": "exe", "application/x-dosexec": "exe",
	"application/vnd.microsoft.portable-executable": "exe", "application/x-msi": "msi",
	"application/x-ms-installer": "msi", "application/vnd.android.package-archive": "apk",
	"application/x-apple-diskimage": "dmg", "application/vnd.debian.binary-package": "deb",
	"application/x-rpm": "rpm", "application/java-archive": "jar",
	"application/msword": "doc", "application/vnd.ms-excel": "xls", "application/vnd.ms-powerpoint": "ppt",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   "docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         "xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": "pptx",
	"application/epub+zip": "epub", "application/rtf": "rtf", "text/csv": "csv", "text/plain": "txt",
	"application/json": "json", "application/xml": "xml", "text/xml": "xml",
	"text/html": "html", "application/xhtml+xml": "html",
	"application/x-bittorrent": "torrent", "application/x-subrip": "srt", "text/vtt": "vtt",
	"application/vnd.apple.mpegurl": "m3u8", "application/x-mpegurl": "m3u8", "audio/mpegurl": "m3u8",
	"audio/x-mpegurl": "m3u8", "application/dash+xml": "mpd",
}

// knownExtSet is every extension we recognize (used to judge URL path names).
var knownExtSet = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range contentTypeExts {
		m[e] = true
	}
	for _, e := range []string{"mp4", "mkv", "webm", "mov", "avi", "flv", "wmv", "m4v", "mpg", "mpeg", "ts",
		"3gp", "mp3", "m4a", "aac", "flac", "wav", "ogg", "opus", "wma", "zip", "rar", "7z", "gz", "tgz",
		"bz2", "xz", "zst", "tar", "iso", "img", "cab", "exe", "msi", "msix", "appx", "apk", "xapk", "dmg",
		"pkg", "deb", "rpm", "appimage", "jar", "bat", "cmd", "ps1", "pdf", "doc", "docx", "xls", "xlsx",
		"ppt", "pptx", "odt", "ods", "odp", "txt", "csv", "epub", "mobi", "azw3", "cbz", "cbr", "rtf",
		"jpg", "jpeg", "png", "gif", "webp", "bmp", "svg", "tiff", "tif", "ico", "heic", "avif", "psd",
		"raw", "cr2", "nef", "srt", "vtt", "ass", "torrent", "bin", "dll", "sys", "iso", "vhd", "vhdx",
		"ova", "vmdk", "m3u8", "mpd", "m3u", "json", "xml", "html", "htm", "js", "css", "ttf", "otf",
		"woff", "woff2", "sql", "db", "sqlite", "bak", "log", "md", "dat", "mkv", "ogv", "weba"} {
		m[e] = true
	}
	return m
}()

func knownExt(ext string) bool { return knownExtSet[strings.ToLower(ext)] }

// mediaType returns the lowercase MIME type without parameters.
func mediaType(ct string) string {
	if ct == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return strings.ToLower(mt)
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// detectExt names the content: magic bytes first (authoritative), then the
// Content-Type. "" when neither says anything useful (octet-stream, unknown).
func detectExt(sniff []byte, contentType string) string {
	if e := sniffExt(sniff); e != "" {
		// A ZIP container may really be an Office doc / APK / JAR / EPUB — let a
		// specific Content-Type refine the generic "zip" answer.
		if e == "zip" {
			if ct := contentTypeExts[mediaType(contentType)]; ct != "" && ct != "txt" && ct != "html" {
				return ct
			}
		}
		return e
	}
	return contentTypeExts[mediaType(contentType)]
}

// sniffExt identifies common formats from their leading bytes.
func sniffExt(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	has := func(off int, sig string) bool {
		return len(b) >= off+len(sig) && string(b[off:off+len(sig)]) == sig
	}
	switch {
	case has(4, "ftyp"):
		brand := ""
		if len(b) >= 12 {
			brand = string(b[8:12])
		}
		switch {
		case brand == "M4A " || brand == "M4B " || brand == "M4P ":
			return "m4a"
		case brand == "qt  ":
			return "mov"
		case strings.HasPrefix(brand, "3g"):
			return "3gp"
		case brand == "heic" || brand == "heix" || brand == "mif1":
			return "heic"
		case brand == "avif":
			return "avif"
		}
		return "mp4"
	case has(0, "\x1a\x45\xdf\xa3"):
		if bytes.Contains(b[:min(len(b), 64)], []byte("webm")) {
			return "webm"
		}
		return "mkv"
	case len(b) >= 189 && b[0] == 0x47 && b[188] == 0x47:
		return "ts"
	case has(0, "FLV\x01"):
		return "flv"
	case has(0, "\x30\x26\xb2\x75\x8e\x66\xcf\x11"):
		return "wmv"
	case has(0, "RIFF") && has(8, "AVI "):
		return "avi"
	case has(0, "RIFF") && has(8, "WAVE"):
		return "wav"
	case has(0, "RIFF") && has(8, "WEBP"):
		return "webp"
	case has(0, "ID3"):
		return "mp3"
	case has(0, "fLaC"):
		return "flac"
	case has(0, "OggS"):
		if bytes.Contains(b[:min(len(b), 64)], []byte("OpusHead")) {
			return "opus"
		}
		return "ogg"
	case len(b) >= 2 && b[0] == 0xFF && (b[1]&0xF6) == 0xF0:
		return "aac" // ADTS
	case len(b) >= 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0:
		return "mp3" // MPEG audio frame sync
	case has(0, "PK\x03\x04"):
		return "zip"
	case has(0, "Rar!\x1a\x07"):
		return "rar"
	case has(0, "7z\xbc\xaf\x27\x1c"):
		return "7z"
	case has(0, "\x1f\x8b"):
		return "gz"
	case has(0, "BZh"):
		return "bz2"
	case has(0, "\xfd7zXZ\x00"):
		return "xz"
	case has(0, "\x28\xb5\x2f\xfd"):
		return "zst"
	case has(0, "MSCF"):
		return "cab"
	case has(0, "%PDF"):
		return "pdf"
	case has(0, "MZ"):
		return "exe"
	case has(0, "\x89PNG"):
		return "png"
	case has(0, "\xff\xd8\xff"):
		return "jpg"
	case has(0, "GIF8"):
		return "gif"
	case has(0, "II*\x00"), has(0, "MM\x00*"):
		return "tiff"
	case has(0, "d8:announce"):
		return "torrent"
	}
	switch text := leadingText(b); {
	case strings.HasPrefix(text, "#EXTM3U"):
		return "m3u8"
	case strings.HasPrefix(text, "<MPD") || (strings.HasPrefix(text, "<?xml") && bytes.Contains(b, []byte("<MPD"))):
		return "mpd"
	case looksHTML(text):
		return "html"
	}
	return ""
}

// leadingText returns the start of b as text, minus a UTF-8 BOM and leading
// whitespace.
func leadingText(b []byte) string {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	b = bytes.TrimLeft(b, " \t\r\n")
	if len(b) > 1024 {
		b = b[:1024]
	}
	return string(b)
}

// htmlStarts are tags a web page (or an HTML error snippet) can open with.
var htmlStarts = []string{"<!doctype html", "<html", "<head", "<body", "<!-- ", "<!--[if", "<script",
	"<title", "<meta", "<link", "<style", "<div", "<iframe", "<form", "<noscript", "<p>", "<p ", "<br",
	"<table", "<center", "<h1", "<h2", "<span", "<pre>", "<a href", "<img", "<section", "<header", "<main"}

// looksHTML reports an HTML document. Real pages don't always open with the
// doctype: IE conditional comments, a bare <meta refresh>, a "<div>Link
// expired</div>" snippet or a comment banner come first on many sites.
func looksHTML(text string) bool {
	l := strings.ToLower(text)
	if !strings.HasPrefix(l, "<") {
		return false
	}
	for _, p := range htmlStarts {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	switch {
	case strings.HasPrefix(l, "<?xml"): // XHTML — but not RSS/SVG/other XML, which have <title> too
		return strings.Contains(l, "<html")
	case strings.HasPrefix(l, "<!--"): // a comment banner before the page itself
		for _, t := range []string{"<html", "<head", "<body", "<meta ", "<div", "<script"} {
			if strings.Contains(l, t) {
				return true
			}
		}
	}
	return false
}

// textualSample reports a body that is text — an HTML page, a JSON or XML
// error, a plain "File not found" — rather than a binary file. Playlists and
// manifests are text too, but they are streams, not refusals, so they don't
// count. A sample cut mid-character is fine; ≤10% odd bytes tolerates legacy
// (non-UTF-8) pages, while binary data runs far above that.
func textualSample(b []byte) bool {
	switch sniffExt(b) {
	case "html":
		return true
	case "":
	default:
		return false // recognized binary content, or an HLS/DASH manifest
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	bad, total := 0, 0
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			if !utf8.FullRune(b[i:]) {
				i = len(b) // a character cut off by the sample's end
				continue
			}
			bad++
		case r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != '\f', r == 0x7f:
			bad++
		}
		total++
		i += size
	}
	return total > 0 && bad*10 <= total
}

// classify decides how a probed URL should be downloaded, treating the URL's
// own name as the file the caller asked for (so "index.html" is a file to
// save). The probe uses classifyProbe, which knows more.
func classify(rawURL, contentType string, sniff []byte) string {
	return classifyProbe(rawURL, rawURL, contentType, sniff, pathName(rawURL))
}

// classifyProbe decides how a probed link is downloaded. orig is the link as
// given, final where its redirects ended. name is a file name somebody asked
// for explicitly (the user's name, the server's Content-Disposition): only
// that makes an HTML answer "a web page to save as a file". An .html URL alone
// is a page to look into (videos, links), and a redirect to "wait.html" must
// not turn a page into a file either.
func classifyProbe(orig, final, contentType string, sniff []byte, name string) string {
	mt := mediaType(contentType)
	se := sniffExt(sniff)
	_, origExt := splitExt(pathName(orig))
	_, finalExt := splitExt(pathName(final))
	urlIs := func(ext string) bool { return origExt == ext || finalExt == ext }
	switch {
	case se == "m3u8",
		(contentTypeExts[mt] == "m3u8" || urlIs("m3u8")) && !binaryMedia(se) && se != "html":
		return kindHLS
	case se == "mpd", (mt == "application/dash+xml" || urlIs("mpd")) && !binaryMedia(se) && se != "html":
		return kindDASH
	}
	isHTML := se == "html" || ((mt == "text/html" || mt == "application/xhtml+xml") && se == "")
	if isHTML && !pageFileExts[nameExt(name)] {
		return kindPage
	}
	return kindFile
}

// pageFileExts are names under which an HTML answer IS the requested file.
var pageFileExts = map[string]bool{"htm": true, "html": true, "xhtml": true, "mht": true, "mhtml": true, "shtml": true}

func nameExt(name string) string {
	_, ext := splitExt(name)
	return ext
}

// binaryMedia reports whether a sniffed type is real media/binary content (so a
// ".m3u8"-looking URL that actually serves an MP4 is still a plain file).
func binaryMedia(ext string) bool {
	return ext != "" && ext != "m3u8" && ext != "mpd" && ext != "html"
}

// Content families: whether a name and the bytes behind it agree is judged per
// family (an .mp4 name on MP3 bytes is wrong; .mp4 on a QuickTime file is not).
const (
	famVideo    = "video"
	famAudio    = "audio"
	famImage    = "image"
	famArchive  = "archive"
	famProgram  = "program"
	famDocument = "document"
	famText     = "text"
)

var extFamilies = func() map[string]string {
	m := map[string]string{}
	add := func(fam string, exts ...string) {
		for _, e := range exts {
			m[e] = fam
		}
	}
	add(famVideo, "mp4", "m4v", "mkv", "webm", "mov", "avi", "flv", "f4v", "wmv", "mpg", "mpeg", "m2ts", "mts",
		"ts", "3gp", "3g2", "ogv", "vob", "asf", "rm", "rmvb", "divx")
	add(famAudio, "mp3", "m4a", "m4b", "aac", "flac", "wav", "ogg", "oga", "opus", "wma", "weba", "aiff", "aif",
		"ape", "wv", "mka", "amr", "alac")
	add(famImage, "jpg", "jpeg", "png", "gif", "webp", "bmp", "tiff", "tif", "ico", "heic", "heif", "avif",
		"psd", "jxl", "cr2", "nef", "arw", "dng")
	add(famArchive, "zip", "rar", "7z", "gz", "tgz", "bz2", "tbz2", "xz", "txz", "zst", "tar", "iso", "img",
		"cab", "lz", "lzma", "lz4", "zipx", "arj", "vhd", "vhdx", "vmdk", "ova", "wim", "esd")
	add(famProgram, "exe", "msi", "msix", "msixbundle", "appx", "appxbundle", "apk", "xapk", "apks", "dmg",
		"pkg", "deb", "rpm", "appimage", "jar", "dll", "sys", "ipa", "crx", "xpi", "vsix", "nupkg", "whl")
	add(famDocument, "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "ods", "odp", "epub", "mobi",
		"azw3", "cbz", "cbr", "chm", "djvu")
	add(famText, "txt", "csv", "json", "xml", "html", "htm", "md", "log", "srt", "vtt", "ass", "ssa", "m3u",
		"m3u8", "mpd", "svg", "rtf", "ini", "cfg", "conf", "yml", "yaml", "toml", "js", "css", "py", "sh",
		"bat", "cmd", "ps1", "nfo", "sfv", "md5", "sha1", "sha256", "sha512", "asc", "sig", "sql", "tex")
	return m
}()

// extFamily returns the content family of a file extension ("" = unknown).
func extFamily(ext string) string { return extFamilies[strings.ToLower(ext)] }

// binaryExt reports extensions whose files are never text, so a text/HTML
// answer for them can only be an error or a landing page. (".ts" is left out:
// it is TypeScript as often as MPEG-TS.)
func binaryExt(ext string) bool {
	f := extFamily(ext)
	return f != "" && f != famText && ext != "ts"
}

// strictExt reports binary types an HTML answer can't be a video page for
// either: archives, programs, documents, images. (A media link answering with
// a player page is worth handing to yt-dlp.)
func strictExt(ext string) bool {
	switch extFamily(ext) {
	case famArchive, famProgram, famDocument, famImage:
		return true
	}
	return false
}
