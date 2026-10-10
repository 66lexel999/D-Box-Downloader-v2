// Smoke test: loads background.js against a mocked chrome.* API and drives
// the capture pipeline (webRequest + page-hook messages), the overlay's
// "sniffed"/"promptStream" messages, the context menus, the toolbar icon (one
// click opens D BOX) and download hand-off.
// Run: node --test extension/test/background.smoke.test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const EXT = path.join(__dirname, "..");
const ev = () => { const fns = []; return { fns, addListener: (f) => fns.push(f) }; };
const calls = { fetch: [], badge: [], dl: [], menus: [], menuObjs: [], menuUpd: [], notes: [], tabsCreated: [], windows: [],
  popup: [], openPopup: [], titles: [], stored: [] };
let fetchFail = false;
let showMode = "shown"; // what POST /api/show answers: shown | headless | old | down
let openPopupFails = false;
let dboxDown = false; // every request to D BOX fails (not running)
const session = {};

const chrome = {
  runtime: { lastError: null, id: "extid", onMessage: ev(), onMessageExternal: ev(), onInstalled: ev(), onStartup: ev(),
    getURL: (p) => "chrome-extension://extid/" + p },
  storage: {
    local: { get: (k, cb) => cb({}), set: (o) => { calls.stored.push(JSON.parse(JSON.stringify(o))); return Promise.resolve(); } },
    onChanged: ev(),
    session: {
      get: async (k) => ({ [k]: session[k] }),
      set: async (o) => { Object.assign(session, JSON.parse(JSON.stringify(o))); },
      remove: async (k) => { for (const x of [].concat(k)) delete session[x]; },
    },
  },
  webRequest: { onBeforeSendHeaders: ev(), onBeforeRequest: ev(), onResponseStarted: ev() },
  downloads: {
    onDeterminingFilename: ev(),
    pause: (id, cb) => { calls.dl.push("pause"); cb(); },
    resume: (id, cb) => { calls.dl.push("resume"); cb && cb(); },
    cancel: (id, cb) => { calls.dl.push("cancel"); cb(); },
    erase: (q, cb) => { calls.dl.push("erase"); cb && cb(); },
    search: (q, cb) => cb([{ state: "in_progress" }]),
  },
  tabs: {
    onRemoved: ev(),
    query: async () => [{ id: 1, url: "https://site.x/ep1", title: "Ep 1", active: true }],
    create: (o) => { calls.tabsCreated.push(o); },
    get: async (id) => ({ id, url: "https://site.x/ep1", title: "Ep 1" }),
    sendMessage: (tabId, msg, opts, cb) => {
      const top = { ok: true, host: "site.x", siteNames: ["MySite"], cands: [{ src: "og", text: "The Real Episode 1 | MySite" }, { src: "doc", text: "Ep 1" }] };
      const emb = { ok: true, host: "byse.sx", siteNames: [], cands: [{ src: "doc", text: "4172q6dt4qljs4kfrvr7 | Byse" }] };
      setTimeout(() => cb(opts.frameId === 0 ? top : emb), 1);
    },
  },
  action: {
    setBadgeText: (o) => { calls.badge.push(o); return Promise.resolve(); },
    setBadgeBackgroundColor: () => {}, setBadgeTextColor: () => {},
    onClicked: ev(),
    setTitle: (o) => { calls.titles.push(o.title); },
    setPopup: (o) => { calls.popup.push(o.popup); return Promise.resolve(); },
    openPopup: (o) => { calls.openPopup.push(o); return openPopupFails ? Promise.reject(new Error("no")) : Promise.resolve(); },
  },
  contextMenus: {
    removeAll: (cb) => { calls.menus.length = 0; calls.menuObjs.length = 0; cb(); },
    create: (o, cb) => { calls.menus.push(o.id); calls.menuObjs.push(o); cb && cb(); },
    update: (id, o, cb) => { calls.menuUpd.push(Object.assign({ id }, o)); cb && cb(); },
    onClicked: ev(),
  },
  notifications: { create: (o, cb) => { calls.notes.push(o); cb && cb(); } },
  cookies: { getAll: (q, cb) => cb([{ name: "sid", value: "1" }]) },
  windows: { create: (o, cb) => { calls.windows.push(o); cb && cb(); }, remove: () => {} },
};

