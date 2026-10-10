// D BOX Integration - MV3 service worker.
// --------------------------------------------------------------------------
// Works like IDM's module: downloads are intercepted at the browser's
// download-manager layer (chrome.downloads) — when a download that D BOX should
// own begins, the browser's copy is paused, handed to D BOX's New Download
// dialog, and only cancelled once D BOX has accepted it (so nothing is lost
// when D BOX is unreachable). MV3 removed blocking webRequest (the MV2
// approach), and the downloads API is the sanctioned replacement; it also
// catches "Save link as…", which the old network-layer hook never saw.
//
// Being a service worker, this script has NO persistent state: it wakes for
// events (downloads, messages) and dies when idle. Settings are re-read from
// storage on each wake; captures live in memory and are flushed (debounced) to
// chrome.storage.session so they survive a worker restart.
//
// The pure rules (what is media, what is junk, dedup keys, ranking, titles)
// live in lib/helpers.js — shared with the content script and unit-tested with
// node (test/helpers.test.js).

importScripts("lib/helpers.js");
const H = self.DBoxHelpers;

const DEFAULT_SETTINGS = { enabled: true, myidmBase: "http://127.0.0.1:8081" };
let settings = Object.assign({}, DEFAULT_SETTINGS);

// Settings load races the first event after a wake — async paths await this.
const settingsReady = new Promise((resolve) => {
  chrome.storage.local.get("settings", (r) => {
    if (!r.settings) chrome.storage.local.set({ settings: DEFAULT_SETTINGS });
    settings = Object.assign({}, DEFAULT_SETTINGS, r.settings || {});
    resolve();
  });
});
chrome.storage.onChanged.addListener((c, area) => {
  if (area === "local" && c.settings) settings = Object.assign({}, DEFAULT_SETTINGS, c.settings.newValue || {});
});

function base() { return settings.myidmBase.replace(/\/+$/, ""); }
const isHttp = (u) => typeof u === "string" && /^https?:\/\//i.test(u);
function isOwn(url) {
  try { return new URL(url).origin === new URL(base()).origin; } catch (_) { return false; }
}

// ---- D BOX availability (cached ping) -------------------------------------
let myidmUp = false, lastPing = 0, pingInflight = null;

// pingMyIDM returns the cached flag when younger than maxAge ms, else refreshes
// it (deduping concurrent refreshes). maxAge 0 forces a live check.
async function pingMyIDM(maxAge = 4000) {
  await settingsReady;
  if (Date.now() - lastPing < maxAge) return myidmUp;
  if (!pingInflight) {
    pingInflight = (async () => {
      try {
        const ctrl = new AbortController();
        const t = setTimeout(() => ctrl.abort(), 2500); // D BOX under load answers slowly
        const r = await fetch(base() + "/api/categories", { signal: ctrl.signal });
        clearTimeout(t);
        myidmUp = r.ok;
      } catch { myidmUp = false; }
      lastPing = Date.now();
      pingInflight = null;
    })();
  }
  await pingInflight;
  return myidmUp;
}
// dboxReachable reuses a recent positive answer (10 s) and otherwise checks live.
async function dboxReachable() {
  await settingsReady;
  if (myidmUp && Date.now() - lastPing < 10000) return true;
  return pingMyIDM(0);
}

async function postJSON(path, body, timeoutMs) {
  const ctrl = new AbortController();
  const t = timeoutMs ? setTimeout(() => ctrl.abort(), timeoutMs) : 0;
  try {
    const r = await fetch(base() + path, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body), signal: ctrl.signal,
    });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
    return data;
  } finally { if (t) clearTimeout(t); }
}

function notify(title, message) {
  try {
    chrome.notifications.create({ type: "basic", iconUrl: "icons/icon128.png", title, message },
      () => void chrome.runtime.lastError);
  } catch (_) {}
}

// ---- page context handed to D BOX -----------------------------------------
// cookieHeader returns the browser's cookies for url as a Cookie header value
// (httpOnly included — chrome.cookies sees them). Sent only to the local D BOX
// app, which keeps them server-side for this one download.
function cookieHeader(url) {
  return new Promise((resolve) => {
    try {
      chrome.cookies.getAll({ url }, (list) => {
        if (chrome.runtime.lastError || !list) { resolve(""); return; }
        resolve(list.map((c) => c.name + "=" + c.value).join("; "));
      });
    } catch { resolve(""); }
  });
}

// pageContext is what makes D BOX's request look like the browser's own: the
// page it came from, the site's cookies, the browser's User-Agent (Cloudflare
// ties its clearance cookie to it) and, for player requests, the Origin plus
// any custom headers the player sent (Authorization, X-Token…).
async function pageContext(url, referer, origin, extraHeaders) {
  const ctx = { userAgent: navigator.userAgent };
  if (referer) ctx.referer = referer;
  const cookies = await cookieHeader(url);
  if (cookies) ctx.cookies = cookies;
  const headers = Object.assign({}, extraHeaders || {});
  if (origin) headers.Origin = origin;
  if (Object.keys(headers).length) ctx.headers = headers;
  return ctx;
}

// ---- titles ----------------------------------------------------------------
// frameTitles asks one frame's content script for its title candidates
// ({cands:[{src,text}], host, siteNames}); null when it doesn't answer.
function frameTitles(tabId, frameId) {
  return new Promise((resolve) => {
    if (tabId == null || tabId < 0) { resolve(null); return; }
    let done = false;
    const t = setTimeout(() => { done = true; resolve(null); }, 500);
    try {
      chrome.tabs.sendMessage(tabId, { type: "getTitles" }, { frameId: frameId || 0 }, (r) => {
        void chrome.runtime.lastError;
        if (done) return;
        clearTimeout(t);
        resolve(r && r.ok ? r : null);
      });
    } catch (_) { clearTimeout(t); resolve(null); }
  });
}

