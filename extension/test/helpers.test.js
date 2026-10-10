// Unit tests for the extension's pure helpers (lib/helpers.js) and the page
// hook's parsers (hook.js exports them when loaded by node).
// Run: node --test extension/test   (or: node extension/test/helpers.test.js)
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const H = require("../lib/helpers.js");
const K = require("../hook.js");

// ---- key normalization -------------------------------------------------------
test("normKey collapses LL-HLS reloads, byte ranges and cache busters", () => {
  const a = H.normKey("https://cdn.x/live/index.m3u8?_HLS_msn=120&_HLS_part=3&token=abc");
  const b = H.normKey("https://cdn.x/live/index.m3u8?token=abc&_HLS_msn=121");
  assert.equal(a, "https://cdn.x/live/index.m3u8?token=abc");
  assert.equal(a, b);
  assert.equal(H.normKey("https://v.fb.x/v.mp4?bytestart=0&byteend=999&oh=1"), "https://v.fb.x/v.mp4?oh=1");
  assert.equal(H.normKey("https://c.x/a.m3u8?_=1727712000123#frag"), "https://c.x/a.m3u8");
  assert.equal(H.normKey("https://c.x/a.m3u8?t=1727712000"), "https://c.x/a.m3u8");
  assert.equal(H.normKey("https://c.x/a.m3u8?t=5"), "https://c.x/a.m3u8?t=5"); // not a timestamp
  assert.equal(H.normKey("https://c.x/v.mp4?range=0-65535"), "https://c.x/v.mp4");
});

test("cleanMediaUrl keeps signed tokens byte-for-byte but drops range params", () => {
  const u = "https://cdn.x/p/v.mp4?Policy=a~b%2Fc&Signature=x_y~z&range=100-200";
  assert.equal(H.cleanMediaUrl(u), "https://cdn.x/p/v.mp4?Policy=a~b%2Fc&Signature=x_y~z");
  // cache busters survive in the hand-off URL (only the dedup key drops them)
  assert.equal(H.cleanMediaUrl("https://c.x/a.m3u8?_=123"), "https://c.x/a.m3u8?_=123");
  assert.ok(H.isPartialUrl("https://c.x/v.mp4?bytestart=1"));
  assert.ok(!H.isPartialUrl("https://c.x/v.mp4?start=1"));
});

// ---- segment detection -------------------------------------------------------
test("segments are never direct files", () => {
  assert.equal(H.classify({ type: "xmlhttprequest", url: "https://c.x/seg-12.ts", ct: "video/mp2t" }).kind, "seg");
  assert.equal(H.classify({ type: "xmlhttprequest", url: "https://c.x/a/b/12.jpg", ct: "video/mp2t" }).kind, "seg"); // disguised
  assert.equal(H.classify({ type: "xmlhttprequest", url: "https://c.x/x", ct: "video/iso.segment" }).kind, "seg");
  assert.equal(H.classify({ type: "xmlhttprequest", url: "https://c.x/v/chunk_5.m4s", ct: "video/mp4" }).kind, "seg");
  assert.equal(H.classify({ type: "xmlhttprequest", url: "https://c.x/v/5.cmfv", ct: "application/octet-stream" }).kind, "seg");
  assert.equal(H.classify({ type: "xmlhttprequest", url: "https://c.x/v/5.cmfa", ct: "" }).kind, "seg");
  // a TypeScript file is not an MPEG-TS segment
  assert.equal(H.classify({ type: "script", url: "https://c.x/app.ts", ct: "" }), null);
  // an HTML error page at a .mp4 address is not media
  assert.equal(H.classify({ type: "media", url: "https://c.x/v.mp4", ct: "text/html" }), null);
});