globalThis.chrome = chrome;
globalThis.self = globalThis;
globalThis.importScripts = (p) => require(path.join(EXT, p));
globalThis.fetch = async (url, init) => {
  calls.fetch.push({ url, method: (init && init.method) || "GET", body: init && init.body ? JSON.parse(init.body) : null });
  if (dboxDown) throw new Error("connection refused");
  if (fetchFail && /\/api\/prompt/.test(url)) throw new Error("refused");
  if (/\/api\/show$/.test(url)) {
    if (showMode === "down") throw new Error("connection refused");
    if (showMode === "old") return { ok: false, status: 404, json: async () => ({ error: "404 page not found" }) };
    return { ok: true, json: async () => ({ shown: showMode === "shown" }) };
  }
  return { ok: true, json: async () => ({ native: true }) };
};
vm.runInThisContext(fs.readFileSync(path.join(EXT, "background.js"), "utf8"), { filename: "background.js" });

const fire = (e, d) => e.fns.forEach((f) => f(d));
const tick = (ms = 20) => new Promise((r) => setTimeout(r, ms));
const msg = (m, sender) => new Promise((resolve) => chrome.runtime.onMessage.fns[0](m, sender, resolve));
const H = globalThis.DBoxHelpers;
const TAB = { id: 1, url: "https://site.x/ep1", title: "Ep 1" };
let rid = 0;
function response(o) {
  const id = String(++rid);
  if (o.referer || o.auth) {
    const hs = [];
    if (o.referer) hs.push({ name: "Referer", value: o.referer });
    if (o.auth) hs.push({ name: "Authorization", value: o.auth });
    hs.push({ name: "User-Agent", value: "UA" });
    fire(chrome.webRequest.onBeforeSendHeaders, { requestId: id, requestHeaders: hs });
  }
  const rh = Object.entries(o.h || {}).map(([name, value]) => ({ name, value }));
  fire(chrome.webRequest.onResponseStarted, Object.assign({ requestId: id, tabId: 1, frameId: 0, statusCode: 200, responseHeaders: rh }, o));
}

test("capture pipeline: junk/segments dropped, manifest wins, sizes from Content-Range", async () => {
  response({ type: "main_frame", url: "https://site.x/ep1", h: { "content-type": "text/html" } });
  response({ type: "media", url: "https://www.youtube.com/s/search/audio/no_input.mp3", h: { "content-type": "audio/mpeg", "content-length": "6953" } });
  response({ type: "media", url: "https://imasdk.googleapis.com/ad.mp4", h: { "content-type": "video/mp4", "content-length": "9000000" } });
  response({ type: "xmlhttprequest", frameId: 3, url: "https://h.x/ep/master.m3u8", referer: "https://byse.sx/e/abc", auth: "Bearer T",
    h: { "content-type": "application/vnd.apple.mpegurl", "content-length": "900" } });
  for (let i = 0; i < 5; i++) response({ type: "xmlhttprequest", frameId: 3, url: `https://h.x/ep/720/seg${i}.ts`, h: { "content-type": "video/mp2t", "content-length": "2000000" } });
  response({ type: "xmlhttprequest", frameId: 3, url: "https://h.x/ep/720/v12.mp4", h: { "content-type": "video/mp4", "content-length": "2000000" } });
  response({ type: "media", frameId: 3, url: "https://foupix.x/v/ep.mp4?range=0-1048575", statusCode: 206,
    h: { "content-type": "video/mp4", "content-length": "1048576", "content-range": "bytes 0-1048575/73500000" } });
  response({ type: "media", frameId: 0, url: "https://intro.x/i.mp4", h: { "content-type": "video/mp4", "content-length": "200000" } });
  // service-worker fetch attributed via initiator
  response({ type: "xmlhttprequest", tabId: -1, frameId: -1, initiator: "https://site.x", url: "https://sw.x/p/live.m3u8", h: { "content-type": "application/x-mpegurl" } });
  await tick();
  const r = await msg({ type: "sniffed" }, { tab: TAB, frameId: 3 });
  const urls = r.streams.map((s) => s.url);
  assert.ok(!urls.some((u) => /youtube|imasdk|\.ts$|v12\.mp4/.test(u)), urls.join("\n"));
  assert.equal(r.streams[0].url, "https://h.x/ep/master.m3u8");
  assert.equal(r.streams[0].size, 0); // no byte size for manifests
  const f = r.streams.find((s) => s.url.startsWith("https://foupix.x"));
  assert.equal(f.url, "https://foupix.x/v/ep.mp4"); // range param dropped
  assert.equal(f.size, 73500000);
  assert.ok(!urls.includes("https://intro.x/i.mp4"), "tiny clip hidden when bigger exists");
  assert.ok(urls.includes("https://sw.x/p/live.m3u8"), "service-worker media attributed");
});