// bestTitle picks the video's name from the asking frame's and the top
// frame's candidates: a player's own playlist title first, then the top
// page's structured titles (JSON-LD, og:, twitter:), its document title, and
// only then an embed frame's (usually a random file code). Random-looking and
// generic titles are skipped; " | Site" suffixes are stripped.
// opts: {urls, first:[cands], later:[cands], tabTitle, tabUrl}
async function bestTitle(tabId, frameId, own, opts) {
  opts = opts || {};
  frameId = frameId || 0;
  const top = frameId === 0 && own ? own : await frameTitles(tabId, 0);
  const frame = frameId === 0 ? null : (own || await frameTitles(tabId, frameId));
  const tag = (r) => (r && r.cands ? r.cands : []).map((c) => ({ src: c.src, text: c.text, host: r.host, siteNames: r.siteNames }));
  const f = tag(frame), t = tag(top);
  const by = (list, srcs) => list.filter((c) => srcs.includes(c.src));
  const cands = [].concat(
    by(f, ["player"]), by(t, ["player"]),
    opts.first || [],
    by(t, ["jsonld", "og", "twitter"]), by(t, ["doc"]),
    opts.later || [],
    by(f, ["jsonld", "og", "twitter", "doc"]), by(t, ["h1", "video"]), by(f, ["h1", "video"]));
  let title = H.pickTitle(cands, { urls: opts.urls || [] });
  if (!title && opts.tabTitle) {
    const c = H.cleanTitle(opts.tabTitle, H.hostOf(opts.tabUrl || ""));
    if (!H.isIdLikeTitle(c, opts.urls || [], H.hostOf(opts.tabUrl || ""))) title = c;
  }
  return title;
}

// ---- download capture ------------------------------------------------------
// Everything reaching chrome.downloads IS a download (the browser decided), so
// classification only scopes WHICH file types D BOX takes over — archives,
// installers, media files. Images and documents the user "Save as…"es stay
// with the browser, as do tiny files (< 50 KB) that aren't archives/installers.
const BINARY_CT = new Set([
  "application/octet-stream", "binary/octet-stream", "application/binary", "application/force-download",
  "application/x-download", "application/download", "application/unknown", "application/x-unknown-content-type",
  "application/zip", "application/x-zip", "application/x-zip-compressed",
  "application/x-rar-compressed", "application/vnd.rar", "application/x-rar", "application/x-7z-compressed",
  "application/gzip", "application/x-gzip", "application/x-tar", "application/x-bzip", "application/x-bzip2",
  "application/x-xz", "application/zstd", "application/vnd.ms-cab-compressed",
  "application/x-msdownload", "application/x-msdos-program", "application/x-msi",
  "application/vnd.android.package-archive", "application/x-apple-diskimage", "application/java-archive",
  "application/x-iso9660-image", "application/vnd.microsoft.portable-executable",
  "application/x-debian-package", "application/x-rpm", "application/x-redhat-package-manager",
]);
const DL_EXT = new Set([
  "zip", "rar", "7z", "tar", "gz", "bz2", "xz", "zst", "iso", "cab", "tgz", "lz", "lzma", "zipx",
  "exe", "msi", "apk", "xapk", "apks", "dmg", "deb", "rpm", "appimage", "msix", "msixbundle", "jar", "bin", "img",
  "mp4", "mkv", "avi", "mov", "webm", "flv", "wmv", "m4v", "mpg", "mpeg", "ts", "3gp",
  "m2ts", "mts", "vob", "rmvb", "ogv",
  "mp3", "wav", "flac", "aac", "ogg", "m4a", "wma", "opus", "mka", "m4b", "aiff", "ape",
]);
const dlExt = (ext) => DL_EXT.has(ext) || /^(\d{3}|r\d{2}|z\d{2})$/.test(ext); // split archives .001 .r00 .z01

function extFromUrl(url) {
  try { return H.extOf(new URL(url).pathname); } catch { return ""; }
}
function isLoopback(url) {
  try {
    const h = new URL(url).hostname;
    return h === "127.0.0.1" || h === "localhost" || h === "[::1]" || h === "::1";
  } catch { return false; }
}

// shouldCapture scopes the takeover: http(s) only (blob:/data:/file: can't be
// re-fetched by D BOX), never D BOX's own traffic, and only file types from the
// capture list (by suggested filename, URL extension, or binary MIME).
function shouldCapture(item) {
  if (!settings.enabled) return false;
  const url = item.finalUrl || item.url || "";
  if (!isHttp(url) || isLoopback(url)) return false;
  const ext = H.extOf(item.filename) || extFromUrl(url);
  if (!dlExt(ext) && !BINARY_CT.has(H.baseCT(item.mime))) return false;
  const fam = H.famOfExt(ext);
  if (item.totalBytes > 0 && item.totalBytes < 50 * 1024 && fam !== "archive" && fam !== "program") return false;
  return true;
}

let lastDownWarn = 0;
function warnNotRunning() {
  const now = Date.now();
  if (now - lastDownWarn < 3600000) return;
  lastDownWarn = now;
  notify("D BOX is not running", "Start D BOX to capture downloads. This one was left to the browser.");
}

const dlApi = (fn, id) => new Promise((resolve) => {
  try { chrome.downloads[fn](id, () => resolve(!chrome.runtime.lastError)); } catch (_) { resolve(false); }
});
const dlState = (id) => new Promise((resolve) => {
  try { chrome.downloads.search({ id }, (l) => resolve((!chrome.runtime.lastError && l && l[0] && l[0].state) || "")); }
  catch (_) { resolve(""); }
});

// tabForDownload guesses the tab a download came from (DownloadItem has no
// tabId): the tab showing the referrer, else the focused window's active tab.
async function tabForDownload(item) {
  try {
    const ref = item.referrer || "";
    if (ref) {
      const all = await chrome.tabs.query({});
      const t = all.find((x) => x.url === ref);
      if (t) return t;
    }
    const [act] = await chrome.tabs.query({ active: true, lastFocusedWindow: true });
    return act || null;
  } catch (_) { return null; }
}

const recentHandoffs = new Map(); // normKey -> ts: one hand-off per file per few seconds

