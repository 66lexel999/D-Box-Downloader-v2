// D BOX Integration - pure helpers shared by the service worker
// (importScripts), the content script, the toolbar popup and the node unit
// tests (extension/test/helpers.test.js). Nothing here touches chrome.* or the
// DOM: every function is a pure function of its arguments, so the rules that
// decide what counts as a video, what is junk, how duplicates collapse and how
// titles are cleaned can be tested without a browser.
(function (root) {
  "use strict";

  // ---- media classification ------------------------------------------------
  // STREAM manifests go to D BOX as streams (HLS natively, DASH/ISM via yt-dlp);
  // SEG are pieces of a stream and are never offered on their own; SUB are
  // subtitle files; everything else in MEDIA_EXTS is a progressive file.
  const STREAM_EXTS = new Set(["m3u8", "mpd", "ism", "f4m"]);
  const SEG_EXTS = new Set(["ts", "m4s", "cmfv", "cmfa", "m4f", "f4f"]);
  const SUB_EXTS = new Set(["srt", "vtt", "ass"]);
  const AUDIO_EXTS = new Set(["mp3", "m4a", "aac", "ogg", "oga", "opus", "wav", "flac", "wma", "mka", "m4b"]);
  const VIDEO_EXTS = new Set(["mp4", "m4v", "webm", "mkv", "mov", "flv", "avi", "wmv", "mpg", "mpeg", "3gp", "ogv",
    "m2ts", "mts", "vob", "rmvb"]);
  const MEDIA_EXTS = new Set([...STREAM_EXTS, ...SEG_EXTS, ...SUB_EXTS, ...AUDIO_EXTS, ...VIDEO_EXTS]);

  // webRequest resource types that can carry a player's media. Scripts,
  // images, stylesheets, fonts and frames never do (a ".ts" script is
  // TypeScript, not an MPEG-TS segment).
  const MEDIA_TYPES = new Set(["media", "xmlhttprequest", "other", "object"]);

  function extOf(name) {
    const m = /\.([a-z0-9]{1,8})$/i.exec(String(name || ""));
    return m ? m[1].toLowerCase() : "";
  }

  // pathExt returns the media extension of a URL's path ("" when none), with
  // Smooth Streaming's ".ism/Manifest" (and its format=m3u8/mpd variants)
  // recognized even though the path doesn't end in an extension.
  function pathExt(url) {
    let path;
    try { path = decodeURIComponent(new URL(url).pathname); } catch (_) { return ""; }
    if (/\.isml?\/manifest/i.test(path)) {
      if (/format=m3u8/i.test(path)) return "m3u8";
      if (/format=mpd/i.test(path)) return "mpd";
      return "ism";
    }
    const e = extOf(path.replace(/\/+$/, ""));
    return MEDIA_EXTS.has(e) ? e : "";
  }

  const CT_EXT = {
    "application/vnd.apple.mpegurl": "m3u8", "application/x-mpegurl": "m3u8", "audio/mpegurl": "m3u8",
    "audio/x-mpegurl": "m3u8", "application/mpegurl": "m3u8", "video/mpegurl": "m3u8", "video/x-mpegurl": "m3u8",
    "application/dash+xml": "mpd", "video/vnd.mpeg.dash.mpd": "mpd",
    "application/vnd.ms-sstr+xml": "ism", "application/f4m+xml": "f4m", "application/f4m": "f4m",
    "video/mp2t": "ts", "audio/mp2t": "ts", "video/iso.segment": "m4s", "audio/iso.segment": "m4s",
    "video/x-flv": "flv", "video/flv": "flv", "video/webm": "webm", "audio/webm": "webm",
    "video/quicktime": "mov", "video/x-matroska": "mkv", "video/matroska": "mkv", "audio/x-matroska": "mka",
    "video/x-msvideo": "avi", "video/avi": "avi", "video/x-ms-wmv": "wmv", "video/3gpp": "3gp",
    "video/ogg": "ogv", "video/mpeg": "mpg", "video/mp4": "mp4", "video/x-m4v": "m4v",
    "audio/mpeg": "mp3", "audio/mp3": "mp3", "audio/mpeg3": "mp3", "audio/x-mpeg": "mp3",
    "audio/mp4": "m4a", "audio/x-m4a": "m4a", "audio/m4a": "m4a", "audio/aac": "aac", "audio/aacp": "aac",
    "audio/x-aac": "aac", "audio/ogg": "ogg", "audio/vorbis": "ogg", "audio/opus": "opus",
    "audio/wav": "wav", "audio/x-wav": "wav", "audio/wave": "wav", "audio/flac": "flac", "audio/x-flac": "flac",
    "audio/x-ms-wma": "wma", "text/vtt": "vtt", "application/x-subrip": "srt", "text/srt": "srt",
  };

  function baseCT(ct) { return String(ct || "").split(";")[0].trim().toLowerCase(); }

  // ctExt maps a Content-Type to a media extension ("" for non-media types).
  function ctExt(ct) {
    const c = baseCT(ct);
    if (!c) return "";
    if (CT_EXT[c]) return CT_EXT[c];
    if (c.includes("mpegurl")) return "m3u8";
    if (c.startsWith("video/")) return "mp4";
    if (c.startsWith("audio/")) return "m4a";
    return "";
  }

  // Types that say nothing about the body: the URL decides.
  const GENERIC_CT = new Set(["", "application/octet-stream", "binary/octet-stream", "application/binary",
    "application/force-download", "application/x-download", "application/download", "application/unknown",
    "text/plain", "application/x-unknown-content-type"]);
  // Types that say "definitely not media" (an error page, a script, a picture).
  function nonMediaCT(c) {
    return c === "text/html" || c === "text/css" || c.includes("javascript") || c.includes("ecmascript") ||
      c.startsWith("image/") || c.startsWith("font/") || c.includes("font-") || c === "application/wasm";
  }

  function kindOf(ext) {
    if (STREAM_EXTS.has(ext)) return "hls";
    if (SUB_EXTS.has(ext)) return "sub";
    if (SEG_EXTS.has(ext)) return "seg";
    return "file";
  }
  const isAudioExt = (ext) => AUDIO_EXTS.has(ext);

  // classify decides what a finished response is: {ext, kind, extFrom} or
  // null when it isn't media. extFrom records where the ext came from ("ct",
  // "url" or "guess") so a later, better source can upgrade it.
  //   - A strong media Content-Type wins over the URL, except that a URL that
  //     names a segment (.m4s served as video/mp4) stays a segment, and a URL
  //     with a more specific extension of the same family keeps it (.mkv as
  //     video/x-matroska vs a generic video/mp4).
  //   - A generic type (octet-stream, text/plain) defers to the URL.
  //   - A <video>/<audio> load with nothing telling is a guessed "mp4".
  function classify(o) {
    if (!o || !MEDIA_TYPES.has(o.type)) return null;
    const u = pathExt(o.url);
    const c = baseCT(o.ct);
    const x = ctExt(c);
    let ext = "", from = "";
    if (x) {
      if (u && SEG_EXTS.has(u) && !STREAM_EXTS.has(x)) { ext = u; from = "url"; }
      else if (u && u !== x && !SEG_EXTS.has(x) && !STREAM_EXTS.has(x) && famOfExt(u) === famOfExt(x)) { ext = u; from = "url"; }
      else { ext = x; from = "ct"; }
    } else if (u) {
      if (nonMediaCT(c) && !STREAM_EXTS.has(u)) return null; // a 200 "not found" page at a .mp4 address
      if (c.startsWith("image/")) return null;
      ext = u; from = "url";
    } else if (o.type === "media" && (GENERIC_CT.has(c))) {
      ext = "mp4"; from = "guess";
    }
    if (!ext) return null;
    return { ext, kind: kindOf(ext), extFrom: from };
  }

  const EXT_RANK = { guess: 0, url: 1, ct: 2, hook: 3 };
  // upgradeExt reports whether a new classification should replace the
  // stored one: a guess is replaced by anything, a plain file becomes a stream
  // when the body/type says so, and a Content-Type beats a URL guess.
  function upgradeExt(old, nu) {
    if (!old || !nu || !nu.ext || old.ext === nu.ext && old.kind === nu.kind) return false;
    if (nu.kind === "hls" && old.kind !== "hls") return true;
    if (old.kind === "hls" && nu.kind !== "hls") return false;
    return (EXT_RANK[nu.extFrom] || 0) > (EXT_RANK[old.extFrom] || 0);
  }

  // parseSize returns the whole resource's size in bytes (0 = unknown): the
  // total from Content-Range for a ranged 206 (a player's chunk request says
  // nothing about the file's size in Content-Length), else Content-Length —
  // unless the body is compressed, where Content-Length is the wire size.
  function parseSize(contentLength, contentRange, contentEncoding) {
    const cr = String(contentRange || "").trim();
    const m = /\/\s*(\d+)\s*$/.exec(cr);
    if (m) return +m[1] || 0;
    if (cr) return 0; // "bytes 0-1023/*": the total is unknown, the length is one chunk
    const ce = String(contentEncoding || "").trim().toLowerCase();
    if (ce && ce !== "identity") return 0;
    const n = parseInt(contentLength, 10);
    return n > 0 ? n : 0;
  }

  // ---- URL normalization ---------------------------------------------------
  // PARTIAL params select a piece of the resource (a byte range, an LL-HLS
  // blocking reload); dropped from the stored URL so D BOX fetches the whole.
  const PARTIAL_PARAMS = /^(range|bytestart|byteend|_hls_msn|_hls_part|_hls_skip|_hls_push)$/i;
  // VOLATILE params change on every request (cache busters) without changing
  // the resource; dropped from the dedup key only (a signed URL may need them).
  const VOLATILE_PARAMS = /^(_|cb|cachebust|cachebuster|cache_buster|cache-buster|nocache|no_cache|rnd|random|rand|rn|rbuf|__t|_t)$/i;
  const TIMESTAMP_PARAMS = /^(t|ts|time|timestamp|v|ver|nonce)$/i;

  // stripParams edits the raw query string pair by pair — never re-encoding
  // it through URLSearchParams, which would turn a signed CDN token's "~" or
  // "/" into %7E/%2F and break its signature.
  function stripParams(url, dropKey) {
    let u;
    try { u = new URL(url); } catch (_) { return String(url || "").split("#")[0]; }
    u.hash = "";
    if (!u.search) return u.href;
    const keep = u.search.slice(1).split("&").filter((pair) => {
      if (!pair) return false;
      const eq = pair.indexOf("=");
      let k = eq < 0 ? pair : pair.slice(0, eq), v = eq < 0 ? "" : pair.slice(eq + 1);
      try { k = decodeURIComponent(k.replace(/\+/g, " ")); } catch (_) {}
      if (PARTIAL_PARAMS.test(k)) return false;
      if (dropKey && (VOLATILE_PARAMS.test(k) || (TIMESTAMP_PARAMS.test(k) && /^\d{10,13}(\.\d+)?$/.test(v)))) return false;
      return true;
    });
    return u.origin + u.pathname + (keep.length ? "?" + keep.join("&") : "");
  }
  // cleanMediaUrl is the URL to hand to D BOX (no fragment, no range params).
  const cleanMediaUrl = (url) => stripParams(url, false);
  // normKey is the dedup key: a live playlist's _HLS_msn reloads, a CDN's
  // bytestart/byteend chunks and cache-busted requests all collapse to one.
  const normKey = (url) => stripParams(url, true);

  function hostOf(url) {
    try { return new URL(url).hostname.toLowerCase(); } catch (_) { return ""; }
  }
  function dirOf(url) {
    try { const u = new URL(url); return u.host + u.pathname.replace(/[^/]*$/, ""); } catch (_) { return ""; }
  }
  function baseName(url) {
    try { return decodeURIComponent(new URL(url).pathname.split("/").pop() || ""); } catch (_) { return ""; }
  }

  // ---- junk / ad / tiny filtering ------------------------------------------
  const JUNK_HOSTS = /(^|\.)(imasdk\.googleapis\.com|doubleclick\.net|googlesyndication\.com|googleadservices\.com|googletagservices\.com|2mdn\.net|adnxs\.com|amazon-adsystem\.com|moatads\.com|serving-sys\.com|flashtalking\.com|adsrvr\.org|spotxchange\.com|spotx\.tv|teads\.tv|innovid\.com|fwmrm\.net|tremorhub\.com|smartadserver\.com|adform\.net|pubmatic\.com|rubiconproject\.com|criteo\.com|taboola\.com|outbrain\.com|springserve\.com|aniview\.com|vidoomy\.com|adcolony\.com|unityads\.unity3d\.com|jwpltx\.com|scorecardresearch\.com|google-analytics\.com)$/i;
  const JUNK_PATHS = /\/s\/search\/audio\/|\/(sounds?|sfx|notification[-_]?sounds?|ui[-_]?sounds?)\/|\/vast\/|vast\.xml|[?&]vast=|\/ads?\/|\/adserver|\/pre-?roll|\/mid-?roll|[-_]pre-?roll[-_.]/i;
  // isJunkUrl: ad servers, ad VAST/creative paths and UI sound effects. These
  // are dropped outright — they're never what the user means by "the video".
  function isJunkUrl(url) {
    const h = hostOf(url);
    if (!h) return true;
    if (JUNK_HOSTS.test(h)) return true;
    let p = "";
    try { const u = new URL(url); p = u.pathname + u.search; } catch (_) {}
    return JUNK_PATHS.test(p);
  }
  // isDemotedUrl: hover previews, thumbnails, sprites and storyboards are real
  // media but rarely the main video; ranked last instead of dropped.
  const DEMOTED = /(^|[/_.-])(previews?|thumbs?|thumbnails?|sprites?|storyboards?|hover|vidthumb|preview[-_]?clip)([/_.-]|$)/i;
  function isDemotedUrl(url) {
    try { return DEMOTED.test(new URL(url).pathname); } catch (_) { return false; }
  }
  // isTiny: a known size too small to be a real video (< 512 KB) or audio
  // track (< 100 KB) — UI sounds, blank clips, ad beacons. Streams never are.
  function isTiny(it) {
    if (!it || it.kind === "hls" || !(it.size > 0)) return false;
    const audio = isAudioExt(it.ext) || /^audio\//i.test(it.ct || "");
    return it.size < (audio ? 100 * 1024 : 512 * 1024);
  }

  // isSegmentName: file names that HLS/DASH packagers give segments.
  function isSegmentName(name) {
    return /(^|[^a-z])(seg(ment)?|chunk|frag(ment)?|init|part)[-_]?(\d+|stream\d*)?([-_.]|$)/i.test(name) ||
      /^\d+\.[a-z0-9]{2,4}$/i.test(name) ||
      /[-_]\d{1,6}\.(mp4|m4s|m4a|m4v|aac|webm|ts|cmf[va]|jpe?g|png|html?|js|css|txt|webp|gif)$/i.test(name);
  }

  // looksLikeSegmentOf decides whether a "file" response is really a piece of a
  // stream the same frame already loaded a manifest for: same host and a
  // directory under the manifest's, or a packager-style segment name. A large
  // complete (non-ranged) response is a file in its own right.
  function looksLikeSegmentOf(o, manifestUrls) {
    if (!manifestUrls || !manifestUrls.length) return false;
    if (!o.partial && o.size >= 25 * 1024 * 1024) return false;
    if (isSegmentName(baseName(o.url))) return true;
    const d = dirOf(o.url);
    return manifestUrls.some((m) => { const md = dirOf(m); return md && d.startsWith(md); });
  }

  // isXhrSegment: media fetched by a script (XHR/fetch) is how MSE players
  // (hls.js, dash.js, shaka) load segments. Only a big whole resource with no
  // manifest in the frame is a real file; FLV (flv.js) is a whole stream.
  function isXhrSegment(o, hasManifest) {
    if (o.type !== "xmlhttprequest" || o.kind !== "file") return false;
    if (o.ext === "flv") return false;
    if (hasManifest) return true;
    return !(o.size >= 8 * 1024 * 1024);
  }

  // ---- ranking ---------------------------------------------------------------
  const KIND_SCORE = { hls: 300, file: 200, sub: 0 };

  // rankMedia filters a tab's captures down to what a person would pick from
  // and orders it best-first:
  //   - segments, variants/renditions of a known master and empty subtitle
  //     placeholders are hidden; a frame that loaded a master.m3u8 hides its
  //     other (unconfirmed) playlists;
  //   - tiny clips are hidden when anything bigger exists;
  //   - order: the playing URL, the asking frame, streams before files before
  //     subtitles, confirmed network hits before page-data hints, masters,
  //     duration matching the <video>, then bigger and newer first.
  // opts: {frameId, preferKey, mse, duration, children: Set|Array, navTs}
  function rankMedia(items, opts) {
    opts = opts || {};
    const children = opts.children instanceof Set ? opts.children : new Set(opts.children || []);
    let list = (items || []).filter((it) => it && it.kind !== "seg" && !children.has(it.key) && !isJunkUrl(it.url) &&
      !(it.kind === "sub" && /(^|\/)empty\.(srt|vtt)/i.test(it.url)));
    const masterFrames = new Set(list.filter((it) => it.kind === "hls" && it.ext === "m3u8" &&
      (it.master === true || /master[^/]*\.m3u8/i.test(it.url))).map((it) => it.frameId));
    list = list.filter((it) => !(it.kind === "hls" && it.ext === "m3u8" && masterFrames.has(it.frameId) &&
      it.master !== true && !/master[^/]*\.m3u8/i.test(it.url)));
    const big = list.some((it) => (it.kind === "hls" || it.kind === "file") && !isTiny(it) && !isDemotedUrl(it.url));
    if (big) list = list.filter((it) => !isTiny(it));
    const score = (it) => {
      let s = KIND_SCORE[it.kind] || 0;
      if (opts.preferKey && it.key === opts.preferKey) s += 5000;
      if (opts.frameId != null && it.frameId === opts.frameId) s += 1000;
      if (it.via !== "json") s += 20;
      if (it.master) s += 50;
      if (isDemotedUrl(it.url)) s -= 250;
      if (opts.navTs && (it.ts || 0) < opts.navTs) s -= 400;
      if (opts.mse && it.kind === "hls" && it.frameId === opts.frameId) s += 100;
      if (opts.duration > 0 && it.duration > 0 && Math.abs(it.duration - opts.duration) / opts.duration < 0.05) s += 150;
      return s;
    };
    return list.map((it) => ({ it, s: score(it) }))
      .sort((a, b) => (b.s - a.s) || ((b.it.size || 0) - (a.it.size || 0)) || ((b.it.ts || 0) - (a.it.ts || 0)))
      .map((x) => x.it);
  }

  // ---- titles ----------------------------------------------------------------
  const GENERIC_TITLES = new Set(["video", "videos", "player", "embed", "watch", "index", "master", "playlist",
    "untitled", "video player", "jw player", "jwplayer", "media player", "html5 video", "loading", "loading...",
    "about:blank", "home", "document", "stream", "live", "movie", "file", "download", "chunklist", "manifest",
    "videoplayback", "blank", "error", "404", "not found", "just a moment...", "attention required!"]);
  // Common second-level labels that aren't the site's name (co.uk, com.br…).
  const SLD = new Set(["co", "com", "net", "org", "gov", "edu", "ac", "or", "ne", "go"]);

  function siteLabels(host) {
    const parts = String(host || "").toLowerCase().replace(/^www\d*\./, "").split(".").filter(Boolean);
    if (parts.length <= 1) return parts;
    parts.pop(); // TLD
    if (parts.length > 1 && SLD.has(parts[parts.length - 1])) parts.pop();
    return parts.filter((p) => p.length >= 3);
  }
  const letters = (s) => String(s || "").toLowerCase().replace(/[^\p{L}\p{N}]+/gu, "");

  // matchesSite reports whether a title fragment is just the site's name: the
  // host's label (byse.sx -> "byse"), a declared og:site_name, or a domain.
  function matchesSite(frag, host, siteNames) {
    const f = letters(frag);
    if (!f) return false;
    if (/^[a-z0-9-]+(\.[a-z0-9-]+)+$/i.test(String(frag).trim())) return true; // "example.com"
    for (const raw of siteLabels(host)) {
      const l = letters(raw); // "arabic-toons" -> "arabictoons", as the fragment is compared
      if (!l) continue;
      if (f === l || (l.length >= 4 && f.includes(l) && f.length <= l.length + 12) || (f.length >= 4 && l.includes(f))) return true;
    }
    for (const n of siteNames || []) {
      const ln = letters(n);
      if (ln && (f === ln || (ln.length >= 4 && (f.includes(ln) && f.length <= ln.length + 12)))) return true;
    }
    return false;
  }

  const SEPS = [" | ", " - ", " – ", " — ", " :: ", " · ", " • ", " « ", " » ", " ~ ", " / "];

  // cleanTitle strips what isn't the video's name: notification counters
  // "(3) ", play glyphs, "Watch"/"شاهد"/"مشاهدة" prefixes, a trailing media
  // extension, and a " | Site" / " - Site" suffix (or "Site | " prefix) when the
  // fragment is the site's own name.
  function cleanTitle(title, host, siteNames) {
    let t = String(title || "").replace(/[\u0000-\u001f\u200b-\u200f\u202a-\u202e\u2066-\u2069\ufeff]/g, " ").replace(/\s+/g, " ").trim();
    t = t.replace(/^\(\d+\+?\)\s*/, "").replace(/^[▶►▷⏵•●◉🔴]+\s*/u, "");
    t = t.replace(/\s+-\s+YouTube$/i, "");
    for (let pass = 0; pass < 3; pass++) {
      let changed = false;
      for (const sep of SEPS) {
        const i = t.lastIndexOf(sep);
        if (i > 0 && matchesSite(t.slice(i + sep.length), host, siteNames)) { t = t.slice(0, i).trim(); changed = true; break; }
        const j = t.indexOf(sep);
        if (j > 0 && matchesSite(t.slice(0, j), host, siteNames)) { t = t.slice(j + sep.length).trim(); changed = true; break; }
      }
      if (!changed) break;
    }
    t = t.replace(/^(watch(\s+online)?|download|stream|شاهد|مشاهدة|تحميل|تنزيل)\s*[:\-–—]?\s+/i, "");
    t = t.replace(/\.(mp4|mkv|webm|m3u8|mpd|mov|avi|flv|ts|m4v|mp3|m4a)$/i, "");
    t = t.replace(/^[\s|:\-–—·•]+|[\s|:\-–—·•]+$/g, "").trim();
    return t.length > 200 ? t.slice(0, 200).trim() : t;
  }

  // isIdLikeTitle rejects titles that are really identifiers: generic words
  // ("video", "master"), host names, hashes, random embed codes
  // ("4172q6dt4qljs4kfrvr7") and tokens that appear in the media URL
  // ("pZxWPRg4" from .../pZxWPRg4.m3u8).
  function isIdLikeTitle(title, urls, host) {
    const t = String(title || "").trim();
    if (t.length < 2) return true;
    const low = t.toLowerCase();
    if (GENERIC_TITLES.has(low)) return true;
    if (host && (low === host || siteLabels(host).includes(low))) return true;
    if (/^[a-f0-9]{16,}$/i.test(t)) return true;
    if (/^[a-z0-9]{8,}$/i.test(t)) {
      // Random embed codes interleave letters and digits ("4172q6dt4qljs4kfrvr7");
      // real one-word names rarely switch more than once ("Episode12").
      const digits = (t.match(/\d/g) || []).length;
      const switches = (t.match(/[a-z](?=\d)|\d(?=[a-z])/gi) || []).length;
      if (digits >= 2 && switches >= 2) return true;
      if (t.length >= 16 && /[a-z]/.test(t) && /[A-Z]/.test(t)) return true;
    }
    if (/^[a-z]{12,}$/.test(t) && !/[aeiou]{1}[^aeiou]{0,3}[aeiou]/.test(t)) return true; // consonant soup
    if (/^[\w-]{6,}$/.test(t)) {
      for (const u of urls || []) {
        let p = "";
        try { const x = new URL(u); p = x.pathname + " " + x.search; } catch (_) { continue; }
        const toks = p.split(/[/?&=.\s_-]+/).filter(Boolean);
        if (toks.includes(t)) return true;
      }
    }
    return false;
  }

  // pickTitle returns the first acceptable cleaned title from candidates in
  // priority order (strings or {text}). "" when none is usable.
  function pickTitle(cands, o) {
    o = o || {};
    for (const c of cands || []) {
      const obj = c && typeof c === "object";
      const raw = obj ? c.text : c;
      if (!raw) continue;
      // A candidate may carry its own frame's host / site names (an iframe
      // player's title vs the embedding page's).
      const host = (obj && c.host) || o.host;
      const names = (obj && c.siteNames) || o.siteNames;
      const t = cleanTitle(raw, host, names);
      if (t && !isIdLikeTitle(t, o.urls, host)) return t;
    }
    return "";
  }

  // customHeaders keeps the request headers worth replaying from a
  // webRequest header list: Authorization and site-specific X-* style
  // headers. Browser-controlled and standard headers are dropped (D BOX sends
  // its own), as are cookies (sent separately, from chrome.cookies).
  const HDR_SKIP = /^(accept(-.*)?|content-(type|length|encoding)|range|cookie2?|referer|origin|user-agent|connection|host|te|trailer|upgrade|via|keep-alive|expect|date|dnt|priority|cache-control|pragma|if-.*|sec-.*|proxy-.*|upgrade-insecure-requests|x-client-data|x-requested-with|purpose|service-worker.*)$/i;
  function customHeaders(list) {
    const out = {};
    let n = 0;
    for (const h of list || []) {
      const k = String((h && h.name) || "").trim();
      const v = String((h && h.value) || "");
      if (!k || HDR_SKIP.test(k) || v.length > 4096 || n >= 12) continue;
      out[k] = v; n++;
    }
    return out;
  }

  // isPartialUrl: the address itself selects a byte range (a CDN's
  // range=/bytestart= chunking), so the response is a piece of the file.
  function isPartialUrl(url) {
    try {
      for (const k of new URL(url).searchParams.keys()) if (/^(range|bytestart|byteend)$/i.test(k)) return true;
    } catch (_) {}
    return false;
  }

  // Title candidate sources, best first (see content.js collectTitles).
  const TITLE_ORDER = ["player", "jsonld", "og", "twitter", "doc", "h1", "video"];
  function orderTitleCands(cands) {
    const rank = (s) => { const i = TITLE_ORDER.indexOf(s); return i < 0 ? 99 : i; };
    return (cands || []).slice().sort((a, b) => rank(a.src) - rank(b.src));
  }

  // ---- download hand-off naming ---------------------------------------------
  const ARCHIVE_EXTS = new Set(["zip", "rar", "7z", "tar", "gz", "bz2", "xz", "zst", "tgz", "lz", "lzma", "zipx", "cab", "iso", "img"]);
  const PROGRAM_EXTS = new Set(["exe", "msi", "apk", "dmg", "deb", "rpm", "appimage", "msix", "msixbundle", "appx", "jar", "bin", "pkg"]);
  function famOfExt(ext) {
    ext = String(ext || "").toLowerCase();
    if (VIDEO_EXTS.has(ext)) return "video";
    if (AUDIO_EXTS.has(ext)) return "audio";
    if (ARCHIVE_EXTS.has(ext) || /^(\d{3}|r\d{2}|z\d{2})$/.test(ext)) return "archive";
    if (PROGRAM_EXTS.has(ext)) return "program";
    if (/^(jpe?g|png|gif|webp|bmp|avif|heic|tiff?)$/.test(ext)) return "image";
    if (/^(pdf|docx?|xlsx?|pptx?|odt|epub|txt|csv)$/.test(ext)) return "document";
    return "";
  }
  function famOfMime(mime) {
    const m = baseCT(mime);
    if (!m || GENERIC_CT.has(m)) return "";
    if (m.includes("mpegurl") || m.includes("dash+xml")) return "";
    if (m.startsWith("video/")) return "video";
    if (m.startsWith("audio/")) return "audio";
    if (m.startsWith("image/")) return "image";
    if (/zip|rar|7z|tar|gzip|bzip|x-xz|zstd|compressed|diskimage|iso9660/.test(m)) return "archive";
    if (/msdownload|msdos-program|x-msi|portable-executable|android\.package|java-archive|x-debian|x-rpm/.test(m)) return "program";
    if (m === "application/pdf" || m.startsWith("text/")) return "document";
    return "";
  }

  // handoffName decides which browser-suggested file name to send D BOX. A
  // generic server-script name ("download.php") is dropped; a name whose
  // extension contradicts the MIME type (or the URL's own media extension,
  // e.g. "clip.mp4" for a ".mp3" URL) is sent without its extension so D BOX
  // names the file from what it actually receives.
  function handoffName(name, mime, url) {
    name = String(name || "").split(/[\\/]/).pop().trim();
    if (!name) return "";
    const ext = extOf(name);
    const stem = ext ? name.slice(0, -(ext.length + 1)) : name;
    if (/^(php\d?|aspx?|jsp|cgi|html?|pl|py|do)$/.test(ext)) return /^(download|get|file|dl|index|view|attachment)$/i.test(stem) ? "" : stem;
    if (/^(download|file|attachment|dl|get)$/i.test(name)) return "";
    const fam = famOfExt(ext), mfam = famOfMime(mime);
    if (fam && mfam && fam !== mfam) return stem;
    let ue = "";
    try { ue = extOf(new URL(url).pathname); } catch (_) {}
    const ufam = famOfExt(ue);
    if (ue && ue !== ext && (fam === "video" || fam === "audio") && (ufam === "video" || ufam === "audio") && ufam !== fam) return stem;
    return name;
  }

  // ---- display ----------------------------------------------------------------
  function humanBytes(n) {
    if (!n || n < 0) return "";
    const u = ["B", "KB", "MB", "GB", "TB"]; let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return n.toFixed(i && n < 10 ? 1 : 0) + " " + u[i];
  }
  function fmtDuration(sec) {
    sec = Math.round(sec || 0);
    if (!(sec > 0)) return "";
    const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
    const p = (x) => String(x).padStart(2, "0");
    return h ? h + ":" + p(m) + ":" + p(s) : m + ":" + p(s);
  }

  // mediaLabel is the one-line description of a captured item used by the
  // overlay menu and the popup: "HLS stream · 1080p · 23:41" / "MP4 file ·
  // cdn.example.com".
  function mediaLabel(it) {
    const parts = [];
    const E = String(it.ext || "").toUpperCase();
    if (it.kind === "hls") parts.push(it.ext === "mpd" ? "DASH stream" : it.ext === "ism" ? "Smooth stream" : it.ext === "f4m" ? "HDS stream" : "HLS stream");
    else if (it.kind === "sub") parts.push(E + " subtitles" + (it.lang ? " (" + it.lang + ")" : ""));
    else parts.push((isAudioExt(it.ext) ? E + " audio" : E + " video") + (it.ext === "flv" && it.live ? " (live)" : ""));
    if (it.height) parts.push(it.height + "p");
    if (it.live) parts.push("LIVE");
    else if (it.duration) parts.push(fmtDuration(it.duration));
    if (it.drm) parts.push("DRM");
    const h = hostOf(it.url);
    if (h) parts.push(h.replace(/^www\./, ""));
    if (it.via === "json" || it.via === "player") parts.push("from page");
    return parts.join(" · ");
  }

  const api = {
    STREAM_EXTS, SEG_EXTS, SUB_EXTS, AUDIO_EXTS, VIDEO_EXTS, MEDIA_EXTS, MEDIA_TYPES,
    extOf, pathExt, ctExt, baseCT, kindOf, isAudioExt, classify, upgradeExt, parseSize,
    cleanMediaUrl, normKey, hostOf, dirOf, baseName,
    isJunkUrl, isDemotedUrl, isTiny, isSegmentName, looksLikeSegmentOf, isXhrSegment, rankMedia,
    customHeaders, isPartialUrl,
    siteLabels, matchesSite, cleanTitle, isIdLikeTitle, pickTitle, orderTitleCands,
    famOfExt, famOfMime, handoffName, humanBytes, fmtDuration, mediaLabel,
  };
  if (typeof module === "object" && module && module.exports) module.exports = api;
  root.DBoxHelpers = api;
})(typeof globalThis !== "undefined" ? globalThis : this);