test("classify: content type upgrades, URL keeps a more specific ext", () => {
  assert.deepEqual(H.classify({ type: "xmlhttprequest", url: "https://c.x/api/source/xyz", ct: "application/vnd.apple.mpegurl" }),
    { ext: "m3u8", kind: "hls", extFrom: "ct" });
  assert.equal(H.classify({ type: "media", url: "https://c.x/play?id=5", ct: "audio/mpeg" }).ext, "mp3");
  assert.equal(H.classify({ type: "media", url: "https://c.x/play?id=5", ct: "" }).extFrom, "guess");
  assert.equal(H.classify({ type: "media", url: "https://c.x/film.mkv", ct: "video/mp4" }).ext, "mkv");
  assert.equal(H.classify({ type: "xmlhttprequest", url: "https://c.x/Manifest.ism/manifest", ct: "text/xml" }).ext, "ism");
  const old = { ext: "mp4", kind: "file", extFrom: "guess" };
  assert.ok(H.upgradeExt(old, { ext: "mp3", kind: "file", extFrom: "ct" }));
  assert.ok(H.upgradeExt(old, { ext: "m3u8", kind: "hls", extFrom: "ct" }));
  assert.ok(!H.upgradeExt({ ext: "m3u8", kind: "hls", extFrom: "url" }, { ext: "mp4", kind: "file", extFrom: "ct" }));
});

test("MSE xhr chunks and files under a manifest's directory are segments", () => {
  assert.ok(H.isXhrSegment({ type: "xmlhttprequest", kind: "file", ext: "mp4", size: 900000 }, false));
  assert.ok(H.isXhrSegment({ type: "xmlhttprequest", kind: "file", ext: "mp4", size: 90e6 }, true));
  assert.ok(!H.isXhrSegment({ type: "xmlhttprequest", kind: "file", ext: "mp4", size: 90e6 }, false)); // ranged progressive file
  assert.ok(!H.isXhrSegment({ type: "media", kind: "file", ext: "mp4", size: 1000 }, false));
  const m = ["https://cdn.x/hls/ep1/master.m3u8"];
  assert.ok(H.looksLikeSegmentOf({ url: "https://cdn.x/hls/ep1/720/v.mp4", size: 2e6 }, m));
  assert.ok(H.looksLikeSegmentOf({ url: "https://other.x/f/fileSequence12.mp4", size: 2e6 }, ["https://a.x/b.m3u8"]) === false);
  assert.ok(H.looksLikeSegmentOf({ url: "https://other.x/f/segment-12.mp4", size: 2e6 }, ["https://a.x/b.m3u8"]));
  assert.ok(!H.looksLikeSegmentOf({ url: "https://cdn.x/hls/ep1/full.mp4", size: 300e6 }, m));
});

// ---- sizes -----------------------------------------------------------------------
test("parseSize uses Content-Range totals and ignores compressed lengths", () => {
  assert.equal(H.parseSize("1048576", "bytes 0-1048575/73500000", ""), 73500000);
  assert.equal(H.parseSize("1048576", "bytes 0-1048575/*", ""), 0);
  assert.equal(H.parseSize("5000", "", "gzip"), 0);
  assert.equal(H.parseSize("5000", "", "identity"), 5000);
  assert.equal(H.parseSize("", "", ""), 0);
});

// ---- junk filter / ranking ---------------------------------------------------------
test("junk: ad hosts, UI sounds and VAST paths", () => {
  assert.ok(H.isJunkUrl("https://www.youtube.com/s/search/audio/no_input.mp3"));
  assert.ok(H.isJunkUrl("https://imasdk.googleapis.com/js/core/bridge.mp4"));
  assert.ok(H.isJunkUrl("https://s0.2mdn.net/videoplayback/ad.mp4"));
  assert.ok(H.isJunkUrl("https://tpc.googlesyndication.com/x.mp4"));
  assert.ok(H.isJunkUrl("https://ad.doubleclick.net/v.mp4"));
  assert.ok(H.isJunkUrl("https://site.x/assets/sounds/click.mp3"));
  assert.ok(!H.isJunkUrl("https://cdn.site.x/videos/episode-1.mp4"));
  assert.ok(!H.isJunkUrl("https://cdn.site.x/downloads/episode.mp4")); // "/ad" must be a whole segment
  assert.ok(H.isDemotedUrl("https://cdn.x/previews/clip.mp4"));
});