// handOff sends a download to D BOX's New Download window (or its headless
// popup). Resolves true once D BOX has accepted it.
async function handOff(o) {
  const ctx = await pageContext(o.url, o.referer, "");
  const body = Object.assign({
    url: o.url, name: o.name || "", title: o.title || "",
    expectedSize: o.expectedSize || 0, expectedMime: o.expectedMime || "",
    origUrl: o.origUrl || "", finalUrl: o.finalUrl || "",
  }, ctx);
  try {
    const d = await postJSON("/api/prompt", body, 8000);
    if (!d || !d.native) openAddPopup(o.url, o.name, o.referer, d && d.ctx, o.title);
    return true;
  } catch (_) { return false; }
}

// openAddPopup is the headless fallback. Cookies are NOT put in this URL; a
// ctx token (when the server issued one) carries them server-side.
function openAddPopup(url, name, referer, ctx, title) {
  const params = new URLSearchParams({ url, ext: chrome.runtime.id });
  if (name) params.set("name", name);
  if (referer) params.set("referer", referer);
  if (ctx) params.set("ctx", ctx);
  if (title) params.set("title", title);
  chrome.windows.create({ url: base() + "/add?" + params.toString(), type: "popup", width: 600, height: 460 });
}

// onDeterminingFilename fires once per download with the browser's suggested
// filename (Content-Disposition already resolved). Returning true lets suggest()
// be called async, after the D BOX liveness check.
chrome.downloads.onDeterminingFilename.addListener((item, suggest) => {
  (async () => {
    await settingsReady;
    if (!shouldCapture(item)) { suggest(); return; }
    const up = await dboxReachable(); // capture must not eat the download if D BOX is gone
    suggest();
    if (!up) { warnNotRunning(); return; }
    // Hold the browser's copy instead of cancelling it: it is only cancelled
    // once D BOX has the download, and resumed if the hand-off fails. A file
    // that already finished (tiny, fast) stays with the browser.
    const paused = await dlApi("pause", item.id);
    if (!paused && (await dlState(item.id)) !== "in_progress") return;
    const url = item.finalUrl || item.url;
    const k = H.normKey(url);
    const dup = Date.now() - (recentHandoffs.get(k) || 0) < 5000;
    recentHandoffs.set(k, Date.now());
    if (recentHandoffs.size > 50) recentHandoffs.delete(recentHandoffs.keys().next().value);
    let ok = dup;
    if (!dup) {
      const tab = await tabForDownload(item);
      const tabUrl = tab && isHttp(tab.url) && tab.url !== url ? tab.url : "";
      const title = tab ? await bestTitle(tab.id, 0, null, { urls: [url], tabTitle: tab.title, tabUrl: tab.url }) : "";
      ok = await handOff({
        url, name: H.handoffName(item.filename, item.mime, url), referer: item.referrer || tabUrl, title,
        expectedSize: item.totalBytes > 0 ? item.totalBytes : (item.fileSize > 0 ? item.fileSize : 0),
        expectedMime: item.mime || "", origUrl: item.url || "", finalUrl: item.finalUrl || "",
      });
    }
    if (ok) {
      chrome.downloads.cancel(item.id, () => {
        void chrome.runtime.lastError;
        // erase removes the cancelled stub from the browser's download shelf/history
        chrome.downloads.erase({ id: item.id }, () => void chrome.runtime.lastError);
      });
    } else {
      if (paused) dlApi("resume", item.id);
      notify("D BOX didn't take this download", "It was left to the browser.");
    }
  })();
  return true; // async suggest()
});

// ---- login cookies for yt-dlp (Instagram stories/reels, etc.) ------------
// yt-dlp runs as a separate process and can't read the browser's cookie DB while
// Brave is open (the file is locked). But WE are inside Brave with the live
// session, so we read the site's cookies via chrome.cookies and hand them to
// D BOX, which writes a cookies.txt and passes --cookies to yt-dlp. Scoped to
// hosts that actually gate media behind a login; re-pushed at most every ~2 min.
const COOKIE_HOSTS = /(^|\.)(instagram|facebook|fb|threads|tiktok|twitter|x|reddit|patreon|nicovideo|weibo|bilibili)\.(com|net|tv|jp|cn)$/i;
const cookiePushed = new Map(); // registrable-domain -> timestamp (resets with the worker; harmless)

function collectCookies(url) {
  return new Promise((resolve) => {
    try {
      chrome.cookies.getAll({ url }, (list) => {
        if (chrome.runtime.lastError || !list) { resolve([]); return; }
        resolve(list.map((c) => ({
          name: c.name, value: c.value, domain: c.domain, path: c.path,
          secure: !!c.secure, httpOnly: !!c.httpOnly, hostOnly: !!c.hostOnly,
          expiry: c.expirationDate || 0, // seconds; absent => session cookie
        })));
      });
    } catch { resolve([]); }
  });
}

async function ensureCookies(url, force) {
  let host;
  try { host = new URL(url).hostname; } catch { return; }
  if (!COOKIE_HOSTS.test(host)) return;
  const key = host.split(".").slice(-2).join(".");
  const now = Date.now();
  if (!force && now - (cookiePushed.get(key) || 0) < 120000) return;
  const cookies = await collectCookies(url);
  if (!cookies.length) return; // not logged in / nothing to send
  try {
    await postJSON("/api/cookies", { url, cookies });
    cookiePushed.set(key, now);
  } catch (_) {}
}

// ---- media sniffer (IDM-style network capture) ---------------------------
// yt-dlp can't resolve many embedded players by page URL, but the player still
// FETCHES the real media. MV3 permits OBSERVING requests, so each tab's media
// is collected here, from three sources:
//   - the network (onResponseStarted: URL + Content-Type + sizes, incl. cache
//     hits), with the request's Referer/Origin/custom headers from
//     onBeforeSendHeaders (matched by requestId);
//   - the MAIN-world page hook (hook.js, relayed by content.js): manifests
//     served as text/plain/octet-stream/JSON, player libraries' sources, the
//     master/variant structure, durations;
//   - service-worker fetches (tabId -1), attributed to tabs of the initiator.
// Segments are never listed; files under a known manifest's directory and MSE
// xhr chunks are treated as segments; junk (ads, UI sounds) is dropped.
//
// State: in memory per tab, flushed (debounced 500 ms) to storage.session.
const CAPS = { hls: 60, file: 60, sub: 30 };
const sessKey = (tabId) => "media_" + tabId;
const tabs = new Map();   // tabId -> Promise<state>
const flushT = new Map(); // tabId -> timer