test("hook messages: extension-less manifest, master hides variants", async () => {
  await msg({ type: "hook", msgs: [
    { t: "manifest", url: "https://cdn.y/api/source/xyz", type: "hls", master: true, height: 1080,
      variants: [{ url: "https://cdn.y/v/1080.m3u8" }], renditions: [], headers: { "X-Token": "k", Cookie: "no" } },
    { t: "player", lib: "hls.js", url: "https://cdn.y/v/1080.m3u8" },
    { t: "json", url: "https://cdn.y/api", urls: [{ url: "https://cdn.y/files/ep.mp4", ext: "mp4" }] },
    { t: "player", lib: "jwplayer", url: "javascript:alert(1)" },
  ] }, { tab: TAB, frameId: 5, url: "https://embed.y/e/1" });
  await tick();
  const r = await msg({ type: "sniffed" }, { tab: TAB, frameId: 5 });
  const urls = r.streams.map((s) => s.url);
  assert.equal(urls[0], "https://cdn.y/api/source/xyz");
  assert.match(r.streams[0].label, /^HLS stream · 1080p/);
  assert.ok(!urls.includes("https://cdn.y/v/1080.m3u8"), "variant of a known master hidden");
  assert.ok(urls.includes("https://cdn.y/files/ep.mp4"));
  assert.ok(!urls.some((u) => u.startsWith("javascript:")));
});

test("promptStream sends the player's context and the top page's title", async () => {
  calls.fetch.length = 0;
  const r = await msg({ type: "promptStream", payload: { key: H.normKey("https://h.x/ep/master.m3u8") } }, { tab: TAB, frameId: 3, url: "https://byse.sx/e/abc" });
  assert.ok(r.ok && r.native, JSON.stringify(r));
  const p = calls.fetch.find((c) => /\/api\/prompt$/.test(c.url)).body;
  assert.equal(p.url, "https://h.x/ep/master.m3u8");
  assert.equal(p.referer, "https://byse.sx/e/abc");
  assert.equal(p.headers.Authorization, "Bearer T");
  assert.equal(p.cookies, "sid=1");
  assert.equal(p.title, "The Real Episode 1");
});

test("promptVideo keeps the embed URL the probe resolved, with its referer", async () => {
  calls.fetch.length = 0;
  await msg({ type: "promptVideo", payload: { url: "https://byse.sx/e/abc", referer: "https://site.x/ep1", title: "4172q6dt4qljs4kfrvr7", selector: "best" } },
    { tab: TAB, frameId: 3, url: "https://byse.sx/e/abc" });
  const p = calls.fetch.find((c) => /\/api\/prompt$/.test(c.url)).body;
  assert.equal(p.url, "https://byse.sx/e/abc");
  assert.equal(p.referer, "https://site.x/ep1");
  assert.equal(p.title, "The Real Episode 1");
});

test("badge, context menus and Link Grabber hand-off", async () => {
  await tick(600); // debounced flush -> badge
  const last = calls.badge.filter((b) => b.tabId === 1).pop();
  assert.ok(last && +last.text > 0, JSON.stringify(calls.badge));
  fire(chrome.runtime.onInstalled);
  await tick();
  assert.deepEqual(calls.menus.slice(0, 3), ["dbox-link", "dbox-video", "dbox-grab"]);
  calls.fetch.length = 0;
  chrome.contextMenus.onClicked.fns[0]({ menuItemId: "dbox-grab", pageUrl: "https://site.x/ep1" }, TAB);
  await tick(50);
  const g = calls.fetch.find((c) => /\/api\/grab\/show$/.test(c.url));
  assert.ok(g, calls.fetch.map((c) => c.url).join(","));
  assert.equal(g.body.url, "https://site.x/ep1");
  assert.equal(g.body.title, "The Real Episode 1");
  assert.equal(g.body.cookies, "sid=1");
});

