// D BOX page hook — runs in the page's own JavaScript world ("world": "MAIN",
// document_start, every frame), before any page script.
//
// The network observer in the service worker only sees URLs and headers. Many
// players on uncommon sites hide what they play: the manifest comes from an
// extension-less address as text/plain or octet-stream, or is wrapped in a
// JSON API answer, and plays through hls.js / dash.js / shaka on a blob: video.
// Those are invisible to URL/Content-Type rules. This hook watches what the
// page itself sees:
//   - fetch() and XMLHttpRequest responses: the first ~64 KB of text-like
//     bodies are checked for "#EXTM3U" (HLS), "<MPD" (DASH),
//     "<SmoothStreamingMedia" and an FLV header, and JSON bodies for
//     m3u8/mpd/mp4 addresses;
//   - player libraries (hls.js loadSource, shaka load, dash.js
//     attachSource/initialize, flv.js/mpegts createPlayer) and read-only
//     snapshots of video.js / JW Player playlists (which also give the title);
//   - URL.createObjectURL(MediaSource) to know the frame plays through MSE.
// Findings go to the extension's isolated content script over a private
// DOM-event channel (see the handshake below), which forwards them to the
// service worker.
//
// It must never break a page: every wrapper calls the original first-class
// function through a Proxy (so fetch.toString() still says [native code]),
// all of our own work is inside try/catch, bodies are only read from a clone
// and only up to MAX_PEEK bytes, and nothing is written to the page's globals.
(function () {
  "use strict";
  const MAX_PEEK = 64 * 1024;

  // ---- parsers (pure; exported to the node tests) ---------------------------
  function attrs(line) {
    const out = {};
    const re = /([A-Z0-9-]+)=("[^"]*"|[^,]*)/g;
    let m;
    while ((m = re.exec(line))) out[m[1]] = m[2].replace(/^"|"$/g, "");
    return out;
  }
  function resolve(u, base) {
    try { return new URL(u, base).href; } catch (_) { return ""; }
  }

  // parseM3U8 reads an HLS playlist: a master's variants (resolution,
  // bandwidth) and renditions, or a media playlist's duration / live state /
  // encryption. truncated = only a prefix was read (duration/live unknown).
  function parseM3U8(text, base, truncated) {
    const s = String(text || "").replace(/^﻿/, "");
    if (!/^\s*#EXTM3U/.test(s)) return null;
    const out = { type: "hls", master: false, variants: [], renditions: [], duration: 0, live: true, drm: false, height: 0, segments: 0 };
    let inf = null;
    for (const raw of s.split(/\r?\n/)) {
      const l = raw.trim();
      if (!l) continue;
      if (l.startsWith("#EXT-X-STREAM-INF:")) inf = attrs(l.slice(18));
      else if (l.startsWith("#EXT-X-MEDIA:")) {
        const a = attrs(l.slice(13));
        if (a.URI) out.renditions.push({ url: resolve(a.URI, base), type: a.TYPE || "", lang: a.LANGUAGE || "", name: a.NAME || "" });
      } else if (l.startsWith("#EXT-X-I-FRAME-STREAM-INF:")) {
        const a = attrs(l.slice(26));
        if (a.URI) out.renditions.push({ url: resolve(a.URI, base), type: "IFRAME" });
      } else if (l.startsWith("#EXTINF:")) {
        out.duration += parseFloat(l.slice(8)) || 0;
        out.segments++;
      } else if (l === "#EXT-X-ENDLIST" || /^#EXT-X-PLAYLIST-TYPE:\s*VOD/i.test(l)) {
        out.live = false;
      } else if (l.startsWith("#EXT-X-KEY:") || l.startsWith("#EXT-X-SESSION-KEY:")) {
        const a = attrs(l.slice(l.indexOf(":") + 1));
        const kf = (a.KEYFORMAT || "identity").toLowerCase();
        if (/^SAMPLE-AES/i.test(a.METHOD || "") && kf !== "identity") out.drm = true;
      } else if (!l.startsWith("#") && inf) {
        const r = /^(\d+)x(\d+)$/i.exec(inf.RESOLUTION || "");
        out.variants.push({
          url: resolve(l, base), bw: +inf.BANDWIDTH || 0, avg: +inf["AVERAGE-BANDWIDTH"] || 0,
          width: r ? +r[1] : 0, height: r ? +r[2] : 0, codecs: inf.CODECS || "",
        });
        inf = null;
      }
    }
    out.master = out.variants.length > 0;
    if (out.master) {
      out.height = out.variants.reduce((h, v) => Math.max(h, v.height || 0), 0);
      out.duration = 0;
      delete out.live;
    } else if (truncated) {
      out.duration = 0;
      delete out.live;
    }
    return out;
  }

  function isoDuration(s) {
    const m = /^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:([\d.]+)S)?)?$/.exec(String(s || "").trim());
    if (!m) return 0;
    return (+m[1] || 0) * 86400 + (+m[2] || 0) * 3600 + (+m[3] || 0) * 60 + (parseFloat(m[4]) || 0);
  }

  // parseMPD reads the few facts the menu shows from a DASH manifest.
  function parseMPD(text) {
    const s = String(text || "");
    const tag = /<MPD\b[^>]*>/i.exec(s);
    if (!tag) return null;
    const dm = /mediaPresentationDuration="([^"]+)"/i.exec(tag[0]);
    let height = 0;
    for (const m of s.matchAll(/\bheight="(\d+)"/gi)) height = Math.max(height, +m[1]);
    return {
      type: "dash", duration: dm ? isoDuration(dm[1]) : 0, live: /\btype="dynamic"/i.test(tag[0]), height,
      drm: /<ContentProtection\b[^>]*urn:uuid:(edef8ba9|9a04f079|94ce86fb|e2719d58)/i.test(s),
    };
  }

  // parseISM reads a Smooth Streaming manifest header.
  function parseISM(text) {
    const tag = /<SmoothStreamingMedia\b[^>]*>/i.exec(String(text || ""));
    if (!tag) return null;
    const a = (n) => { const m = new RegExp("\\b" + n + '="([^"]*)"', "i").exec(tag[0]); return m ? m[1] : ""; };
    const ts = +a("TimeScale") || 10000000;
    let height = 0;
    for (const m of String(text).matchAll(/\b(?:MaxHeight|Height)="(\d+)"/gi)) height = Math.max(height, +m[1]);
    return { type: "ism", duration: (+a("Duration") || 0) / ts, live: /^true$/i.test(a("IsLive")), height,
      drm: /<ProtectionHeader\b/i.test(text) };
  }

  const URL_RE = /https?:(?:\\?\/){2}[^\s"'<>(){}|^`]+?\.(m3u8|mpd|mp4|m4v|webm|mkv|mov|flv|mp3|m4a|vtt|srt)(?:\?[^\s"'<>{}|^`]*)?(?=["'\s<>,)}\]\\]|$)/gi;
  // scanMediaUrls finds media addresses in a JSON/text body ("\/" and
  // "/" escapes undone), deduped, in order.
  function scanMediaUrls(text) {
    const s = String(text || "").replace(/\\u002[fF]/g, "/").replace(/\\u0026/g, "&");
    const seen = new Set(), out = [];
    let m;
    URL_RE.lastIndex = 0;
    while ((m = URL_RE.exec(s)) && out.length < 40) {
      const u = m[0].replace(/\\\//g, "/").replace(/&amp;/g, "&").replace(/\\+$/, "");
      if (!seen.has(u)) { seen.add(u); out.push({ url: u, ext: m[1].toLowerCase() }); }
    }
    return out;
  }

  // analyzeText classifies a response prefix; returns a message or null.
  function analyzeText(text, url, ct, truncated) {
    const s = String(text || "").replace(/^﻿/, "");
    const head = s.slice(0, 2048).trimStart();
    if (head.startsWith("#EXTM3U")) {
      const p = parseM3U8(s, url, truncated);
      return p && Object.assign({ t: "manifest", url }, p);
    }
    if (/^(<\?xml[^>]*>\s*)?(<!--[\s\S]*?-->\s*)*<MPD\b/i.test(head)) {
      const p = parseMPD(s);
      return p && Object.assign({ t: "manifest", url }, p);
    }
    if (/^(<\?xml[^>]*>\s*)?<SmoothStreamingMedia\b/i.test(head)) {
      const p = parseISM(s);
      return p && Object.assign({ t: "manifest", url }, p);
    }
    const c = String(ct || "").toLowerCase();
    if (/^[[{]/.test(head) || c.includes("json") || (s.length < 4096 && /\.(m3u8|mpd)\b/i.test(s))) {
      const urls = scanMediaUrls(s);
      if (!urls.length) return null;
      // A feed of many clips (a "related videos" API) is not this player's
      // source list; keep only its stream manifests then.
      const media = urls.filter((x) => x.ext !== "vtt" && x.ext !== "srt");
      const list = media.length > 6 ? urls.filter((x) => x.ext === "m3u8" || x.ext === "mpd").slice(0, 3) : urls.slice(0, 12);
      return list.length ? { t: "json", url, urls: list } : null;
    }
    return null;
  }

  if (typeof process === "object" && process && process.versions && process.versions.node && typeof module === "object") {
    module.exports = { parseM3U8, parseMPD, parseISM, isoDuration, scanMediaUrls, analyzeText };
    return;
  }

  // ---- private channel to the isolated content script -------------------------
  // Handshake: we announce a random nonce on `document` ("dbox-hello") while no
  // page script has run yet; the content script (also document_start) answers
  // "dbox-ack-<nonce>" and listens on "dbox-<nonce>", an event type a page
  // can't guess. If the content script wasn't listening yet it pings
  // ("dbox-ping") and we announce again — only until acknowledged.
  const rnd = new Uint32Array(4);
  try { crypto.getRandomValues(rnd); } catch (_) { for (let i = 0; i < 4; i++) rnd[i] = Math.random() * 4294967296; }
  const nonce = Array.from(rnd, (x) => x.toString(36)).join("");
  const EVT = "dbox-" + nonce;
  let acked = false, queue = [];
  // Page scripts may later replace these globals (to snoop or break us):
  // keep the originals.
  const Custom = window.CustomEvent;
  const dispatchEv = EventTarget.prototype.dispatchEvent;
  const fire = (type, detail) => { try { dispatchEv.call(document, new Custom(type, { detail })); } catch (_) {} };
  const dispatch = (s) => fire(EVT, s);
  function post(msg) {
    let s;
    try { s = JSON.stringify(msg); } catch (_) { return; }
    if (!acked) { if (queue.length < 200) queue.push(s); return; }
    dispatch(s);
  }
  function hello() { fire("dbox-hello", nonce); }
  function onPing() { if (!acked) hello(); }
  try {
    document.addEventListener("dbox-ack-" + nonce, () => {
      if (acked) return;
      acked = true;
      document.removeEventListener("dbox-ping", onPing, true);
      const q = queue; queue = [];
      for (const s of q) dispatch(s);
    }, { once: true });
    document.addEventListener("dbox-ping", onPing, true);
  } catch (_) {}
  hello();

  // Report each distinct finding once per page.
  const reported = new Set();
  function report(msg, dedupKey) {
    const k = dedupKey || JSON.stringify(msg);
    if (reported.has(k)) return;
    if (reported.size > 2000) reported.clear();
    reported.add(k);
    post(msg);
  }
  const isHttp = (u) => typeof u === "string" && /^https?:\/\//i.test(u);

  // Custom request headers a player adds (hls.js xhrSetup, a fetch() init):
  // Authorization / X-Token style headers some CDNs require. Standard and
  // browser-controlled headers are left out (D BOX sets its own).
  const HDR_SKIP = /^(accept(-.*)?|content-(type|length|encoding)|range|cookie2?|referer|origin|user-agent|connection|host|te|trailer|upgrade|via|keep-alive|expect|date|dnt|priority|cache-control|pragma|if-.*|sec-.*|proxy-.*|x-requested-with)$/i;
  function addHdr(o, k, v) {
    k = String(k || "").trim(); v = String(v == null ? "" : v);
    if (!k || HDR_SKIP.test(k) || v.length > 4096 || Object.keys(o).length >= 12) return;
    o[k] = v;
  }
  function fetchHeaders(args) {
    const o = {};
    try {
      const add = (h) => {
        if (!h) return;
        if (Array.isArray(h)) { for (const p of h) if (p && p.length >= 2) addHdr(o, p[0], p[1]); }
        else if (typeof h.forEach === "function") h.forEach((v, k) => addHdr(o, k, v));
        else if (typeof h === "object") { for (const k of Object.keys(h)) addHdr(o, k, h[k]); }
      };
      const r = args[0];
      if (r && typeof r === "object" && r.headers) add(r.headers);
      if (args[1] && args[1].headers) add(args[1].headers);
    } catch (_) {}
    return o;
  }
  const withHdrs = (msg, h) => { if (msg && h && Object.keys(h).length) msg.headers = h; return msg; };
  const abs = (u) => { try { return new URL(String(u), location.href).href; } catch (_) { return ""; } };

  // ---- response peeking ---------------------------------------------------------
  // sniffMode decides from the headers alone whether a body is worth reading:
  // manifest types always; generic/text/JSON/XML types when small enough.
  // Media, images, fonts, scripts, styles and HTML are never read.
  function sniffMode(ct, url, len) {
    const c = String(ct || "").split(";")[0].trim().toLowerCase();
    if (/mpegurl|dash\+xml|vnd\.ms-sstr|f4m/.test(c)) return "manifest";
    if (c === "video/x-flv" || c === "video/flv") return "flv";
    if (/^(video|audio|image|font)\//.test(c) || /javascript|ecmascript|text\/css|text\/html|wasm|protobuf|grpc|x-www-form/.test(c)) return "";
    if (len > 2 * 1024 * 1024) return "";
    if (!c || /octet-stream|binary|text\/plain|json|xml|unknown|force-download/.test(c)) return "peek";
    return "";
  }

  function decode(bytes) {
    try { return new TextDecoder("utf-8").decode(bytes); } catch (_) { return ""; }
  }
  function analyzeBytes(bytes, url, ct, truncated, hdrs) {
    if (!bytes || !bytes.length) return;
    // MPEG-TS (sync byte every 188 bytes): a segment, never text.
    if (bytes[0] === 0x47 && (bytes.length < 189 || bytes[188] === 0x47)) return;
    // FLV header ("FLV" + version 1): an flv.js / mpegts.js stream.
    if (bytes[0] === 0x46 && bytes[1] === 0x4c && bytes[2] === 0x56 && bytes[3] === 0x01) {
      report({ t: "media", url, ext: "flv", live: truncated }, "flv " + url);
      return;
    }
    // Binary that isn't text (a segment, an image): stop here.
    const first = bytes[0] === 0xef && bytes[1] === 0xbb ? bytes[3] : bytes[0];
    if (first !== undefined && first < 0x09) return;
    analyzeString(decode(bytes), url, ct, truncated, hdrs);
  }
  function analyzeString(text, url, ct, truncated, hdrs) {
    try {
      const msg = withHdrs(analyzeText(text, url, ct, truncated), hdrs);
      if (!msg) return;
      if (msg.t === "manifest") report(msg, "m " + url + " " + (msg.master ? 1 : 0) + " " + msg.segments);
      else report(msg);
    } catch (_) {}
  }

  async function readPrefix(body, max) {
    const reader = body.getReader();
    const chunks = [];
    let n = 0, done = false;
    try {
      while (n < max) {
        const r = await reader.read();
        if (r.done) { done = true; break; }
        chunks.push(r.value);
        n += r.value.length;
      }
    } finally {
      try { reader.cancel(); } catch (_) {}
    }
    const out = new Uint8Array(Math.min(n, max));
    let o = 0;
    for (const c of chunks) {
      const take = Math.min(c.length, out.length - o);
      if (take <= 0) break;
      out.set(c.subarray(0, take), o);
      o += take;
    }
    return { bytes: out, truncated: !done };
  }

  function onFetchResponse(r, hdrs) {
    try {
      if (!r || r.type === "opaque" || r.type === "opaqueredirect" || !(r.status >= 200 && r.status < 300)) return;
      const url = r.url;
      if (!isHttp(url)) return;
      const ct = r.headers.get("content-type") || "";
      const mode = sniffMode(ct, url, +r.headers.get("content-length") || 0);
      if (!mode) return;
      if (mode === "flv") { report({ t: "media", url, ext: "flv", live: !r.headers.get("content-length") }, "flv " + url); return; }
      const c = r.clone();
      if (!c.body) return;
      readPrefix(c.body, MAX_PEEK).then((p) => analyzeBytes(p.bytes, url, ct, p.truncated, hdrs), () => {});
    } catch (_) {}
  }

  function wrap(obj, name, before, after) {
    try {
      const orig = obj && obj[name];
      if (typeof orig !== "function") return false;
      const p = new Proxy(orig, {
        apply(target, self, args) {
          if (before) { try { before(args, self); } catch (_) {} }
          const res = Reflect.apply(target, self, args);
          if (after) { try { after(res, args, self); } catch (_) {} }
          return res;
        },
      });
      obj[name] = p;
      return obj[name] === p;
    } catch (_) { return false; }
  }

  // fetch: look at the response the page receives (our reaction is attached
  // before the page's, so the clone happens before the page reads the body).
  wrap(window, "fetch", null, (p, args) => {
    if (p && typeof p.then === "function") {
      const h = fetchHeaders(args);
      p.then((r) => onFetchResponse(r, h), () => {});
    }
  });

  // XMLHttpRequest: remember the method/URL at open(), read the body on load.
  const xhrUrl = new WeakMap(), xhrHdr = new WeakMap();
  function onXhrLoad() {
    const x = this;
    try {
      if (!(x.status >= 200 && x.status < 300)) return;
      const url = x.responseURL || xhrUrl.get(x) || "";
      if (!isHttp(url)) return;
      const ct = x.getResponseHeader("content-type") || "";
      const mode = sniffMode(ct, url, +x.getResponseHeader("content-length") || 0);
      if (!mode) return;
      const rt = x.responseType, h = xhrHdr.get(x);
      if (rt === "" || rt === "text") {
        const t = x.responseText || "";
        analyzeString(t.length > MAX_PEEK ? t.slice(0, MAX_PEEK) : t, url, ct, t.length > MAX_PEEK, h);
      } else if (rt === "arraybuffer" && x.response) {
        const len = x.response.byteLength;
        analyzeBytes(new Uint8Array(x.response, 0, Math.min(len, MAX_PEEK)), url, ct, len > MAX_PEEK, h);
      } else if (rt === "json" && x.response && typeof x.response === "object") {
        analyzeString(boundedJSON(x.response), url, "application/json", false, h);
      } else if (rt === "blob" && x.response && x.response.size < 2 * 1024 * 1024) {
        const b = x.response;
        b.slice(0, MAX_PEEK).arrayBuffer().then((ab) => analyzeBytes(new Uint8Array(ab), url, ct, b.size > MAX_PEEK, h), () => {});
      }
    } catch (_) {}
  }
  // boundedJSON serializes at most ~MAX_PEEK characters of a parsed JSON
  // response (a responseType "json" XHR has no text to slice).
  function boundedJSON(obj) {
    let out = "", nodes = 0;
    const walk = (v, depth) => {
      if (out.length > MAX_PEEK || nodes++ > 5000 || depth > 12) return;
      if (typeof v === "string") { out += JSON.stringify(v) + ","; return; }
      if (!v || typeof v !== "object") return;
      out += Array.isArray(v) ? "[" : "{";
      for (const k in v) { if (Object.prototype.hasOwnProperty.call(v, k)) walk(v[k], depth + 1); }
      out += Array.isArray(v) ? "]" : "}";
    };
    try { walk(obj, 0); } catch (_) {}
    return "{" + out;
  }
  try {
    const XP = window.XMLHttpRequest && window.XMLHttpRequest.prototype;
    wrap(XP, "open", (args, self) => { xhrUrl.set(self, abs(args[1])); xhrHdr.delete(self); });
    wrap(XP, "setRequestHeader", (args, self) => {
      let o = xhrHdr.get(self);
      if (!o) { o = {}; xhrHdr.set(self, o); }
      addHdr(o, args[0], args[1]);
    });
    wrap(XP, "send", (args, self) => { if (xhrUrl.has(self)) self.addEventListener("load", onXhrLoad); });
  } catch (_) {}

  // URL.createObjectURL(MediaSource): this frame plays through MSE.
  let mseSent = false;
  try {
    const MS = window.MediaSource || window.ManagedMediaSource;
    wrap(window.URL, "createObjectURL", (args) => {
      if (!mseSent && MS && args[0] instanceof MS) { mseSent = true; post({ t: "mse" }); }
    });
  } catch (_) {}

  // ---- player libraries ------------------------------------------------------------
  function playerSource(lib, u, extra) {
    const url = abs(u);
    if (!isHttp(url)) return;
    report(Object.assign({ t: "player", lib, url }, extra || {}), "p " + url);
  }
  function playerTitle(lib, title) {
    title = String(title || "").trim();
    if (title) report({ t: "title", lib, title: title.slice(0, 300) }, "t " + title);
  }

  const patched = new WeakSet();
  function once(obj) { if (!obj || patched.has(obj)) return false; patched.add(obj); return true; }

  function patchLibs() {
    const w = window;
    try {
      const Hls = w.Hls;
      if (typeof Hls === "function" && Hls.prototype && once(Hls)) {
        wrap(Hls.prototype, "loadSource", (a) => playerSource("hls.js", a[0]));
      }
    } catch (_) {}
    try {
      const SP = w.shaka && w.shaka.Player;
      if (typeof SP === "function" && SP.prototype && once(SP)) {
        wrap(SP.prototype, "load", (a) => { if (typeof a[0] === "string") playerSource("shaka", a[0]); });
      }
    } catch (_) {}
    try {
      const dj = w.dashjs;
      if (dj && typeof dj.MediaPlayer === "function" && once(dj)) {
        wrap(dj, "MediaPlayer", null, (factory) => {
          wrap(factory, "create", null, (player) => {
            wrap(player, "attachSource", (a) => { if (typeof a[0] === "string") playerSource("dash.js", a[0]); });
            wrap(player, "initialize", (a) => { if (typeof a[1] === "string") playerSource("dash.js", a[1]); });
          });
        });
      }
    } catch (_) {}
    for (const name of ["flvjs", "mpegts"]) {
      try {
        const lib = w[name];
        if (lib && typeof lib.createPlayer === "function" && once(lib)) {
          wrap(lib, "createPlayer", (a) => { const m = a[0] || {}; if (m.url) playerSource(name, m.url, { ext: m.type === "flv" || /\.flv/i.test(m.url) ? "flv" : "", live: !!m.isLive }); });
        }
      } catch (_) {}
    }
  }

  // snapshotPlayers reads (never changes) the source lists and titles of
  // video.js and JW Player instances.
  function snapshotPlayers() {
    const w = window;
    try {
      const jw = w.jwplayer;
      if (typeof jw === "function") {
        for (let i = 0; i < 6; i++) {
          let p;
          try { p = jw(i); } catch (_) { break; }
          if (!p || typeof p.getPlaylistItem !== "function") break;
          const item = p.getPlaylistItem() || {};
          if (item.title) playerTitle("jwplayer", item.title);
          const srcs = (item.sources || []).concat(item.file ? [{ file: item.file }] : []);
          for (const s of srcs) if (s && s.file) playerSource("jwplayer", s.file, { label: s.label || "" });
          for (const t of item.tracks || []) {
            if (t && t.file && /captions|subtitles/i.test(t.kind || "")) playerSource("jwplayer", t.file, { sub: true, lang: t.label || "" });
          }
        }
      }
    } catch (_) {}
    try {
      const vjs = w.videojs;
      const players = vjs && typeof vjs.getPlayers === "function" ? vjs.getPlayers() : null;
      for (const k in players || {}) {
        const p = players[k];
        if (!p || typeof p.currentSrc !== "function") continue;
        const srcs = typeof p.currentSources === "function" ? p.currentSources() : [];
        for (const s of srcs || []) if (s && s.src) playerSource("videojs", s.src);
        const cur = p.currentSrc();
        if (cur) playerSource("videojs", cur);
        const mi = p.mediainfo; // Brightcove
        if (mi && mi.name) playerTitle("videojs", mi.name);
      }
    } catch (_) {}
  }

  function scan() { patchLibs(); snapshotPlayers(); }
  let ticks = 0;
  const iv = setInterval(() => { scan(); if (++ticks > 90) clearInterval(iv); }, 2000); // first 3 minutes
  try {
    document.addEventListener("DOMContentLoaded", scan, { once: true });
    window.addEventListener("load", scan, { once: true });
    let playT = 0;
    document.addEventListener("play", () => { clearTimeout(playT); playT = setTimeout(scan, 300); }, true);
  } catch (_) {}
})();