function blankState() { return { items: new Map(), children: new Set(), mse: {}, navTs: 0 }; }
function loadTab(tabId) {
  let p = tabs.get(tabId);
  if (!p) {
    const k = sessKey(tabId);
    p = chrome.storage.session.get(k).then((r) => {
      const st = blankState(), s = r && r[k];
      if (s) {
        for (const it of s.items || []) if (it && it.key) st.items.set(it.key, it);
        for (const c of s.children || []) st.children.add(c);
        st.mse = s.mse || {};
        st.navTs = s.navTs || 0;
      }
      return st;
    }, () => blankState());
    tabs.set(tabId, p);
  }
  return p;
}
// withTab runs fn on the tab's state; fn returns true when something changed.
function withTab(tabId, fn) {
  return loadTab(tabId).then((st) => {
    let changed = false;
    try { changed = fn(st); } catch (_) {}
    if (changed) scheduleFlush(tabId);
    return st;
  });
}
function scheduleFlush(tabId) {
  if (flushT.has(tabId)) return;
  flushT.set(tabId, setTimeout(() => { flushT.delete(tabId); flush(tabId); }, 500));
}
async function flush(tabId) {
  const p = tabs.get(tabId);
  if (!p) return;
  const st = await p;
  const data = () => ({ items: [...st.items.values()], children: [...st.children].slice(-400), mse: st.mse, navTs: st.navTs });
  try { await chrome.storage.session.set({ [sessKey(tabId)]: data() }); }
  catch (_) {
    // Quota: keep streams, drop the older half of the files, retry once.
    const files = [...st.items.values()].filter((x) => x.kind !== "hls").sort((a, b) => a.ts - b.ts);
    for (const f of files.slice(0, Math.ceil(files.length / 2))) st.items.delete(f.key);
    try { await chrome.storage.session.set({ [sessKey(tabId)]: data() }); } catch (_) {}
  }
  updateBadge(tabId, st);
}

function setBadge(tabId, n) {
  try {
    chrome.action.setBadgeText({ tabId, text: n > 0 ? (n > 99 ? "99+" : String(n)) : "" }).catch(() => {});
  } catch (_) {}
}
function updateBadge(tabId, st) {
  const n = H.rankMedia([...st.items.values()], { children: st.children, navTs: st.navTs })
    .filter((it) => it.kind !== "sub" && !(st.navTs && (it.ts || 0) < st.navTs)).length;
  setBadge(tabId, n);
}
try {
  chrome.action.setBadgeBackgroundColor({ color: "#2fcc66" });
  if (chrome.action.setBadgeTextColor) chrome.action.setBadgeTextColor({ color: "#ffffff" });
} catch (_) {}

function resetTab(tabId) {
  tabs.set(tabId, Promise.resolve(blankState()));
  chrome.storage.session.remove(sessKey(tabId)).catch(() => {});
  setBadge(tabId, 0);
}
function dropTab(tabId) {
  tabs.delete(tabId);
  const t = flushT.get(tabId);
  if (t) { clearTimeout(t); flushT.delete(tabId); }
  chrome.storage.session.remove(sessKey(tabId)).catch(() => {});
}
chrome.tabs.onRemoved.addListener(dropTab);

// resetFrame drops one frame's captures when its iframe navigates to a new
// document — so a new video loading in the SAME player frame replaces the old
// one instead of stacking behind it.
function resetFrame(tabId, frameId) {
  withTab(tabId, (st) => {
    let ch = false;
    for (const [k, it] of st.items) if (it.frameId === frameId) { st.items.delete(k); ch = true; }
    if (st.mse[frameId]) { delete st.mse[frameId]; ch = true; }
    return ch;
  });
}

const manifestsOf = (st, frameId) => [...st.items.values()]
  .filter((x) => x.kind === "hls" && x.frameId === frameId).map((x) => x.url);

// addItem merges one capture into the tab state (dedup by normalized key).
// A later, better classification upgrades ext/kind (a guessed "mp4" becomes
// the audio/mpeg "mp3", a plain file becomes a stream); sizes only grow.
function addItem(st, n) {
  const old = st.items.get(n.key);
  if (old) {
    let ch = false;
    if (n.size > (old.size || 0) && old.kind !== "hls") { old.size = n.size; ch = true; }
    if (H.upgradeExt(old, n)) {
      old.ext = n.ext; old.kind = n.kind; old.extFrom = n.extFrom; ch = true;
      if (old.kind === "hls") old.size = 0; // a manifest's bytes aren't the video's size
    }
    if (n.via === "net" && old.via !== "net") { old.via = "net"; ch = true; }
    for (const f of ["master", "duration", "height", "live", "drm"]) {
      if (n[f] && old[f] !== n[f]) { old[f] = n[f]; ch = true; }
    }
    for (const f of ["referer", "origin", "frameUrl", "ct", "lang", "label"]) {
      if (n[f] && !old[f]) { old[f] = n[f]; ch = true; }
    }
    if (n.headers) { old.headers = Object.assign({}, old.headers || {}, n.headers); ch = true; }
    if (old.frameId !== n.frameId) { old.frameId = n.frameId; ch = true; }
    old.ts = n.ts;
    return ch;
  }
  const cap = CAPS[n.kind] || 30;
  const same = [...st.items.values()].filter((x) => x.kind === n.kind);
  if (same.length >= cap) {
    const oldest = same.reduce((a, b) => ((b.ts || 0) < (a.ts || 0) ? b : a));
    st.items.delete(oldest.key);
  }
  st.items.set(n.key, n);
  return true;
}

// suppressSegFiles removes "files" of a frame that turn out to be pieces of a
// stream once its manifest is known (captured before the manifest was).
function suppressSegFiles(st, man) {
  let ch = false;
  for (const [k, it] of st.items) {
    if (it.kind === "file" && it.frameId === man.frameId && it.via === "net" &&
        H.looksLikeSegmentOf(it, [man.url])) { st.items.delete(k); ch = true; }
  }
  return ch;
}