test("rankMedia hides tiny clips, segments and variants; best first", () => {
  const items = [
    { key: "a", url: "https://www.youtube.com/s/search/audio/no_input.mp3", ext: "mp3", kind: "file", size: 6953, frameId: 0, ts: 5 },
    { key: "b", url: "https://cdn.x/a/beep.m4a", ext: "m4a", kind: "file", size: 40000, frameId: 0, ts: 6 },
    { key: "c", url: "https://foupix.x/v/ep.mp4", ext: "mp4", kind: "file", size: 73.5e6, frameId: 3, ts: 1 },
    { key: "d", url: "https://intro.x/v/intro.mp4", ext: "mp4", kind: "file", size: 1.16e6, frameId: 0, ts: 9 },
    { key: "e", url: "https://c.x/seg1.ts", ext: "ts", kind: "seg", size: 1e6, frameId: 3, ts: 2 },
    { key: "f", url: "https://h.x/ep/master.m3u8", ext: "m3u8", kind: "hls", frameId: 3, ts: 3, master: true },
    { key: "g", url: "https://h.x/ep/1080p/playlist.m3u8", ext: "m3u8", kind: "hls", frameId: 3, ts: 4 },
    { key: "h", url: "https://h.x/ep/tiny.mp4", ext: "mp4", kind: "file", size: 200000, frameId: 3, ts: 4 },
  ];
  const out = H.rankMedia(items, { frameId: 3, children: ["g"] }).map((x) => x.key);
  assert.deepEqual(out, ["f", "c", "d"]);
  // nothing better: the tiny clip stays
  assert.deepEqual(H.rankMedia([items[1]], {}).map((x) => x.key), ["b"]);
  // the playing URL wins
  assert.equal(H.rankMedia(items, { frameId: 3, preferKey: "d" })[0].key, "d");
  // media playlists named without "master" are hidden by a hook-confirmed master in the same frame
  const named = [
    { key: "m", url: "https://h.x/index.m3u8", ext: "m3u8", kind: "hls", frameId: 1, master: true, ts: 1 },
    { key: "v", url: "https://h.x/720/index.m3u8", ext: "m3u8", kind: "hls", frameId: 1, ts: 2 },
  ];
  assert.deepEqual(H.rankMedia(named, {}).map((x) => x.key), ["m"]);
});

// ---- titles --------------------------------------------------------------------------
test("cleanTitle strips site suffixes, prefixes, counters and extensions", () => {
  assert.equal(H.cleanTitle("Episode 5 | Arabic Toons", "www.arabic-toons.com"), "Episode 5");
  assert.equal(H.cleanTitle("(3) My Cat - YouTube", "www.youtube.com"), "My Cat");
  assert.equal(H.cleanTitle("Byse - The Movie Night", "byse.sx"), "The Movie Night");
  assert.equal(H.cleanTitle("شاهد الحلقة 1 من كرتون - كرتوني", "cartoony.net", ["كرتوني"]), "الحلقة 1 من كرتون");
  assert.equal(H.cleanTitle("Watch: Big Buck Bunny.mp4", "x.com"), "Big Buck Bunny");
  assert.equal(H.cleanTitle("Some Show - Episode 3 - example.org", "cdn.other.net"), "Some Show - Episode 3");
  assert.equal(H.cleanTitle("Ep​ 1‮", ""), "Ep 1");
});

test("isIdLikeTitle rejects embed codes, generic words and URL tokens", () => {
  assert.ok(H.isIdLikeTitle("4172q6dt4qljs4kfrvr7"));
  assert.ok(H.isIdLikeTitle("sqvh5j52m7b8whx0xcam"));
  assert.ok(H.isIdLikeTitle("master"));
  assert.ok(H.isIdLikeTitle("Video"));
  assert.ok(H.isIdLikeTitle("pZxWPRg4", ["https://c.x/e/pZxWPRg4.m3u8"]));
  assert.ok(H.isIdLikeTitle("d41d8cd98f00b204e9800998ecf8427e"));
  assert.ok(H.isIdLikeTitle("byse", [], "byse.sx"));
  assert.ok(!H.isIdLikeTitle("Episode12"));
  assert.ok(!H.isIdLikeTitle("Big Buck Bunny"));
  assert.ok(!H.isIdLikeTitle("الحلقة 1"));
});