test("download hand-off: pause, send expected size/mime/title, cancel only on success", async () => {
  const item = { id: 7, url: "https://files.x/get?id=1", finalUrl: "https://cdn.files.x/a.zip", filename: "a.zip",
    mime: "application/zip", totalBytes: 6e9, referrer: "" };
  calls.fetch.length = 0; calls.dl.length = 0;
  chrome.downloads.onDeterminingFilename.fns[0](item, () => {});
  await tick(80);
  const p = calls.fetch.find((c) => /\/api\/prompt$/.test(c.url)).body;
  assert.equal(p.expectedSize, 6e9);
  assert.equal(p.expectedMime, "application/zip");
  assert.equal(p.referer, "https://site.x/ep1"); // tab fallback
  assert.equal(p.name, "a.zip");
  assert.deepEqual(calls.dl, ["pause", "cancel", "erase"]);
  // D BOX refuses -> the browser download resumes
  fetchFail = true; calls.dl.length = 0;
  chrome.downloads.onDeterminingFilename.fns[0](Object.assign({}, item, { id: 8, finalUrl: "https://cdn.files.x/b.zip", filename: "b.zip" }), () => {});
  await tick(80);
  fetchFail = false;
  assert.deepEqual(calls.dl, ["pause", "resume"]);
});

// ---- toolbar icon: one click opens D BOX; the rest is on its right-click menu ----

const shows = () => calls.fetch.filter((c) => /\/api\/show$/.test(c.url));
const menuClick = (info, tab) => chrome.contextMenus.onClicked.fns[0](info, tab);
const storageChange = (settings) =>
  chrome.storage.onChanged.fns.forEach((f) => f({ settings: { newValue: settings } }, "local"));

test("toolbar click asks the running D BOX to show its window", async () => {
  calls.fetch.length = 0; calls.tabsCreated.length = 0; calls.notes.length = 0; showMode = "shown";
  fire(chrome.action.onClicked, TAB);
  await tick();
  assert.equal(shows().length, 1);
  assert.equal(shows()[0].method, "POST");
  assert.equal(shows()[0].url, "http://127.0.0.1:8081/api/show");
  assert.equal(calls.tabsCreated.length, 0, "the window came up: no browser tab");
  assert.equal(calls.notes.length, 0);
});

test("toolbar click: headless or older D BOX opens its page in a tab", async () => {
  for (const mode of ["headless", "old"]) {
    calls.tabsCreated.length = 0; calls.notes.length = 0; showMode = mode;
    fire(chrome.action.onClicked, TAB);
    await tick();
    assert.deepEqual(calls.tabsCreated, [{ url: "http://127.0.0.1:8081/" }], mode);
    assert.equal(calls.notes.length, 0, mode);
  }
  showMode = "shown";
});

test("toolbar click: D BOX not running shows a notification, no tab", async () => {
  calls.tabsCreated.length = 0; calls.notes.length = 0; showMode = "down";
  fire(chrome.action.onClicked, TAB);
  await tick();
  showMode = "shown";
  assert.equal(calls.tabsCreated.length, 0);
  assert.equal(calls.notes.length, 1);
  assert.equal(calls.notes[0].title, "D BOX is not running");
});

test("one menu builder: page menus and the icon's own menu together", async () => {
  fire(chrome.runtime.onStartup);
  await tick();
  assert.deepEqual(calls.menus, ["dbox-link", "dbox-video", "dbox-grab", "dbox-capture", "dbox-panel", "dbox-grab-tab"]);
  const action = calls.menuObjs.filter((o) => (o.contexts || []).includes("action"));
  assert.deepEqual(action.map((o) => o.id), ["dbox-capture", "dbox-panel", "dbox-grab-tab"]);
  assert.ok(action.every((o) => o.contexts.length === 1), "icon items stay off the page menus");
  assert.equal(action[0].type, "checkbox");
  assert.equal(action[0].checked, true);
  assert.ok(action.length <= 6, "Chrome shows at most 6 top-level icon-menu items");
});