// captureNet files one classified network response under a tab.
function captureNet(tabId, item) {
  withTab(tabId, (st) => {
    if (item.kind === "file") {
      const mans = manifestsOf(st, item.frameId);
      if (H.isXhrSegment(item, mans.length > 0)) return false;
      if (mans.length && H.looksLikeSegmentOf(item, mans)) return false;
    }
    let ch = addItem(st, item);
    if (item.kind === "hls") ch = suppressSegFiles(st, item) || ch;
    return ch;
  });
}

// ---- network observers ---------------------------------------------------------
// Request headers the player really sent, by requestId, committed when the
// response is classified as media: Referer/Origin (hotlink protection,
// CORS-gated CDNs) and custom headers (Authorization, X-Token…). "extraHeaders"
// is what makes Referer/Origin visible to the listener.
const reqHdrs = new Map();
chrome.webRequest.onBeforeSendHeaders.addListener(
  (d) => {
    let referer = "", origin = "";
    for (const h of d.requestHeaders || []) {
      const n = h.name.toLowerCase();
      if (n === "referer") referer = h.value || "";
      else if (n === "origin" && h.value !== "null") origin = h.value || "";
    }
    const headers = H.customHeaders(d.requestHeaders);
    const hasCustom = Object.keys(headers).length > 0;
    if (!referer && !origin && !hasCustom) return;
    if (reqHdrs.size > 800) reqHdrs.delete(reqHdrs.keys().next().value);
    reqHdrs.set(d.requestId, { referer, origin, headers: hasCustom ? headers : null });
  },
  { urls: ["http://*/*", "https://*/*"], types: ["media", "xmlhttprequest", "other", "object"] },
  ["requestHeaders", "extraHeaders"]
);

// An iframe navigating to a new document gets a clean slate.
chrome.webRequest.onBeforeRequest.addListener(
  (d) => { if (d.tabId >= 0 && d.type === "sub_frame" && d.documentLifecycle !== "prerender") resetFrame(d.tabId, d.frameId); },
  { urls: ["http://*/*", "https://*/*"], types: ["sub_frame"] }
);

function header(d, name) {
  for (const h of d.responseHeaders || []) if (h.name.toLowerCase() === name) return h.value || "";
  return "";
}

// A main-frame response that renders a page (not a download, not a
// prerender) starts a new capture list for the tab.
function isPageLoad(d) {
  if (d.frameId !== 0 || d.documentLifecycle === "prerender") return false;
  if (d.statusCode === 204 || d.statusCode === 205) return false;
  if (/attachment/i.test(header(d, "content-disposition"))) return false;
  const c = H.baseCT(header(d, "content-type"));
  return !c || c.includes("html") || c.includes("xml") || c === "text/plain";
}

// onResponseStarted (not onHeadersReceived) also fires for disk-cache hits.
chrome.webRequest.onResponseStarted.addListener(
  (d) => {
    const hdr = reqHdrs.get(d.requestId);
    if (hdr) reqHdrs.delete(d.requestId);
    if (d.type === "main_frame") { if (d.tabId >= 0 && isPageLoad(d)) resetTab(d.tabId); return; }
    if (!H.MEDIA_TYPES.has(d.type)) return;
    if (d.statusCode && (d.statusCode < 200 || d.statusCode >= 300)) return;
    if (isOwn(d.url)) return;
    const ct = header(d, "content-type");
    const c = H.classify({ type: d.type, url: d.url, ct });
    if (!c || c.kind === "seg") return; // pieces of a stream are never listed
    if (H.isJunkUrl(d.url) || (d.documentUrl && H.isJunkUrl(d.documentUrl))) return;
    const cr = header(d, "content-range");
    const item = {
      key: H.normKey(d.url), url: H.cleanMediaUrl(d.url), ext: c.ext, kind: c.kind, extFrom: c.extFrom,
      ct: H.baseCT(ct), type: d.type, via: "net", frameId: d.frameId, ts: Date.now(),
      size: c.kind === "hls" ? 0 : H.parseSize(header(d, "content-length"), cr, header(d, "content-encoding")),
      partial: d.statusCode === 206 || !!cr || H.isPartialUrl(d.url),
    };
    if (d.documentUrl) item.frameUrl = d.documentUrl;
    if (hdr) {
      if (hdr.referer) item.referer = hdr.referer;
      if (hdr.origin) item.origin = hdr.origin;
      if (hdr.headers) item.headers = hdr.headers;
    }
    if (d.tabId >= 0) captureNet(d.tabId, item);
    else attributeOrphan(d, item);
  },
  { urls: ["http://*/*", "https://*/*"] },
  ["responseHeaders"]
);

// attributeOrphan files media fetched by a site's service worker (tabId -1)
// under the tabs showing that site (the initiator's origin).
async function attributeOrphan(d, item) {
  const init = d.initiator || "";
  if (!isHttp(init) || isOwn(init)) return;
  if (item.kind === "file" && H.isXhrSegment(item, false)) return;
  let list = [];
  try { list = await chrome.tabs.query({ url: init.replace(/\/+$/, "") + "/*" }); } catch (_) { return; }
  for (const t of list.slice(0, 3)) captureNet(t.id, Object.assign({}, item, { frameId: 0, sw: true }));
}

// ---- page hook findings (hook.js -> content.js -> here) -----------------------
const str = (v, n) => (typeof v === "string" ? v.slice(0, n) : "");
const num = (v) => (typeof v === "number" && isFinite(v) && v > 0 ? v : 0);
function cleanHdrs(h) {
  if (!h || typeof h !== "object") return null;
  const out = H.customHeaders(Object.keys(h).map((k) => ({ name: k, value: String(h[k]) })));
  return Object.keys(out).length ? out : null;
}

function hookItem(url, ext, kind, extFrom, base, extra) {
  if (!isHttp(url) || url.length > 4096 || isOwn(url) || H.isJunkUrl(url)) return null;
  return Object.assign({ key: H.normKey(url), url: H.cleanMediaUrl(url), ext, kind, extFrom, size: 0 }, base, extra);
}

