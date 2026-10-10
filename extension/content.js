// D BOX grab overlay: a floating "Download this video" button (IDM-style) on
// pages with a video, whose dropdown lists qualities from D BOX's yt-dlp probe
// (or the streams the page loaded); plus a hover "download" badge on large
// images. All network work is proxied through the background worker (which
// holds the D BOX host permission).
//
// The UI lives in a closed SHADOW ROOT. It used to be plain elements with class
// names like .row/.close/.msg added to the page, so the page's own CSS styled
// it too — Bootstrap's `.row` (negative side margins + flex-wrap) gave the menu
// a horizontal scrollbar and split "1." and the label onto separate lines, and
// right-to-left pages flipped and clipped it. Page CSS can't reach inside a
// shadow root.
(function () {
  "use strict";
  const isTop = window.top === window;
  const H = globalThis.DBoxHelpers; // lib/helpers.js, loaded before this file
  // about:blank / srcdoc / blob: frames (match_about_blank) have no address
  // yt-dlp could read; they still relay hook findings and list captures.
  const httpPage = /^https?:/i.test(location.href);
  // Runs in ALL frames (all_frames:true): an iframe-embedded player (uqload,
  // streamtape, dood…) gets the button too and probes its own frame URL.
  // Frames too small to hold a player (ads, pixels) get no button — checked in
  // tick(), since players are often sized or revealed by script later.
  const frameTooSmall = () => !isTop && innerWidth > 0 && innerHeight > 0 && (innerWidth < 320 || innerHeight < 180);

  // ---- relay from the MAIN-world page hook (hook.js) --------------------------
  // hook.js announces a random nonce ("dbox-hello"); we acknowledge it and
  // listen on "dbox-<nonce>". Findings (manifests in text/JSON bodies, player
  // library sources, MSE use) are batched to the service worker; player titles
  // (JW Player / Brightcove) stay here as title candidates.
  const playerTitles = [];
  let hookEvt = "", hookQ = [], hookT = 0;
  function onHookEvent(e) {
    let m;
    try { m = JSON.parse(typeof e.detail === "string" ? e.detail : ""); } catch (_) { return; }
    if (!m || typeof m !== "object" || typeof m.t !== "string") return;
    if (m.t === "title") {
      const t = String(m.title || "").slice(0, 300).trim();
      if (t && !playerTitles.includes(t)) { playerTitles.unshift(t); if (playerTitles.length > 5) playerTitles.pop(); }
      return;
    }
    if (hookQ.length < 300) hookQ.push(m);
    if (!hookT) hookT = setTimeout(flushHook, 250);
  }
  function flushHook() {
    hookT = 0;
    const msgs = hookQ; hookQ = [];
    if (msgs.length) send({ type: "hook", msgs });
  }
  document.addEventListener("dbox-hello", (e) => {
    const n = typeof e.detail === "string" ? e.detail : "";
    if (hookEvt || !/^[a-z0-9]{8,80}$/.test(n)) return;
    hookEvt = "dbox-" + n;
    document.addEventListener(hookEvt, onHookEvent);
    document.dispatchEvent(new CustomEvent("dbox-ack-" + n));
  });
  document.dispatchEvent(new CustomEvent("dbox-ping")); // the hook re-announces (it ran long before us)

  // ---- title candidates ------------------------------------------------------------
  // What this frame knows about the video's name, best sources first; the
  // service worker combines the asking frame's and the top frame's lists.
  function collectTitles() {
    const cands = [];
    const add = (src, text) => {
      text = String(text || "").replace(/\s+/g, " ").trim();
      if (text && text.length <= 300 && !cands.some((c) => c.text === text && c.src === src)) cands.push({ src, text });
    };
    const meta = (sel) => { const el = document.querySelector(sel); return (el && el.getAttribute("content")) || ""; };
    for (const t of playerTitles) add("player", t);
    try {
      for (const s of document.querySelectorAll('script[type="application/ld+json"]')) {
        const txt = s.textContent || "";
        if (txt.length > 300000) continue;
        let j;
        try { j = JSON.parse(txt); } catch (_) { continue; }
        const stack = [j];
        for (let n = 0; stack.length && n < 300; n++) {
          const o = stack.pop();
          if (!o || typeof o !== "object") continue;
          if (Array.isArray(o)) { stack.push(...o.slice(0, 50)); continue; }
          const ty = [].concat(o["@type"] || []).join(" ");
          if (/VideoObject|Movie|Episode|Clip|AudioObject|MusicRecording|BroadcastEvent/i.test(ty) && typeof o.name === "string") add("jsonld", o.name);
          if (o["@graph"]) stack.push(o["@graph"]);
          if (o.video && typeof o.video === "object") stack.push(o.video);
        }
      }
    } catch (_) {}
    add("og", meta('meta[property="og:title"]'));
    add("twitter", meta('meta[name="twitter:title"],meta[property="twitter:title"]'));
    add("doc", document.title);
    const h1 = document.querySelectorAll("h1");
    if (h1.length === 1) add("h1", h1[0].textContent);
    if (curVideo) add("video", curVideo.getAttribute("title") || curVideo.getAttribute("aria-label"));
    const siteNames = [meta('meta[property="og:site_name"]'), meta('meta[name="application-name"]')].filter(Boolean);
    return { ok: true, cands: H.orderTitleCands(cands), host: location.hostname, siteNames };
  }
  try {
    chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
      if (msg && msg.type === "getTitles") sendResponse(collectTitles());
    });
  } catch (_) {}

  // Palette = the app's default "Slate" theme (internal/server/web/ui/theme.css):
  // surfaces #262b32/#2d333b, border #3a414b, text #e3e7ed, brand green #2fcc66.
  const CSS = `
:host { all: initial; }
* { box-sizing: border-box; }
.vbtn {
  position: fixed; display: none; align-items: center; gap: 5px;
  padding: 3px 4px 3px 4px; margin: 0;
  font: 12px/1.2 "Segoe UI", Tahoma, sans-serif; color: #e3e7ed; direction: ltr;
  background: linear-gradient(#2d333b, #262b32);
  border: 1px solid #3a414b; border-radius: 8px;
  box-shadow: 0 2px 8px rgba(0,0,0,.38), inset 0 1px 0 rgba(255,255,255,.05);
  cursor: pointer; user-select: none; white-space: nowrap; touch-action: none;
  transition: border-color .12s, box-shadow .12s;
}
.vbtn:hover { border-color: #2fcc66; }
.vbtn.dragging { cursor: grabbing; opacity: .92; box-shadow: 0 6px 18px rgba(0,0,0,.5); }
.vbtn .play { width: 20px; height: 20px; display: block; flex: none; pointer-events: none;
  filter: drop-shadow(0 1px 1px rgba(0,0,0,.3)); }
.vbtn .x {
  display: inline-grid; place-items: center; width: 16px; height: 16px; margin-left: 1px;
  border: 1px solid #3a414b; border-radius: 5px; background: #323843; color: #c0c7d1;
  line-height: 0;
}
.vbtn .x svg { width: 8px; height: 8px; display: block; pointer-events: none; }
.vbtn .x:hover { background: #3a414b; color: #fff; }
/* Formats prefetched and ready: green outline. */
.vbtn.ready { border-color: #2fcc66; box-shadow: 0 2px 8px rgba(0,0,0,.38), 0 0 0 2px rgba(47,204,102,.28); }

.menu {
  position: fixed; direction: ltr; text-align: left;
  width: max-content; min-width: min(280px, calc(100vw - 16px)); max-width: min(520px, calc(100vw - 16px));
  max-height: min(70vh, calc(100vh - 16px)); overflow-x: hidden; overflow-y: auto;
  background: #2d333b; color: #e3e7ed; border: 1px solid #3a414b; border-radius: 10px;
  box-shadow: 0 14px 40px rgba(0,0,0,.5), 0 2px 8px rgba(0,0,0,.3);
  font: 12px/1.4 "Segoe UI", Tahoma, sans-serif; padding: 5px; margin: 0;
  scrollbar-width: thin; scrollbar-color: #4d5562 transparent;
}
.head {
  padding: 6px 8px 7px 20px; margin: 0 0 4px; color: #ffffff; font-weight: 600;
  border-bottom: 1px solid #3a414b; white-space: normal; overflow-wrap: anywhere; position: relative;
}
/* brand accent (pure CSS: a data: image could be blocked by the page's CSP) */
.head::before { content: ""; position: absolute; left: 8px; top: 8px; bottom: 9px; width: 4px; border-radius: 2px;
  background: linear-gradient(#3fd97a, #1f9d52); }
.sub { padding: 0 8px 4px; font-size: 11px; color: #8b95a2; }
.item { display: flex; align-items: baseline; gap: 8px; padding: 6px 8px; cursor: pointer; min-width: 0;
  border-radius: 6px; }
.item:hover { background: rgba(47,204,102,.16); color: #fff; box-shadow: inset 2px 0 0 #2fcc66; }
.item .n { color: #8b95a2; flex: none; min-width: 16px; font-variant-numeric: tabular-nums; }
.item:hover .n { color: #2fcc66; }
.item .lbl { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.item .sz { flex: none; padding-left: 10px; color: #8b95a2; font-variant-numeric: tabular-nums; }
.item:hover .sz { color: #c0c7d1; }
.note { padding: 8px; color: #c0c7d1; white-space: normal; overflow-wrap: anywhere; }
.note.err { color: #f0716d; }
.foot { border-top: 1px solid #3a414b; margin-top: 4px; padding: 6px 8px 3px; font-size: 11px; color: #8b95a2; cursor: pointer; }
.foot:hover { color: #fff; }

.imgbtn {
  position: fixed; display: none; place-items: center; width: 28px; height: 28px;
  background: rgba(38,43,50,.92); color: #fff; font: 600 14px/1 "Segoe UI", sans-serif;
  border: 1px solid #3a414b; border-radius: 8px; cursor: pointer;
  box-shadow: 0 2px 8px rgba(0,0,0,.45);
  transition: border-color .12s, background-color .12s;
}
.imgbtn:hover { background: #2d333b; border-color: #2fcc66; }
.imgbtn svg { width: 20px; height: 20px; display: block; pointer-events: none; }
`;
  // The D BOX download glyph (green tile + arrow), inline so no image request
  // is needed and it stays crisp at any zoom.
  const DL_SVG = '<svg class="play" viewBox="0 0 20 20" aria-hidden="true">' +
    '<rect x="1" y="1" width="18" height="18" rx="5.5" fill="#2fcc66"/>' +
    '<rect x="1" y="1" width="18" height="9" rx="5.5" fill="#fff" opacity=".12"/>' +
    '<path d="M10 5v7.2M6.9 9.3 10 12.4l3.1-3.1M6.2 15h7.6" fill="none" stroke="#fff" stroke-width="1.9" ' +
    'stroke-linecap="round" stroke-linejoin="round"/></svg>';
  const X_SVG = '<svg viewBox="0 0 8 8" aria-hidden="true"><path d="M1 1l6 6M7 1 1 7" stroke="currentColor" ' +
    'stroke-width="1.5" stroke-linecap="round"/></svg>';

  // ---- shadow host --------------------------------------------------------
  let host = null, root = null;
  function ui() {
    if (root) return root;
    host = document.createElement("dbox-overlay");
    // Pin the host itself against page CSS (!important beats non-important page rules).
    for (const [k, v] of [["all", "initial"], ["position", "fixed"], ["top", "0"], ["left", "0"],
      ["width", "0"], ["height", "0"], ["z-index", "2147483647"], ["display", "block"]]) {
      host.style.setProperty(k, v, "important");
    }
    root = host.attachShadow({ mode: "closed" });
    const st = document.createElement("style");
    st.textContent = CSS;
    root.appendChild(st);
    document.documentElement.appendChild(host);
    return root;
  }
  // inUI reports whether an event happened inside our overlay (events from a
  // shadow root reach document listeners retargeted to the host).
  const inUI = (e) => !!host && e.composedPath().includes(host);

  let btn = null, menu = null, curVideo = null, dismissed = false, lastUrl = location.href;
  let curResolved = location.href; // the URL the successful probe actually resolved (frame or top page)
  let curReferer = "";             // the Referer that probe used (the tab URL for an embed)
  let upCache = { val: false, ts: 0 };
  // Prefetched probe results, keyed by URL, so clicking opens the quality menu
  // instantly instead of waiting on yt-dlp. Failures retry after a cooldown;
  // successes expire after a few minutes (the page may have changed video).
  const probeCache = new Map(); // url -> { p, ts, ok, msg }
  const PROBE_TTL = 5 * 60 * 1000;

  function warm(url) {
    const e = probeCache.get(url);
    const age = e ? Date.now() - e.ts : 0;
    if (e && (e.ok === undefined || (e.ok && age < PROBE_TTL) || (!e.ok && age < 12000))) return e.p;
    if (probeCache.size > 40) probeCache.clear();
    const entry = { p: null, ts: Date.now(), ok: undefined, msg: null };
    entry.p = send({ type: "probe", url }).then((r) => {
      entry.ok = !!(r && r.ok);
      entry.ts = Date.now();
      entry.msg = r;
      if (entry.ok) {
        curResolved = (r && r.resolved) || url;
        curReferer = (r && r.referer) || "";
        if (url === location.href) markReady();
      }
      return r;
    });
    probeCache.set(url, entry);
    return entry.p;
  }
  function cachedMsg(url) {
    const e = probeCache.get(url);
    return e && e.ok && Date.now() - e.ts < PROBE_TTL ? e.msg : null;
  }
  function markReady() {
    if (!btn) return;
    btn.classList.add("ready");
    btn.title = "Formats ready — click to pick a quality · drag to move";
  }
  function clearReady() { if (btn) btn.classList.remove("ready"); }

  function send(msg) {
    return new Promise((resolve) => {
      try { chrome.runtime.sendMessage(msg, (r) => resolve(r || { ok: false })); }
      catch { resolve({ ok: false }); }
    });
  }
  async function dboxUp() {
    const now = Date.now();
    if (now - upCache.ts < 5000) return upCache.val;
    const r = await send({ type: "status" });
    upCache = { val: !!(r && r.ok), ts: now };
    return upCache.val;
  }
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }
  const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
  const isFullscreen = () => !!(document.fullscreenElement || document.webkitFullscreenElement);

  // ---- video discovery ------------------------------------------------------
  // Some players render <video> inside (open) shadow roots. Those are always
  // scanned too — a small ad <video> in the light DOM must not hide the real
  // player — but only every few seconds, since it walks the whole tree.
  function videosInShadow(r, out, depth) {
    if (depth > 6) return;
    for (const el of r.querySelectorAll("*")) {
      const sr = el.shadowRoot;
      if (!sr) continue;
      for (const v of sr.querySelectorAll("video")) out.push(v);
      videosInShadow(sr, out, depth + 1);
    }
  }
  let shadowVids = [], shadowTs = 0;
  function largestVideo() {
    const vids = Array.from(document.querySelectorAll("video"));
    const now = Date.now();
    if (now - shadowTs > 3000) {
      shadowTs = now; shadowVids = [];
      try { videosInShadow(document, shadowVids, 0); } catch (_) {}
    }
    for (const v of shadowVids) if (v.isConnected) vids.push(v);
    let best = null, bestArea = 0;
    for (const v of vids) {
      const r = v.getBoundingClientRect();
      if (r.width < 220 || r.height < 140) continue;
      if (r.bottom < 0 || r.top > innerHeight) continue;
      const area = r.width * r.height;
      if (area > bestArea) { best = v; bestArea = area; }
    }
    return best;
  }

  // ---- the button: click opens the menu, drag moves it ---------------------
  // Its position is remembered per site, relative to the video (fractions of
  // the video's box), so it stays where you put it as the player resizes.
  const posKey = "btnpos:" + location.hostname;
  let pos = null;       // { fx, fy } center of the button within the video, 0..1
  let drag = null;      // active drag state
  let suppressClick = false;
  try { chrome.storage.local.get(posKey, (r) => { if (r && r[posKey]) pos = r[posKey]; }); } catch (_) {}

  function ensureButton() {
    if (btn) return btn;
    btn = document.createElement("div");
    btn.className = "vbtn";
    btn.innerHTML =
      DL_SVG + '<span class="x" title="Hide">' + X_SVG + "</span>";
    btn.title = "Download this video — click to pick a quality · drag to move";
    btn.addEventListener("click", onOpen);
    btn.querySelector(".x").addEventListener("click", (e) => {
      e.stopPropagation(); dismissed = true; hideAll();
    });
    btn.addEventListener("pointerdown", dragStart);
    btn.addEventListener("pointermove", dragMove);
    btn.addEventListener("pointerup", dragEnd);
    btn.addEventListener("pointercancel", dragEnd);
    ui().appendChild(btn);
    return btn;
  }

  function dragStart(e) {
    if (e.button !== 0 || e.target.closest(".x")) return;
    const r = btn.getBoundingClientRect();
    drag = { sx: e.clientX, sy: e.clientY, ox: r.left, oy: r.top, moved: false };
    try { btn.setPointerCapture(e.pointerId); } catch (_) {}
  }
  function dragMove(e) {
    if (!drag) return;
    const dx = e.clientX - drag.sx, dy = e.clientY - drag.sy;
    if (!drag.moved && Math.hypot(dx, dy) < 4) return; // a click, not a drag (yet)
    if (!drag.moved) { drag.moved = true; btn.classList.add("dragging"); closeMenu(); }
    btn.style.left = clamp(drag.ox + dx, 2, innerWidth - btn.offsetWidth - 2) + "px";
    btn.style.top = clamp(drag.oy + dy, 2, innerHeight - btn.offsetHeight - 2) + "px";
  }
  function dragEnd(e) {
    if (!drag) return;
    const moved = drag.moved;
    drag = null;
    btn.classList.remove("dragging");
    try { btn.releasePointerCapture(e.pointerId); } catch (_) {}
    if (!moved) return;
    suppressClick = true; // the click that follows this pointerup is the drop, not "open"
    setTimeout(() => { suppressClick = false; }, 350);
    if (curVideo) {
      const v = curVideo.getBoundingClientRect(), b = btn.getBoundingClientRect();
      if (v.width > 0 && v.height > 0) {
        pos = {
          fx: clamp((b.left + b.width / 2 - v.left) / v.width, 0, 1),
          fy: clamp((b.top + b.height / 2 - v.top) / v.height, 0, 1),
        };
        try { chrome.storage.local.set({ [posKey]: pos }); } catch (_) {}
      }
    }
  }
  function resetPosition() {
    pos = null;
    try { chrome.storage.local.remove(posKey); } catch (_) {}
    if (curVideo) positionButton(curVideo);
  }

  function positionButton(v) {
    if (drag) return; // never fight the user's drag
    const r = v.getBoundingClientRect();
    const w = btn.offsetWidth || 52, h = btn.offsetHeight || 24;
    let left, top;
    if (pos) {
      left = r.left + pos.fx * r.width - w / 2;
      top = r.top + pos.fy * r.height - h / 2;
    } else { // default: the video's top-right corner
      left = r.right - w - 6;
      top = r.top + 8;
    }
    btn.style.left = clamp(left, 2, innerWidth - w - 2) + "px";
    btn.style.top = clamp(top, 2, innerHeight - h - 2) + "px";
    if (menu) placeMenu();
  }

  // ---- the menu -------------------------------------------------------------
  function closeMenu() { if (menu) { menu.remove(); menu = null; } }
  function hideAll() { if (btn) btn.style.display = "none"; closeMenu(); }

  // placeMenu keeps the menu fully inside the window: under the button when it
  // fits, above it otherwise, never past the left/right edge.
  function placeMenu() {
    if (!menu || !btn) return;
    const r = btn.getBoundingClientRect();
    const mw = menu.offsetWidth, mh = menu.offsetHeight;
    const left = clamp(r.left, 8, Math.max(8, innerWidth - mw - 8));
    let top = r.bottom + 4;
    if (top + mh > innerHeight - 8 && r.top - mh - 4 >= 8) top = r.top - mh - 4;
    menu.style.left = left + "px";
    menu.style.top = clamp(top, 8, Math.max(8, innerHeight - mh - 8)) + "px";
  }
  function setMenu(html) {
    if (!menu) return;
    menu.innerHTML = html + (pos ? '<div class="foot" data-reset="1">&#8634; Reset button position</div>' : "");
    const f = menu.querySelector("[data-reset]");
    if (f) f.addEventListener("click", () => { closeMenu(); resetPosition(); });
    placeMenu();
  }
  // Titles and labels may be Arabic/Hebrew: dir=auto lays each out in its own
  // direction inside the left-to-right menu.
  const headHTML = (t) => '<div class="head" dir="auto">' + esc(t) + "</div>";
  const itemHTML = (attr, i, label, size) =>
    '<div class="item" ' + attr + '><span class="n">' + (i + 1) + '.</span><span class="lbl" dir="auto">' +
    esc(label) + "</span>" + (size ? '<span class="sz">' + size + "</span>" : "") + "</div>";

  const curSrc = () => (curVideo && /^https?:/i.test(curVideo.currentSrc || "") ? curVideo.currentSrc : "");
  const curDur = () => (curVideo && isFinite(curVideo.duration) && curVideo.duration > 0 ? curVideo.duration : 0);

  // onOpen shows ONE menu with both sources: the formats D BOX's yt-dlp probe
  // found for the page, and the media the page actually loaded (captured off
  // the network / by the page hook). The network list is shown at once; the
  // yt-dlp section fills in when the probe answers.
  async function onOpen(e) {
    e.stopPropagation();
    if (suppressClick) { suppressClick = false; return; }
    if (menu) { closeMenu(); return; }
    menu = document.createElement("div");
    menu.className = "menu";
    ui().appendChild(menu);
    setMenu('<div class="note">Looking for formats…</div>');

    const probeP = httpPage ? warm(location.href) : Promise.resolve(null);
    const sn = await send({ type: "sniffed", preferUrl: curSrc(), duration: curDur() });
    if (!menu) return;
    const streams = (sn && sn.streams) || [];
    const cached = httpPage ? cachedMsg(location.href) : null;
    if (cached || !httpPage) { renderAll(cached, streams, false); return; }
    renderAll(null, streams, true);
    const res = await probeP;
    if (!menu) return;
    renderAll(res, streams, false);
  }

  function probeError(res) {
    const err = (res && res.error) || "";
    const ig = /instagram\.com/i.test(location.href);
    if (/not found|not installed/i.test(err)) return "yt-dlp isn’t installed — put yt-dlp.exe next to DBox.exe.";
    if (!res || !err || /failed to fetch|networkerror|connection|refused/i.test(err)) return "Couldn’t reach D BOX — make sure it’s running.";
    if (ig && /log ?in|login|sign ?in|cookies|authentication|rate.?limit|restricted|empty|not available|unavailable|unreachable/i.test(err)) {
      return "Instagram wouldn’t authorize this. Make sure you’re logged into Instagram in Brave (open instagram.com), then try again — stories also expire after 24h.";
    }
    if (/private|not available|unavailable|deleted|removed|age|restricted/i.test(err)) {
      return "This video can’t be read — it may be private, age-restricted, deleted, or region-locked.";
    }
    return "Couldn’t read this video: play it for a moment (D BOX watches the network), then click here again. " + err;
  }

  // pageTitle is the best name this frame can give the video (player title,
  // JSON-LD, og:title… with " | Site" suffixes stripped, random codes skipped).
  function pageTitle() {
    const t = collectTitles();
    return H.pickTitle(t.cands, { host: t.host, siteNames: t.siteNames }) ||
      (document.title || "video").replace(/\s+-\s+YouTube$/, "").trim();
  }

  // renderAll draws the menu. The yt-dlp section goes first unless its answer
  // looks wrong for what's playing: the generic extractor (often an intro or
  // og:video clip on uncommon sites) or a duration >10% off the <video>'s —
  // then the network list leads.
  function renderAll(res, streams, pending) {
    const info = res && res.ok && res.result ? res.result : null;
    const opts = (info && info.options) || [];
    let title = pageTitle();
    if (info && info.title) {
      const yt = H.cleanTitle(info.title, location.hostname);
      if (yt && !H.isIdLikeTitle(yt, [location.href], location.hostname) && (!curResolved || curResolved === location.href || !title)) title = yt;
    }
    const vd = curDur();
    const mismatch = !!(info && info.duration > 0 && vd > 0 && Math.abs(info.duration - vd) / vd > 0.1);
    const generic = !!(info && /^generic$/i.test(info.extractor || ""));
    const netFirst = streams.length > 0 && (mismatch || generic || !opts.length);

    let ytHTML = "";
    if (pending) ytHTML = '<div class="note">Asking D BOX (yt-dlp) for formats…</div>';
    else if (opts.length) {
      const extra = [info.extractor && !generic ? info.extractor : "", info.isLive ? "LIVE" : H.fmtDuration(info.duration)].filter(Boolean).join(" · ");
      ytHTML = '<div class="sub">Found by D BOX' + (extra ? " · " + esc(extra) : "") + (mismatch ? " · length differs from the playing video" : "") + "</div>";
      opts.forEach((o, i) => {
        const lbl = String(o.label || "").replace(/\s*\(~[^)]*\)\s*$/, ""); // size has its own column
        ytHTML += itemHTML('data-i="' + i + '"', i, lbl, H.humanBytes(o.size));
      });
    } else if (!streams.length) {
      ytHTML = info ? '<div class="note">No downloadable formats found for this video.</div>'
        : '<div class="note err">' + esc(httpPage ? probeError(res) : "Play the video for a moment (D BOX watches the network), then click here again.") + "</div>";
    }

    let netHTML = "";
    if (streams.length) {
      netHTML = '<div class="sub">Detected on the network</div>';
      streams.forEach((s, i) => {
        netHTML += itemHTML('data-si="' + i + '"', opts.length + i, s.label || s.ext.toUpperCase(),
          s.kind === "hls" ? "" : H.humanBytes(s.size));
      });
    }
    setMenu(headHTML(title) + (netFirst ? netHTML + ytHTML : ytHTML + netHTML));
    menu.querySelectorAll("[data-i]").forEach((row) =>
      row.addEventListener("click", () => pick(info, +row.dataset.i, info.title || title)));
    menu.querySelectorAll("[data-si]").forEach((row) =>
      row.addEventListener("click", () => pickSniffed(streams[+row.dataset.si])));
  }

  function showResult(res) {
    if (!menu) return;
    const ok = res && (res.native || res.ok);
    setMenu('<div class="note' + (ok ? "" : " err") + '">' +
      (res && res.native ? "Opening download window…"
        : ok ? "Sent to D BOX ✓"
        : ("Failed: " + esc((res && res.error) || "error"))) + "</div>");
    setTimeout(closeMenu, ok ? 1500 : 3000);
  }

  // pickSniffed hands a captured stream to D BOX WITH the context the browser
  // used to fetch it (the worker keeps the Referer/Origin/custom headers the
  // player sent, keyed by the capture), plus the site's cookies and the
  // browser's User-Agent. Streams go to D BOX's New Download window, which
  // downloads HLS natively and DASH via yt-dlp. Subtitles queue directly.
  async function pickSniffed(s) {
    setMenu('<div class="note">Opening…</div>');
    showResult(await send({ type: "promptStream", payload: {
      key: s.key, url: s.url, kind: s.kind, ext: s.ext, frameUrl: location.href, titles: collectTitles(),
    } }));
  }

  // pick downloads a yt-dlp format of the URL the probe resolved, with the
  // Referer the probe used (an embed frame keeps its own URL).
  async function pick(info, i, title) {
    const o = info.options[i];
    setMenu('<div class="note">Opening…</div>');
    showResult(await send({
      type: "promptVideo",
      payload: {
        url: curResolved || location.href, referer: curReferer, title, titles: collectTitles(),
        selector: o.selector, ext: o.ext, audio: !!o.audio, size: o.size || 0,
      },
    }));
  }

  async function tick() {
    const changed = location.href !== lastUrl;
    if (changed) {
      lastUrl = location.href; dismissed = false; closeMenu(); clearReady();
      curResolved = location.href; curReferer = "";
      if (isTop) send({ type: "navigated" }); // SPA route change: older captures rank lower
    }
    // hide while a video is fullscreen, or in a frame too small for a player
    const v = (dismissed || isFullscreen() || frameTooSmall()) ? null : largestVideo();
    if (!v || !(await dboxUp())) { hideAll(); curVideo = v; return; }
    ensureButton();
    btn.style.display = "inline-flex";
    curVideo = v;
    positionButton(v);
    // Let the URL settle one tick after a SPA navigation, then prefetch formats.
    if (!changed && httpPage) warm(location.href);
  }
  setInterval(tick, 900);
  document.addEventListener("fullscreenchange", () => tick());
  document.addEventListener("webkitfullscreenchange", () => tick());
  addEventListener("scroll", (e) => {
    if (menu && inUI(e)) return; // scrolling the menu's own list
    if (btn && curVideo && btn.style.display !== "none") positionButton(curVideo);
  }, true);
  addEventListener("resize", () => { if (btn && curVideo) positionButton(curVideo); });
  addEventListener("click", (e) => { if (menu && !inUI(e)) closeMenu(); }, true);

  // ---- image hover download ------------------------------------------------
  let imgBtn = null, hovImg = null;
  function ensureImgBtn() {
    if (imgBtn) return imgBtn;
    imgBtn = document.createElement("div");
    imgBtn.className = "imgbtn";
    imgBtn.title = "Download image with D BOX";
    const ico = DL_SVG;
    imgBtn.innerHTML = ico;
    imgBtn.addEventListener("click", async (e) => {
      e.stopPropagation(); e.preventDefault();
      if (!hovImg) return;
      const url = bestImgSrc(hovImg);
      if (!url || /^data:|^blob:/.test(url)) return;
      imgBtn.textContent = "…";
      // An explicit file name only when the address has one with an extension
      // ("images?q=…" would become an extension-less "images"); otherwise D BOX
      // names the file from the alt text / page title and the Content-Type.
      let name = url.split("?")[0].split("#")[0].split("/").pop() || "";
      try { name = decodeURIComponent(name); } catch (_) {}
      const payload = { url, category: "Images", referer: location.href, title: (hovImg.alt || "").trim() || pageTitle() };
      if (/\.[a-z0-9]{2,5}$/i.test(name)) payload.fileName = name;
      const res = await send({ type: "queueFile", payload });
      imgBtn.textContent = res.ok ? "✓" : "✕";
      setTimeout(() => { if (imgBtn) imgBtn.innerHTML = ico; }, 1000);
    });
    ui().appendChild(imgBtn);
    return imgBtn;
  }
  // bestImgSrc picks the largest srcset / <picture> candidate, not the
  // thumbnail-sized one the page happens to display.
  function bestImgSrc(img) {
    let best = img.currentSrc || img.src || "", bw = 0;
    const scan = (set) => {
      for (const part of String(set || "").split(/,\s+/)) {
        const m = /^(\S+)\s+(\d+)w$/.exec(part.trim());
        if (m && +m[2] > bw) { try { best = new URL(m[1], location.href).href; bw = +m[2]; } catch (_) {} }
      }
    };
    scan(img.getAttribute("srcset"));
    const pic = img.parentElement && img.parentElement.tagName === "PICTURE" ? img.parentElement : null;
    if (pic) for (const src of pic.querySelectorAll("source")) scan(src.getAttribute("srcset"));
    return best;
  }
  function showImgBtn(img) {
    hovImg = img;
    ensureImgBtn();
    const r = img.getBoundingClientRect();
    imgBtn.style.display = "grid";
    imgBtn.style.top = (r.top + 6) + "px";
    imgBtn.style.left = (r.left + 6) + "px";
  }
  function hideImgBtn() { if (imgBtn) imgBtn.style.display = "none"; hovImg = null; }
  const ptIn = (r, x, y) => x >= r.left && x <= r.right && y >= r.top && y <= r.bottom;

  // Keep the "D BOX up?" flag warm so the hover check below is synchronous.
  // Image hover is a TOP-frame feature only.
  const refreshUp = () => send({ type: "status" }).then((r) => { upCache = { val: !!(r && r.ok), ts: Date.now() }; });
  if (isTop) { refreshUp(); setInterval(refreshUp, 5000); }

  // Track the cursor and find the image UNDER it via the element stack (works
  // through the overlay <div>s sites like Google Images stack on thumbnails).
  let lastMove = 0;
  if (isTop) document.addEventListener("mousemove", (e) => {
    const now = Date.now();
    if (now - lastMove < 60) return; // ~16fps
    lastMove = now;
    const x = e.clientX, y = e.clientY;
    if (hovImg) {
      if (ptIn(hovImg.getBoundingClientRect(), x, y)) return;
      if (imgBtn && ptIn(imgBtn.getBoundingClientRect(), x, y)) return;
      hideImgBtn();
    }
    if (!upCache.val) return;
    for (const el of document.elementsFromPoint(x, y)) {
      if (el === host) return; // over our own overlay
      if (el instanceof HTMLImageElement) {
        const r = el.getBoundingClientRect();
        if (r.width >= 120 && r.height >= 120 && el !== hovImg) showImgBtn(el);
        return;
      }
    }
  }, true);
  addEventListener("scroll", () => { if (hovImg) showImgBtn(hovImg); }, true);
})();