test("Capture downloads switch works while D BOX is down, and is reflected", async () => {
  calls.stored.length = 0; calls.notes.length = 0; calls.menuUpd.length = 0; calls.titles.length = 0; dboxDown = true;
  menuClick({ menuItemId: "dbox-capture", checked: false }, TAB);
  await tick();
  dboxDown = false;
  assert.equal(calls.notes.length, 0, "no 'not running' complaint for a local switch");
  assert.equal(calls.stored.length, 1);
  assert.equal(calls.stored[0].settings.enabled, false);
  storageChange(calls.stored[0].settings);
  assert.deepEqual(calls.menuUpd.pop(), { id: "dbox-capture", checked: false });
  assert.equal(calls.titles.pop(), "Open D BOX (download capture is off)");
  storageChange({ enabled: true, myidmBase: "http://127.0.0.1:8081" });
  assert.deepEqual(calls.menuUpd.pop(), { id: "dbox-capture", checked: true });
  assert.equal(calls.titles.pop(), "Open D BOX");
});

test("'Media on this page' opens the panel anchored to the icon, then clears the popup", async () => {
  calls.popup.length = 0; calls.openPopup.length = 0; calls.windows.length = 0; openPopupFails = false;
  menuClick({ menuItemId: "dbox-panel" }, Object.assign({ windowId: 9 }, TAB));
  await tick();
  assert.deepEqual(calls.popup, ["action.html", ""], "a later click must reach onClicked again");
  assert.deepEqual(calls.openPopup, [{ windowId: 9 }]);
  assert.equal(calls.windows.length, 0);
});

test("'Media on this page' falls back to a small window for the same tab", async () => {
  calls.popup.length = 0; calls.windows.length = 0; openPopupFails = true;
  menuClick({ menuItemId: "dbox-panel" }, Object.assign({ windowId: 9 }, TAB));
  await tick();
  openPopupFails = false;
  assert.deepEqual(calls.popup, ["action.html", ""]);
  assert.equal(calls.windows.length, 1);
  assert.equal(calls.windows[0].url, "action.html?tab=1");
  assert.equal(calls.windows[0].type, "popup");
});

test("icon menu 'Grab all links from this page' opens the Link Grabber for the tab", async () => {
  calls.fetch.length = 0;
  menuClick({ menuItemId: "dbox-grab-tab" }, TAB); // icon clicks carry no pageUrl
  await tick(50);
  const g = calls.fetch.find((c) => /\/api\/grab\/show$/.test(c.url));
  assert.ok(g, calls.fetch.map((c) => c.url).join(","));
  assert.equal(g.body.url, "https://site.x/ep1");
});

test("panel's Open D BOX button brings the window up through the worker", async () => {
  calls.fetch.length = 0; calls.tabsCreated.length = 0; showMode = "shown";
  const r = await msg({ type: "openApp" }, {});
  assert.ok(r.ok);
  assert.equal(shows().length, 1);
  assert.equal(calls.tabsCreated.length, 0);
});

test("panel opened as a small window acts on the tab it names, not on itself", async () => {
  const panelUrl = "chrome-extension://extid/action.html?tab=1";
  const sender = { tab: { id: 77, url: panelUrl, title: "D BOX Integration" }, url: panelUrl };
  const r = await msg({ type: "sniffed", tabId: 1 }, sender);
  assert.ok(r.ok && r.streams.length > 0, "the page's captured media, not the panel's (none)");
  const urls = r.streams.map((x) => x.url);
  assert.ok(urls.includes("https://h.x/ep/master.m3u8") && urls.includes("https://cdn.y/api/source/xyz"), urls.join(" | "));
  calls.fetch.length = 0;
  const g = await msg({ type: "grabPage", tabId: 1 }, sender);
  assert.ok(g.ok, JSON.stringify(g));
  const req = calls.fetch.find((c) => /\/api\/grab\/show$/.test(c.url));
  assert.equal(req.body.url, "https://site.x/ep1");
});