function hookOne(st, m, frameId, frameUrl) {
  if (!m || typeof m !== "object") return false;
  const b = { frameId, ts: Date.now(), frameUrl };
  if (m.t === "mse") { st.mse[frameId] = Date.now(); return true; }
  if (m.t === "manifest") {
    const ext = m.type === "dash" ? "mpd" : m.type === "ism" ? "ism" : "m3u8";
    const it = hookItem(m.url, ext, "hls", "hook", b, {
      via: "hook", master: !!m.master, duration: num(m.duration), height: num(m.height),
      live: m.live === true, drm: !!m.drm, headers: cleanHdrs(m.headers),
    });
    if (!it) return false;
    if (m.master) {
      const kids = [].concat(Array.isArray(m.variants) ? m.variants : [], Array.isArray(m.renditions) ? m.renditions : []);
      for (const v of kids.slice(0, 80)) if (v && isHttp(v.url)) st.children.add(H.normKey(v.url));
    }
    let ch = addItem(st, it);
    ch = suppressSegFiles(st, it) || ch;
    return ch;
  }
  if (m.t === "json" && Array.isArray(m.urls)) {
    let ch = false;
    for (const u of m.urls.slice(0, 12)) {
      const ext = u && H.MEDIA_EXTS.has(u.ext) ? u.ext : "";
      if (!ext || H.kindOf(ext) === "seg") continue;
      const it = hookItem(u.url, ext, H.kindOf(ext), "url", b, { via: "json", headers: cleanHdrs(m.headers) });
      if (it) ch = addItem(st, it) || ch;
    }
    return ch;
  }
  if (m.t === "player") {
    const lib = str(m.lib, 20);
    const pe = H.pathExt(m.url);
    let ext = pe || (H.MEDIA_EXTS.has(m.ext) ? m.ext : "");
    let from = pe ? "url" : "hook";
    if (!ext) {
      ext = lib === "hls.js" ? "m3u8" : (lib === "shaka" || lib === "dash.js") ? "mpd" : (lib === "flvjs" || lib === "mpegts") ? "flv" : "mp4";
      if (ext === "mp4") from = "guess";
    }
    const kind = m.sub ? "sub" : H.kindOf(ext);
    if (kind === "seg") return false;
    const it = hookItem(m.url, ext, kind, from, b, { via: "player", lang: str(m.lang, 40), label: str(m.label, 40), live: !!m.live });
    return it ? addItem(st, it) : false;
  }
  if (m.t === "media") {
    const it = hookItem(m.url, "flv", "file", "hook", b, { via: "hook", live: !!m.live });
    return it ? addItem(st, it) : false;
  }
  return false;
}

// ---- ranked view for the overlay / popup / context menu ------------------------------
async function rankedFor(tabId, o) {
  o = o || {};
  const st = await loadTab(tabId);
  const preferKey = isHttp(o.preferUrl) ? H.normKey(o.preferUrl) : "";
  const ranked = H.rankMedia([...st.items.values()], {
    frameId: o.frameId, preferKey, mse: o.frameId != null && !!st.mse[o.frameId],
    duration: +o.duration || 0, children: st.children, navTs: st.navTs,
  });
  return ranked.slice(0, 30).map((it) => ({
    key: it.key, url: it.url, ext: it.ext, kind: it.kind, frameId: it.frameId, via: it.via,
    size: it.kind === "hls" ? 0 : (it.size || 0), duration: it.duration || 0, height: it.height || 0,
    live: !!it.live, label: H.mediaLabel(it),
  }));
}