test("pickTitle falls through id-like candidates, per-candidate host", () => {
  const t = H.pickTitle([
    { src: "doc", text: "4172q6dt4qljs4kfrvr7 | Byse", host: "byse.sx" },
    { src: "og", text: "The Real Episode Name - MySite", host: "mysite.com" },
  ], {});
  assert.equal(t, "The Real Episode Name");
  assert.equal(H.pickTitle(["", "master.m3u8", "Video"], {}), "");
  const ordered = H.orderTitleCands([{ src: "doc", text: "a" }, { src: "og", text: "b" }, { src: "player", text: "c" }]);
  assert.deepEqual(ordered.map((x) => x.src), ["player", "og", "doc"]);
});

// ---- hand-off naming / headers -----------------------------------------------------------
test("handoffName drops generic and contradicting extensions", () => {
  assert.equal(H.handoffName("download.php", "application/zip", "https://x/download.php"), "");
  assert.equal(H.handoffName("Fast goal video.mp4", "application/octet-stream", "https://s88.x/a_mp3_1.mp3"), "Fast goal video");
  assert.equal(H.handoffName("setup.exe", "application/octet-stream", "https://x/setup.exe"), "setup.exe");
  assert.equal(H.handoffName("clip.mp4", "audio/mpeg", "https://x/get"), "clip");
});

test("customHeaders keeps Authorization/X-* only", () => {
  const h = H.customHeaders([
    { name: "Authorization", value: "Bearer t" }, { name: "X-Token", value: "1" }, { name: "Cookie", value: "a=1" },
    { name: "User-Agent", value: "UA" }, { name: "Sec-Fetch-Mode", value: "cors" }, { name: "Referer", value: "r" },
    { name: "Accept", value: "*/*" }, { name: "X-Client-Data", value: "zz" },
  ]);
  assert.deepEqual(h, { Authorization: "Bearer t", "X-Token": "1" });
});

test("mediaLabel never shows a byte size and names the stream", () => {
  assert.equal(H.mediaLabel({ kind: "hls", ext: "m3u8", height: 1080, duration: 1421, url: "https://www.cdn.x/a.m3u8" }),
    "HLS stream · 1080p · 23:41 · cdn.x");
  assert.equal(H.mediaLabel({ kind: "file", ext: "mp3", url: "https://a.x/b.mp3" }), "MP3 audio · a.x");
});

// ---- hook.js parsers ---------------------------------------------------------------------
test("hook parseM3U8: master vs media playlist", () => {
  const master = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5200000,RESOLUTION=1920x1080,CODECS=\"avc1.640028,mp4a.40.2\"\n1080p/playlist.m3u8\n" +
    "#EXT-X-STREAM-INF:BANDWIDTH=1200000,RESOLUTION=854x480\n480p/playlist.m3u8\n" +
    "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",LANGUAGE=\"ar\",NAME=\"Arabic\",URI=\"audio/ar.m3u8\"\n";
  const m = K.parseM3U8(master, "https://h.x/ep/index.m3u8");
  assert.equal(m.master, true);
  assert.equal(m.height, 1080);
  assert.equal(m.variants[0].url, "https://h.x/ep/1080p/playlist.m3u8");
  assert.equal(m.renditions[0].lang, "ar");
  const media = "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\ns1.ts\n#EXTINF:4.5,\ns2.ts\n#EXT-X-ENDLIST\n";
  const p = K.parseM3U8(media, "https://h.x/a.m3u8");
  assert.equal(p.master, false);
  assert.equal(p.duration, 10.5);
  assert.equal(p.live, false);
  assert.equal(K.parseM3U8("<html>", ""), null);
});

test("hook analyzeText: DASH, Smooth, JSON source lists", () => {
  const mpd = '<?xml version="1.0"?><MPD mediaPresentationDuration="PT23M41S" type="static"><Representation height="720"/></MPD>';
  const d = K.analyzeText(mpd, "https://c.x/api/stream", "text/plain", false);
  assert.equal(d.type, "dash");
  assert.equal(d.duration, 1421);
  assert.equal(d.height, 720);
  const j = K.analyzeText('{"sources":[{"file":"https:\\/\\/cdn.x\\/v\\/ep.m3u8?t=1"}],"tracks":[]}', "https://c.x/api/source/1", "application/json", false);
  assert.equal(j.t, "json");
  assert.equal(j.urls[0].url, "https://cdn.x/v/ep.m3u8?t=1");
  assert.equal(K.analyzeText("hello world", "https://c.x/", "text/plain", false), null);
});