// sendStream hands one captured stream/file to D BOX with the context the
// player used: Referer/Origin/custom headers it sent, the site's cookies and
// the browser's User-Agent; named from the best page title.
async function sendStream(tab, it, ownFrameId, own) {
  const tabId = tab ? tab.id : -1;
  const tabUrl = (tab && tab.url) || "";
  const referer = it.referer || it.frameUrl || tabUrl || "";
  if (it.kind === "sub") {
    const ctx = await pageContext(it.url, referer, it.origin || "", it.headers);
    const name = (H.baseName(it.url) || ("subtitles." + it.ext)).replace(/[<>:"/\\|?*]+/g, "_");
    return { ok: true, result: await postJSON("/api/tasks", Object.assign({ url: it.url, fileName: name, category: "Documents" }, ctx)) };
  }
  const fid = it.frameId != null ? it.frameId : (ownFrameId || 0);
  const title = await bestTitle(tabId, fid, fid === ownFrameId ? own : null,
    { urls: [it.url], tabTitle: tab && tab.title, tabUrl });
  const ctx = await pageContext(it.url, referer, it.origin || "", it.headers);
  let native = false;
  try {
    const d = await postJSON("/api/prompt", Object.assign({ url: it.url, title }, ctx), 8000);
    native = !!(d && d.native);
  } catch (_) {}
  if (native) return { ok: true, native: true };
  return { ok: true, result: await postJSON("/api/tasks", Object.assign({ url: it.url, title, category: "Video" }, ctx)) };
}

// grabShow opens D BOX's Link Grabber on a page (the server crawls it with
// the browser's context).
async function grabShow(tab, url) {
  if (!isHttp(url)) throw new Error("This page has no web address D BOX can read.");
  const title = tab ? await bestTitle(tab.id, 0, null, { tabTitle: tab.title, tabUrl: tab.url }) : "";
  const ctx = await pageContext(url, url, "");
  return postJSON("/api/grab/show", Object.assign({ url, title }, ctx), 8000);
}

async function tabById(id) {
  try { return await chrome.tabs.get(id); } catch (_) { return null; }
}

// ---- messages from the content script / popup ---------------------------------
chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    await settingsReady;
    // Content scripts speak for the tab they run in. The extension's own pages
    // (the media panel — a tab of its own when opened as a small window) name
    // the tab they're about in msg.tabId.
    const extPage = typeof sender.url === "string" && sender.url.startsWith(chrome.runtime.getURL(""));
    const fromTab = !extPage && !!(sender.tab && sender.tab.id >= 0);
    const tab = fromTab ? sender.tab : (msg.tabId != null ? await tabById(msg.tabId) : null);
    const tabId = tab ? tab.id : -1;
    const frameId = fromTab ? (sender.frameId || 0) : (msg.frameId != null ? msg.frameId : null);
    const tabURL = (tab && tab.url) || "";
    try {
      if (msg.type === "status") {
        sendResponse({ ok: await pingMyIDM() });
      } else if (msg.type === "hook") {
        if (fromTab && Array.isArray(msg.msgs)) {
          const fu = sender.url || "";
          withTab(tabId, (st) => {
            let ch = false;
            for (const m of msg.msgs.slice(0, 60)) ch = hookOne(st, m, frameId, fu) || ch;
            return ch;
          });
        }
        sendResponse({ ok: true });
      } else if (msg.type === "navigated") {
        // SPA route change in the top frame: older captures rank below new ones.
        if (fromTab && frameId === 0) withTab(tabId, (st) => { st.navTs = Date.now(); return true; });
        sendResponse({ ok: true });
      } else if (msg.type === "sniffed") {
        if (tabId < 0) { sendResponse({ ok: true, streams: [] }); return; }
        sendResponse({ ok: true, streams: await rankedFor(tabId, { frameId, preferUrl: msg.preferUrl, duration: msg.duration }) });
      } else if (msg.type === "probe") {
        // Probe the frame's own URL first — for player EMBEDS (uqload, dood, …)
        // the frame URL is exactly what yt-dlp resolves, with the embedding
        // page as Referer. When that fails (a player shell), retry with the
        // tab's top-level page URL. The response says which URL and Referer
        // worked, so the download later uses the very same context.
        const cands = [msg.url, tabURL].filter((u, i, a) => isHttp(u) && a.indexOf(u) === i);
        if (!cands.length) throw new Error("This frame has no web address D BOX can read.");
        let r = null, data = null, resolved = "", referer = "";
        for (const u of cands) {
          await ensureCookies(u); // authenticate yt-dlp BEFORE probing (IG/etc.)
          const ref = tabURL && tabURL !== u ? tabURL : "";
          r = await fetch(base() + "/api/probe?url=" + encodeURIComponent(u) + (ref ? "&referer=" + encodeURIComponent(ref) : ""));
          data = await r.json().catch(() => ({}));
          if (r.ok) { resolved = u; referer = ref; break; }
        }
        if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
        sendResponse({ ok: true, result: data, resolved, referer });
      } else if (msg.type === "queueVideo") {
        sendResponse({ ok: true, result: await postJSON("/api/video", msg.payload) });
      } else if (msg.type === "promptVideo") {
        // Download what the probe resolved (an embed frame's own URL, or the
        // page), with the Referer the probe used — never swapped for the top
        // page, whose extraction would be a different one.
        const p = msg.payload || {};
        const url = isHttp(p.url) ? p.url : tabURL;
        const referer = p.referer || (tabURL && tabURL !== url ? tabURL : "");
        await ensureCookies(url, true); // refresh cookies right before the download
        const ctx = await pageContext(url, referer, "");
        const yt = p.title ? [{ src: "ytdlp", text: p.title, host: H.hostOf(url) }] : [];
        const embed = url !== tabURL;
        const title = (await bestTitle(tabId, frameId || 0, p.titles, {
          urls: [url], first: embed ? [] : yt, later: embed ? yt : [], tabTitle: tab && tab.title, tabUrl: tabURL,
        })) || p.title || "";
        let native = false;
        try {
          const d = await postJSON("/api/prompt", Object.assign({
            url, name: title, video: true, selector: p.selector, ext: p.ext, audio: !!p.audio, title,
          }, ctx), 8000);
          native = !!(d && d.native);
        } catch (_) {}
        if (native) sendResponse({ ok: true, native: true });
        else sendResponse({ ok: true, result: await postJSON("/api/video", Object.assign({
          url, title, selector: p.selector, ext: p.ext, audio: !!p.audio, size: p.size || 0, category: "Video",
        }, ctx)) });
      } else if (msg.type === "promptStream") {
        const p = msg.payload || {};
        let it = null;
        if (tabId >= 0 && p.key) it = (await loadTab(tabId)).items.get(p.key) || null;
        if (!it) {
          if (!isHttp(p.url)) throw new Error("That stream is no longer available — play the video again.");
          it = { url: p.url, kind: p.kind || "file", ext: p.ext || "", frameId: frameId || 0, frameUrl: p.frameUrl || "" };
        }
        sendResponse(await sendStream(tab, it, frameId, p.titles));
      } else if (msg.type === "queueFile") {
        const p = msg.payload || {};
        const ctx = await pageContext(p.url, p.referer || tabURL, "");
        sendResponse({ ok: true, result: await postJSON("/api/tasks", Object.assign({}, p, ctx)) });
      } else if (msg.type === "openApp") {
        await openApp();
        sendResponse({ ok: true });
      } else if (msg.type === "grabPage") {
        sendResponse({ ok: true, result: await grabShow(tab, tabURL) });
      } else {
        sendResponse({ ok: false, error: "unknown message" });
      }
    } catch (e) {
      sendResponse({ ok: false, error: String((e && e.message) || e) });
    }
  })();
  return true; // async response
});

// D BOX's dialog asks us to close its popup window after queueing the download.
chrome.runtime.onMessageExternal.addListener((msg, sender, sendResponse) => {
  if (msg && msg.type === "closeDialog" && sender.tab) {
    chrome.windows.remove(sender.tab.windowId, () => void chrome.runtime.lastError);
    sendResponse({ ok: true });
  }
  return true;
});

// ---- toolbar icon: one click opens D BOX ------------------------------------
// Clicking the icon brings up the D BOX window that's running (often hidden in
// the tray since Windows started). Everything the old popup offered lives on
// the icon's right-click menu: Capture on/off, the media captured on the tab
// (the same panel, opened from there) and the Link Grabber; Options (D BOX's
// address) is the browser's own item on that menu.
chrome.action.onClicked.addListener(() => { openApp(); });

async function openApp() {
  await settingsReady;
  let r;
  try {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), 3000);
    r = await fetch(base() + "/api/show", { method: "POST", signal: ctrl.signal });
    clearTimeout(t);
  } catch {
    myidmUp = false; lastPing = Date.now();
    notify("D BOX is not running",
      "Open D BOX from the Start menu or desktop. It starts with Windows by default, so it'll be ready next time.");
    return;
  }
  myidmUp = true; lastPing = Date.now();
  const d = r.ok ? await r.json().catch(() => ({})) : {};
  // No window to show (D BOX runs without one, or is older than 1.2.1):
  // open its interface in a tab instead.
  if (!r.ok || !d.shown) chrome.tabs.create({ url: base() + "/" });
}

// openPanel shows the media panel (action.html) anchored to the icon. The
// icon has no popup — so a click reaches onClicked — except for this moment.
async function openPanel(tab) {
  try {
    await chrome.action.setPopup({ popup: "action.html" });
    await chrome.action.openPopup(tab && tab.windowId >= 0 ? { windowId: tab.windowId } : {});
    return;
  } catch (_) {
    // openPopup is missing (older browser) or refused: a small window instead.
  } finally {
    chrome.action.setPopup({ popup: "" }).catch(() => {});
  }
  const tabId = tab && tab.id >= 0 ? tab.id : -1;
  chrome.windows.create({ url: "action.html?tab=" + tabId, type: "popup", width: 340, height: 520 },
    () => void chrome.runtime.lastError);
}

// ---- context menus -------------------------------------------------------------
const MENU_CAPTURE = "dbox-capture", MENU_PANEL = "dbox-panel", MENU_GRAB_TAB = "dbox-grab-tab";

// One builder for every menu: removeAll() wipes them all, so a second builder
// would race this one. Rebuilt on install/update and on browser start.
function setupMenus() {
  chrome.contextMenus.removeAll(() => {
    void chrome.runtime.lastError;
    const mk = (o) => chrome.contextMenus.create(o, () => void chrome.runtime.lastError);
    mk({ id: "dbox-link", title: "Download link with D BOX", contexts: ["link"] });
    mk({ id: "dbox-video", title: "Download video with D BOX", contexts: ["video", "audio"] });
    mk({ id: "dbox-grab", title: "Grab all links from this page with D BOX", contexts: ["page", "frame", "selection", "link", "image", "video", "audio"] });
    // the toolbar icon's own right-click menu
    mk({ id: MENU_CAPTURE, title: "Capture downloads", type: "checkbox", checked: !!settings.enabled, contexts: ["action"] });
    mk({ id: MENU_PANEL, title: "Media on this page…", contexts: ["action"] });
    mk({ id: MENU_GRAB_TAB, title: "Grab all links from this page", contexts: ["action"] });
  });
}
chrome.runtime.onInstalled.addListener(() => { settingsReady.then(setupMenus); });
chrome.runtime.onStartup.addListener(() => { settingsReady.then(setupMenus); });

// The capture switch follows the setting wherever it changes (menu, panel,
// Options page); the tooltip says when capture is off.
function reflectCapture(enabled) {
  chrome.contextMenus.update(MENU_CAPTURE, { checked: !!enabled }, () => void chrome.runtime.lastError);
  try {
    chrome.action.setTitle({ title: enabled ? "Open D BOX" : "Open D BOX (download capture is off)" });
  } catch (_) {}
}
chrome.storage.onChanged.addListener((c, area) => {
  if (area === "local" && c.settings) reflectCapture((c.settings.newValue || DEFAULT_SETTINGS).enabled);
});
settingsReady.then(() => reflectCapture(settings.enabled));

chrome.contextMenus.onClicked.addListener((info, tab) => {
  if (info.menuItemId === MENU_PANEL) { openPanel(tab); return; } // first, while the click is fresh
  menuClick(info, tab).catch((e) => notify("D BOX", String((e && e.message) || e)));
});

async function menuClick(info, tab) {
  await settingsReady;
  if (info.menuItemId === MENU_CAPTURE) { // works whether or not D BOX is running
    settings.enabled = !!info.checked;
    await chrome.storage.local.set({ settings });
    return;
  }
  if (!(await pingMyIDM(0))) { notify("D BOX is not running", "Start D BOX, then try again."); return; }
  const tabId = tab && tab.id >= 0 ? tab.id : -1;
  const pageUrl = info.frameUrl || info.pageUrl || (tab && tab.url) || "";
  if (info.menuItemId === "dbox-link") {
    const url = info.linkUrl || "";
    if (!isHttp(url)) throw new Error("Only http(s) links can be downloaded.");
    const lt = String(info.linkText || "").trim();
    const title = lt && lt.length < 200 && !/^(download|click here|here|link|get|dl)$/i.test(lt) ? lt : "";
    const ctx = await pageContext(url, isHttp(pageUrl) ? pageUrl : "", "");
    const d = await postJSON("/api/prompt", Object.assign({ url, title }, ctx), 8000);
    if (!d || !d.native) openAddPopup(url, "", pageUrl, d && d.ctx, title);
  } else if (info.menuItemId === "dbox-video") {
    const src = info.srcUrl || "";
    let it = null;
    if (tabId >= 0) {
      const st = await loadTab(tabId);
      if (isHttp(src)) it = st.items.get(H.normKey(src)) || null;
      if (!it) {
        const r = (await rankedFor(tabId, { frameId: info.frameId, preferUrl: src })).filter((x) => x.kind !== "sub");
        const best = r.find((x) => x.frameId === info.frameId) || r[0];
        if (best) it = st.items.get(best.key) || null;
      }
    }
    if (!it && isHttp(src)) {
      const ext = H.pathExt(src) || "mp4";
      it = { url: H.cleanMediaUrl(src), ext, kind: H.kindOf(ext) === "seg" ? "file" : H.kindOf(ext), frameId: info.frameId || 0, frameUrl: pageUrl };
    }
    if (it) await sendStream(tab, it, info.frameId || 0, null);
    else await grabShow(tab, isHttp(pageUrl) ? pageUrl : (tab && tab.url) || ""); // nothing captured: let D BOX read the page
  } else if (info.menuItemId === "dbox-grab" || info.menuItemId === MENU_GRAB_TAB) {
    await grabShow(tab, (tab && tab.url) || pageUrl);
  }
}
