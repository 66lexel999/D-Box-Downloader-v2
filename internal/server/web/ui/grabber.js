/* D BOX — Link Grabber (JDownloader2-style). Served at /ui/grabber.js; needs
   /ui/theme.css, /ui/icons.js (window.DBI) and /ui/grabber.css.

   window.DBGrab = {
     fromUrl(url, opts) → Promise   opts: {onDirect(url), onBusy(bool), ctx, referer, title}
                                    POST /api/grab, wait (≤ 8 s) for the first decisive snapshot;
                                    a direct file/stream → DELETE the job + opts.onDirect(url);
                                    a web page → the Link Grabber modal on that job.
     openText(text)                 grab every link in a block of text (text mode).
     openPaste()                    the modal in "paste" mode (address or text + Scan).
     isOpen() → bool,  close()
   }
   Self-contained: its own api()/toast helpers and DOM (built lazily on first use).
   Rows are patched in place by item id on every poll (~400 ms), so the user's
   checkboxes, renames and quality choices survive the live updates. */
(function () {
  "use strict";
  if (window.DBGrab) return;

  var BASE = window.DBGRAB_BASE || "";
  var POLL_MS = 400, FIRST_WAIT_MS = 8000;

  // ---- helpers ------------------------------------------------------------
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function I(name, size, cls) { return window.DBI ? window.DBI.icon(name, size, cls) : ""; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  async function api(method, path, body) {
    var res = await fetch(BASE + path, {
      method: method,
      headers: body ? { "Content-Type": "application/json" } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
    var data = await res.json().catch(function () { return {}; });
    if (!res.ok) {
      var err = new Error(data.error || res.statusText || ("HTTP " + res.status));
      err.status = res.status;
      throw err;
    }
    return data;
  }
  function forget(id) { if (id) api("DELETE", "/api/grab/" + encodeURIComponent(id)).catch(function () {}); }
  function toast(msg) {
    if (typeof window.toast === "function") { try { window.toast(msg); return; } catch (e) { /* fall through */ } }
    var t = document.getElementById("gbToast");
    if (!t) { t = document.createElement("div"); t.id = "gbToast"; t.className = "gb-toast"; document.body.appendChild(t); }
    t.textContent = msg;
    t.classList.add("show");
    clearTimeout(t._h);
    t._h = setTimeout(function () { t.classList.remove("show"); }, 3500);
  }
  function humanBytes(n) {
    var u = ["B", "KB", "MB", "GB", "TB"], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return n.toFixed(n >= 100 || i === 0 ? 0 : 1) + " " + u[i];
  }
  function hostOf(u) { try { return new URL(u).hostname.replace(/^www\./, ""); } catch (e) { return ""; } }
  function fmtDur(s) {
    s = Math.round(s);
    var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), x = s % 60;
    return (h ? h + ":" + String(m).padStart(2, "0") : m) + ":" + String(x).padStart(2, "0");
  }
  function hasExt(n) { return /\.[a-z0-9]{1,6}$/i.test(n || ""); }
  function stripExt(n) { return String(n || "").replace(/\.[a-z0-9]{1,6}$/i, ""); }
  function plural(n, w) { return n + " " + w + (n === 1 ? "" : "s"); }

  // ---- kinds, filters, tags -------------------------------------------------
  var GROUP_OF = { video: "video", stream: "video", audio: "audio", image: "image", archive: "archive", document: "document", program: "program" };
  function groupOf(k) { return GROUP_OF[k] || "other"; }
  var ORDER = { video: 1, stream: 2, audio: 3, archive: 4, program: 5, document: 6, torrent: 7, subtitle: 8, other: 9, image: 10, page: 11 };
  var CHIPS = [
    ["all", "All", "ui-list"], ["video", "Video", "ft-video"], ["audio", "Audio", "ft-audio"], ["image", "Images", "ft-image"],
    ["archive", "Archives", "ft-archive"], ["document", "Documents", "ft-doc"], ["program", "Programs", "ft-program"], ["other", "Other", "ft-file"],
  ];
  var KIND_ICON = { video: "ft-video", stream: "ft-stream", audio: "ft-audio", image: "ft-image", archive: "ft-archive",
    document: "ft-doc", program: "ft-program", subtitle: "ft-subtitle", torrent: "ft-torrent", page: "ft-web" };
  var SRC = { direct: "direct", ytdlp: "yt-dlp", "video-tag": "video", "audio-tag": "audio", "source-tag": "source", img: "image",
    link: "link", meta: "meta", jsonld: "JSON-LD", script: "script", iframe: "iframe", "player-config": "player", text: "text" };
  var SRC_TIP = { direct: "The address itself is the file", ytdlp: "Found by yt-dlp — pick the quality",
    "video-tag": "From the page's <video> player", "audio-tag": "From the page's <audio> player", "source-tag": "From a <source> of the player",
    img: "An image on the page", link: "A download link on the page", meta: "From the page's meta tags", jsonld: "From the page's structured data",
    script: "Found inside the page's scripts", iframe: "From an embedded frame", "player-config": "From the video player's settings", text: "From the pasted text" };

  function rowIcon(it) {
    if (it.video) return I(it.kind === "audio" ? "ft-audio" : "ft-video", 18);
    if (it.kind === "stream") return I("ft-stream", 18);
    var byName = window.DBI ? window.DBI.kindOf(it.name || it.ext || "") : "file";
    if (byName !== "file" && byName !== "web") return window.DBI.file(it.name || it.ext, 18);
    return I(KIND_ICON[it.kind] || "ft-file", 18);
  }
  function srcTag(it) {
    var label = SRC[it.source] || it.source || "", cls = "";
    if (it.kind === "stream") {
      var e = String(it.ext || "").toLowerCase();
      label = e === "mpd" ? "DASH" : "HLS";
      cls = "t-stream";
    } else if (it.source === "ytdlp") cls = "t-yt";
    else if (/^(video-tag|audio-tag|source-tag|player-config)$/.test(it.source)) cls = "t-media";
    else if (it.source === "img") cls = "t-img";
    if (!label) return "";
    return '<span class="gb-tag ' + cls + '" title="' + esc(SRC_TIP[it.source] || "") + '">' + esc(label) + "</span>";
  }
  function optKey(o) { return o ? [o.selector, o.ext, o.audio ? 1 : 0, o.label].join("|") : ""; }
  function opts(R) { return (R.it.video && R.it.video.options) || []; }
  function currentOpt(R) {
    var list = opts(R);
    if (!list.length) return null;
    for (var i = 0; i < list.length; i++) if (optKey(list[i]) === R.optKey) return list[i];
    return list[0];
  }
  function itemSize(R) {
    var it = R.it, o = currentOpt(R);
    if (o && o.size > 0) return { n: o.size, est: true };
    return { n: it.size > 0 ? it.size : 0, est: !!it.sizeEstimated || !!it.video };
  }
  function pending(it) { return it.status === "pending" || it.status === "probing"; }

  // ---- session ------------------------------------------------------------------
  var G = null;      // the open grabber
  var openSeq = 0;   // newest fromUrl/openText/openPaste call wins

  function build() {
    var root = document.createElement("div");
    root.className = "gbx";
    root.innerHTML =
      '<div class="gb" role="dialog" aria-modal="true" aria-labelledby="gbTitle" tabindex="-1">' +
        '<header class="gb-hd">' +
          '<span class="gb-badge">' + I("md-grab", 30) + "</span>" +
          '<div class="gb-ttl"><div class="gb-t1"><b id="gbTitle">Link Grabber</b><span class="gb-host"></span></div>' +
            '<div class="gb-t2"></div></div>' +
          '<div class="gb-stat" aria-live="polite"><span class="gb-si"></span><span class="gb-st"></span></div>' +
          '<button type="button" class="btn gb-stop"></button>' +
          '<button type="button" class="btn ghost gb-ib gb-new" title="Scan another page or text…">' + I("ui-paste", 16) + "</button>" +
          '<button type="button" class="btn ghost gb-ib gb-x" title="Close (Esc)">' + I("ui-close", 16) + "</button>" +
        "</header>" +
        '<div class="gb-paste">' +
          '<textarea class="field gb-ta" spellcheck="false" placeholder="Paste a page address or any text with links"></textarea>' +
          '<div class="gb-perr"></div>' +
          '<div class="gb-prow"><span class="gb-hint">' + I("ui-info", 14) +
            "<span>A web page address lists every file, video and stream on it; pasted text or HTML lists all its links.</span></span>" +
            '<button type="button" class="btn gb-pcancel">Cancel</button>' +
            '<button type="button" class="btn primary gb-scan">' + I("ui-search", 14) + "Scan</button></div>" +
        "</div>" +
        '<div class="gb-main">' +
          '<div class="gb-bar"><div class="gb-chips" role="tablist"></div><div class="gb-tools">' +
            '<button type="button" class="btn ghost gb-ib gb-sall" title="Check everything in this view">' + I("ui-select-all", 16) + "</button>" +
            '<button type="button" class="btn ghost gb-ib gb-snone" title="Uncheck everything in this view">' + I("ui-select-none", 16) + "</button>" +
            '<label class="gb-search">' + I("ui-search", 13) + '<input type="search" class="field gb-q" placeholder="Filter by name" spellcheck="false"></label>' +
            '<label class="gb-small" title="Show small images (icons, thumbnails, spacers)"><input type="checkbox" class="gb-sm"><span><span class="lg">Small images</span><span class="n"></span></span></label>' +
          "</div></div>" +
          '<div class="gb-list">' +
            '<div class="gb-head gb-grid"><span class="c-ck"><input type="checkbox" class="gb-all" title="Check / uncheck all in this view"></span><span class="c-ic"></span>' +
              '<span class="c-nm">Name</span><span class="c-q">Quality</span><span class="c-sz">Size</span><span class="c-src">Found in</span><span class="c-host">Host</span></div>' +
            '<div class="gb-rows"></div>' +
            '<div class="gb-empty" hidden></div>' +
          "</div>" +
        "</div>" +
        '<footer class="gb-ft">' +
          '<div class="gb-sum"></div>' +
          '<label class="gb-later" title="Queue them in Download Later instead of starting now"><input type="checkbox" class="gb-lat"><span><span class="lg">Add to </span>Download Later</span></label>' +
          '<button type="button" class="btn primary gb-go">' + I("ui-download", 15) + '<span class="gb-gol"><span class="lg">Download </span>selected</span></button>' +
          '<button type="button" class="btn gb-cancel">Cancel</button>' +
        "</footer>" +
      "</div>";

    function q(s) { return root.querySelector(s); }
    var g = {
      root: root, box: q(".gb"), rowsEl: q(".gb-rows"), list: q(".gb-list"), empty: q(".gb-empty"),
      host: q(".gb-host"), t2: q(".gb-t2"), stat: q(".gb-stat"), si: q(".gb-si"), st: q(".gb-st"),
      stop: q(".gb-stop"), chips: q(".gb-chips"), search: q(".gb-q"), small: q(".gb-sm"), smallN: q(".gb-small .n"),
      all: q(".gb-all"), sum: q(".gb-sum"), later: q(".gb-lat"), go: q(".gb-go"), goL: q(".gb-gol"),
      ta: q(".gb-ta"), perr: q(".gb-perr"), scan: q(".gb-scan"),
      rows: new Map(), ordered: [], orderKey: "", params: {}, id: "", snap: null, running: false, stopped: false,
      adding: false, fails: 0, jobSeq: 0, timer: 0, filter: "all", q: "", showSmall: false, mode: "list",
      prevFocus: document.activeElement, chipEls: {}, cache: {},
    };
    try { g.later.checked = localStorage.getItem("dbox.grab.later") === "1"; } catch (e) { /* storage blocked */ }

    CHIPS.forEach(function (c) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "chip" + (c[0] === "all" ? " on" : "");
      b.dataset.f = c[0];
      b.setAttribute("role", "tab");
      b.innerHTML = I(c[2], 14) + "<span>" + c[1] + '</span><span class="n"></span>';
      g.chips.appendChild(b);
      g.chipEls[c[0]] = b;
    });

    // Keep the main window's own shortcuts / paste / drop handlers out of the modal.
    ["keydown", "keyup", "keypress", "paste", "copy", "cut", "drop", "dragover", "contextmenu", "wheel"].forEach(function (ev) {
      root.addEventListener(ev, function (e) { e.stopPropagation(); });
    });
    root.addEventListener("dragover", function (e) { e.preventDefault(); });
    root.addEventListener("drop", function (e) {
      e.preventDefault();
      var dt = e.dataTransfer, txt = dt && (dt.getData("text/uri-list") || dt.getData("text/plain") || dt.getData("text"));
      if (txt && g.mode === "paste") g.ta.value = txt.trim();
    });
    g.onKey = function (e) { onKey(g, e); };
    document.addEventListener("keydown", g.onKey, true);

    q(".gb-x").addEventListener("click", close);
    q(".gb-cancel").addEventListener("click", close);
    q(".gb-pcancel").addEventListener("click", close);
    q(".gb-new").addEventListener("click", function () { newScan(g); });
    g.stop.addEventListener("click", function () { if (g.running) stop(g); else rescan(g); });
    g.scan.addEventListener("click", function () { scanPaste(g); });
    g.ta.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && (e.ctrlKey || (!e.shiftKey && g.ta.value.indexOf("\n") < 0))) { e.preventDefault(); scanPaste(g); }
    });
    g.chips.addEventListener("click", function (e) {
      var b = e.target.closest(".chip");
      if (!b) return;
      g.filter = b.dataset.f;
      g.list.scrollTop = 0;
      refresh(g);
    });
    g.search.addEventListener("input", function () { g.q = g.search.value.trim().toLowerCase(); refresh(g); });
    g.search.addEventListener("keydown", function (e) {
      if (e.key === "Enter") e.preventDefault();
      if (e.key === "Escape" && g.search.value) { e.preventDefault(); g.search.value = ""; g.q = ""; refresh(g); }
    });
    g.small.addEventListener("change", function () { g.showSmall = g.small.checked; refresh(g); });
    g.later.addEventListener("change", function () { try { localStorage.setItem("dbox.grab.later", g.later.checked ? "1" : "0"); } catch (e) { /* ignore */ } });
    g.all.addEventListener("click", function () {
      var vis = visibleRows(g), on = vis.length && vis.every(function (R) { return R.checked; });
      checkRows(g, vis, !on);
    });
    q(".gb-sall").addEventListener("click", function () { checkRows(g, visibleRows(g), true); });
    q(".gb-snone").addEventListener("click", function () { checkRows(g, visibleRows(g), false); });
    g.go.addEventListener("click", function () { addSelected(g); });

    g.rowsEl.addEventListener("click", function (e) {
      var R = rowOf(g, e.target);
      if (!R || R.editing || e.target.closest("select, .gb-ren")) return;
      var cb = e.target.closest("input[type=checkbox]");
      R.checked = cb ? cb.checked : !R.checked;
      R.touched = true;
      paintState(R);
      refresh(g);
    });
    g.rowsEl.addEventListener("dblclick", function (e) {
      var R = rowOf(g, e.target);
      if (R && e.target.closest(".c-nm") && !e.target.closest("select")) startRename(g, R);
    });
    g.rowsEl.addEventListener("change", function (e) {
      if (!e.target.matches(".gb-qs")) return;
      var R = rowOf(g, e.target);
      if (!R) return;
      R.optKey = optKey(opts(R)[+e.target.value]);
      R.checked = true;
      R.touched = true;
      paintState(R);
      paintSize(g, R);
      refresh(g);
    });
    g.rowsEl.addEventListener("mouseover", function (e) { previewOver(g, e); });
    g.rowsEl.addEventListener("mouseleave", function () { previewHide(g); });
    g.list.addEventListener("scroll", function () { previewHide(g); }, { passive: true });

    document.body.appendChild(root);
    return g;
  }

  function ensure() {
    if (G) return G;
    G = build();
    return G;
  }

  function rowOf(g, el) {
    var r = el && el.closest && el.closest(".gb-row");
    return r ? g.rows.get(r.dataset.id) : null;
  }

  function onKey(g, e) {
    if (G !== g) return;
    var inside = g.root.contains(e.target);
    if (e.key === "Escape") {
      // rename cancels itself; a non-empty search box clears first
      if (inside && (e.target.closest(".gb-ren") || (e.target.closest(".gb-q") && g.search.value))) return;
      e.preventDefault();
      e.stopPropagation();
      close();
      return;
    }
    if (!inside) {
      // Focus drifted to the page behind the modal: don't let its shortcuts (Delete, Ctrl+A …) act on the list.
      e.stopPropagation();
      if (e.key === "Tab") { e.preventDefault(); g.box.focus(); }
      return;
    }
    if ((e.ctrlKey || e.metaKey) && (e.key === "a" || e.key === "A") && g.mode === "list" &&
        !/^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) {
      e.preventDefault();
      checkRows(g, visibleRows(g), true);
    }
  }

  // Reset the modal for a new job (keeps the DOM, forgets rows and the old job).
  function reset(g, params) {
    clearTimeout(g.timer);
    if (g.id) forget(g.id);
    g.jobSeq++;
    g.id = "";
    g.snap = null;
    g.running = false;
    g.stopped = false;
    g.adding = false;
    g.fails = 0;
    g.rows.clear();
    g.rowsEl.textContent = "";
    g.ordered = [];
    g.orderKey = "";
    g.filter = "all";
    g.q = "";
    g.search.value = "";
    g.params = params || {};
    g.cache = {};
    previewHide(g);
  }

  function showList(g) {
    g.mode = "list";
    g.box.classList.remove("paste");
    if (!g.root.contains(document.activeElement) || document.activeElement === g.ta) g.box.focus();
  }

  function showPaste(g, value, err) {
    g.mode = "paste";
    g.box.classList.add("paste");
    g.host.textContent = "";
    g.t2.textContent = "Paste a page address, or text / HTML that contains links";
    g.ta.value = value || "";
    g.perr.innerHTML = err ? I("ui-warning", 14) + "<span>" + esc(err) + "</span>" : "";
    g.scan.disabled = false;
    setTimeout(function () { g.ta.focus(); g.ta.select(); }, 0);
  }

  function reqBody(p) {
    var b = {};
    ["url", "text", "title", "referer", "ctx"].forEach(function (k) { if (p[k]) b[k] = p[k]; });
    return b;
  }

  async function startJob(g) {
    var p = g.params, seq = ++g.jobSeq;
    g.running = true;
    g.stopped = false;
    g.snap = { status: "probing", mode: p.url ? "page" : "text", url: p.url || "", title: p.title || "", items: [], counts: {} };
    showList(g);
    refresh(g);
    var s;
    try {
      s = await api("POST", "/api/grab", reqBody(p));
    } catch (e) {
      if (G !== g || g.jobSeq !== seq) return;
      g.running = false;
      if (g.fromPaste || p.text) { showPaste(g, p.url || p.text, e.message); return; }
      g.snap = Object.assign({}, g.snap, { status: "error", error: e.message });
      refresh(g);
      return;
    }
    if (G !== g || g.jobSeq !== seq) { forget(s.id); return; }
    g.id = s.id;
    apply(g, s);
    if (g.running) schedule(g);
  }

  function schedule(g) {
    clearTimeout(g.timer);
    g.timer = setTimeout(function () { poll(g); }, POLL_MS);
  }

  async function poll(g) {
    if (G !== g || !g.running || !g.id) return;
    var id = g.id, seq = g.jobSeq, s;
    try {
      s = await api("GET", "/api/grab/" + encodeURIComponent(id));
    } catch (e) {
      if (G !== g || g.id !== id || g.jobSeq !== seq) return;
      if (e.status === 404 || ++g.fails > 10) {
        g.running = false;
        g.stopped = true;
        g.id = "";
        repaintPending(g);
        refresh(g);
        return;
      }
      schedule(g);
      return;
    }
    if (G !== g || g.id !== id || g.jobSeq !== seq || !g.running) return;
    g.fails = 0;
    apply(g, s);
    if (g.running) schedule(g);
  }

  function stop(g) {
    clearTimeout(g.timer);
    forget(g.id);
    g.id = "";
    g.jobSeq++;
    g.running = false;
    g.stopped = true;
    repaintPending(g);
    refresh(g);
  }

  function rescan(g) {
    var p = g.params, fromPaste = g.fromPaste;
    reset(g, p);
    g.fromPaste = fromPaste;
    startJob(g);
  }

  function newScan(g) {
    var p = g.params;
    reset(g, {});
    g.fromPaste = true;
    showPaste(g, p.url || p.text || "", "");
    refresh(g);
  }

  function scanPaste(g) {
    var v = g.ta.value.trim();
    if (!v) { g.perr.innerHTML = I("ui-warning", 14) + "<span>Paste a page address or some text first.</span>"; g.ta.focus(); return; }
    var params;
    if (/^https?:\/\/\S+$/i.test(v)) params = { url: v };
    else if (/^(www\.)?[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,}(\/\S*)?$/i.test(v)) params = { url: "https://" + v };
    else params = { text: v };
    reset(g, params);
    g.fromPaste = true;
    startJob(g);
  }

  // ---- applying a snapshot --------------------------------------------------------
  function apply(g, s) {
    g.snap = s;
    g.running = s.status === "probing" || s.status === "scanning";
    var seen = {}, first = !g.rows.size, items = s.items || [];
    for (var i = 0; i < items.length; i++) {
      var it = items[i], R = g.rows.get(it.id);
      seen[it.id] = true;
      if (!R) {
        R = { id: it.id, it: it, idx: i, checked: false, touched: false, rename: "", optKey: "", sig: "", editing: false };
        R.el = document.createElement("div");
        R.el.className = "gb-row gb-grid" + (first ? "" : " new");
        R.el.dataset.id = it.id;
        if (!first) setTimeout(function (el) { el.classList.remove("new"); }.bind(null, R.el), 1200);
        g.rows.set(it.id, R);
      }
      R.it = it;
      R.idx = i;
    }
    g.rows.forEach(function (R, id) { if (!seen[id]) { R.el.remove(); g.rows.delete(id); } });
    defaults(g);
    g.rows.forEach(function (R) { paintRow(g, R); });
    reorder(g);
    refresh(g);
  }

  // Default selection (only for rows the user hasn't touched): the main video,
  // media, archives, documents and programs; images only when the page has no
  // media; never the error rows, and not the obvious duplicates of the main video.
  function defaults(g) {
    var rows = Array.from(g.rows.values()), rec = null;
    g.hasMedia = false;
    g.hasYtVideo = false;
    g.hasQ = false;
    rows.forEach(function (R) {
      var it = R.it;
      if (it.recommended && !rec) rec = R;
      if (it.status !== "error" && !it.small && /^(video|stream|audio)$/.test(it.kind)) g.hasMedia = true;
      if (it.video && it.kind === "video") g.hasYtVideo = true;
      if (it.video && it.video.options && it.video.options.length) g.hasQ = true;
    });
    g.recStem = rec ? stripExt(rec.it.name).toLowerCase() : "";
    g.recSingleYt = !!(rec && rec.it.video && !rec.it.playlistItem);
    var text = g.snap && g.snap.mode === "text";
    rows.forEach(function (R) { if (!R.touched) R.checked = defaultChecked(g, R.it, text); });
  }
  function defaultChecked(g, it, text) {
    if (it.status === "error") return false;
    if (it.recommended) return true;
    var k = it.kind, media = k === "video" || k === "stream" || k === "audio";
    if (text) return k !== "page";
    if (it.video && k === "audio" && !it.playlistItem && g.hasYtVideo) return false;           // yt-dlp "audio only" twin
    if (media && g.recStem && stripExt(it.name).toLowerCase() === g.recStem) return false;    // same file, other format
    if ((k === "video" || k === "stream") && g.recSingleYt && !it.playlistItem) return false; // the player's own files
    // A page with a known main video preselects just that one: the other media are
    // usually alternate formats/masters (NASA lists 4 GB ProRes copies next to the
    // MP4), a gallery's extras are one "Select all" away, and nobody should start
    // 20 GB by pressing Download without looking.
    if (media && g.recStem) return false;
    if (media || k === "archive" || k === "document" || k === "program" || k === "torrent") return true;
    if (k === "image") return !g.hasMedia;
    return false;
  }

  function rowSig(g, R) {
    var it = R.it, o = opts(R);
    return [it.name, it.url, it.kind, it.ext, it.status, it.error, it.size, it.sizeEstimated ? 1 : 0, it.live ? 1 : 0,
      it.recommended ? 1 : 0, it.duration, it.width, it.height, it.source, it.playlistItem,
      o.map(function (x) { return optKey(x) + x.size; }).join(","), R.rename, pending(it) && g.stopped ? 1 : 0].join("\u0001");
  }

  function paintRow(g, R) {
    if (R.editing) return;
    var sig = rowSig(g, R);
    if (sig !== R.sig) {
      R.sig = sig;
      R.el.innerHTML = rowHTML(g, R);
      var sel = R.el.querySelector(".gb-qs");
      if (sel) {
        var list = opts(R), idx = 0;
        for (var i = 0; i < list.length; i++) if (optKey(list[i]) === R.optKey) { idx = i; break; }
        sel.value = String(idx);
        R.optKey = optKey(list[idx]);
      }
    }
    var it = R.it;
    R.el.classList.toggle("rec", !!it.recommended);
    R.el.classList.toggle("err", it.status === "error");
    R.el.classList.toggle("skip", it.status === "skipped");
    paintState(R);
  }
  function paintState(R) {
    var cb = R.el.querySelector(".c-ck input");
    if (cb && cb.checked !== R.checked) cb.checked = R.checked;
    R.el.classList.toggle("on", R.checked);
  }
  function repaintPending(g) { g.rows.forEach(function (R) { if (pending(R.it)) paintRow(g, R); }); }

  function tipOf(R) {
    var it = R.it, lines = [];
    if (it.status === "error") lines.push("Error: " + (it.error || "could not be checked"));
    else if (it.status === "skipped") lines.push("Not checked (time limit)");
    lines.push(R.rename || it.name || "");
    if (R.rename && it.name) lines.push("Original name: " + it.name);
    lines.push(it.url);
    if (it.foundOn && it.foundOn !== it.url) lines.push("Found on: " + it.foundOn);
    lines.push("Double-click the name to rename");
    return lines.filter(Boolean).join("\n");
  }

  function sizeHTML(g, R) {
    var it = R.it;
    if (pending(it)) return g.stopped ? '<span class="gb-nc">not checked</span>' : '<span class="spinner gb-spin" title="Checking…"></span>';
    if (it.status === "skipped") return '<span class="gb-nc">not checked</span>';
    if (it.status === "error") return '<span class="gb-bad">' + I("ui-warning", 12) + "error</span>";
    if (it.live) return '<span class="gb-live">' + I("st-live", 12) + "live</span>";
    var sz = itemSize(R);
    if (sz.n > 0) return '<span class="' + (sz.est ? "est" : "") + '"' + (sz.est ? ' title="Estimated size"' : "") + ">" +
      (sz.est ? "~" : "") + humanBytes(sz.n) + "</span>";
    return '<span class="gb-unk" title="Size unknown">—</span>';
  }
  function paintSize(g, R) {
    var c = R.el.querySelector(".c-sz");
    if (c) c.innerHTML = sizeHTML(g, R);
  }

  function rowHTML(g, R) {
    var it = R.it, name = R.rename || it.name || it.url, meta = [], tip = esc(tipOf(R));
    if (it.playlistItem) meta.push("#" + it.playlistItem);
    if (it.duration > 0) meta.push(fmtDur(it.duration));
    if (it.width > 0 && it.height > 0) meta.push(it.width + "×" + it.height);
    var list = opts(R), q = "";
    if (list.length) {
      q = '<select class="field gb-qs" title="Quality">' + list.map(function (o, i) {
        return '<option value="' + i + '">' + esc(o.label || String(o.ext || "").toUpperCase() || "Best") + "</option>";
      }).join("") + "</select>";
    } else if (it.video) q = '<span class="gb-qnone">best available</span>';
    return '<span class="c-ck"><input type="checkbox" aria-label="Download this item"></span>' +
      '<span class="c-ic" title="' + tip + '">' + rowIcon(it) + "</span>" +
      '<span class="c-nm" title="' + tip + '"><span class="gb-nm">' + esc(name) + "</span>" +
        (R.rename ? '<span class="gb-ren-mark" title="Renamed">' + I("ui-rename", 11) + "</span>" : "") +
        (meta.length ? '<span class="gb-meta">' + esc(meta.join(" · ")) + "</span>" : "") +
        (it.recommended ? '<span class="gb-main-pill">' + (it.kind === "audio" ? "Main" : "Main<span class=\"lg\"> video</span>") + "</span>" : "") +
      "</span>" +
      '<span class="c-q">' + q + "</span>" +
      '<span class="c-sz">' + sizeHTML(g, R) + "</span>" +
      '<span class="c-src">' + srcTag(it) + "</span>" +
      '<span class="c-host">' + esc(hostOf(it.url)) + "</span>";
  }

  function reorder(g) {
    var list = Array.from(g.rows.values()).sort(function (a, b) {
      return ((b.it.recommended ? 1 : 0) - (a.it.recommended ? 1 : 0)) ||
        ((ORDER[a.it.kind] || 9) - (ORDER[b.it.kind] || 9)) || (a.idx - b.idx);
    });
    var key = list.map(function (R) { return R.id; }).join(",");
    g.ordered = list;
    if (key === g.orderKey) return;
    g.orderKey = key;
    var prev = null;
    list.forEach(function (R) {
      var want = prev ? prev.nextSibling : g.rowsEl.firstChild;
      if (want !== R.el) g.rowsEl.insertBefore(R.el, want);
      prev = R.el;
    });
  }

  // ---- filters / header / footer ---------------------------------------------
  function hiddenSmall(g, R) { return R.it.small && !g.showSmall; }
  function isVisible(g, R) {
    if (hiddenSmall(g, R)) return false;
    if (g.filter !== "all" && groupOf(R.it.kind) !== g.filter) return false;
    if (g.q) {
      var hay = ((R.rename || "") + " " + (R.it.name || "") + " " + R.it.url).toLowerCase();
      if (hay.indexOf(g.q) < 0) return false;
    }
    return true;
  }
  function visibleRows(g) { return g.ordered.filter(function (R) { return isVisible(g, R); }); }
  function checkRows(g, rows, on) {
    rows.forEach(function (R) { R.checked = on; R.touched = true; paintState(R); });
    refresh(g);
  }
  function setHTML(g, key, el, html) {
    if (g.cache[key] === html) return;
    g.cache[key] = html;
    el.innerHTML = html;
  }

  function chipCount(g, key) {
    var c = (g.snap && g.snap.counts) || null, small = g.showSmall ? ((c && c.small) || 0) : 0;
    if (!c) { // older server: count ourselves
      var n = 0;
      g.rows.forEach(function (R) { if (!hiddenSmall(g, R) && (key === "all" || groupOf(R.it.kind) === key)) n++; });
      return n;
    }
    if (key === "all") return ["video", "audio", "image", "archive", "document", "program", "stream", "other"]
      .reduce(function (a, k) { return a + (c[k] || 0); }, 0) + small;
    if (key === "video") return (c.video || 0) + (c.stream || 0);
    if (key === "image") return (c.image || 0) + small;
    return c[key] || 0;
  }

  function refresh(g) {
    if (G !== g) return;
    var s = g.snap || { status: "", items: [], counts: {} };
    g.box.classList.toggle("has-q", !!g.hasQ);

    // rows: visibility
    var vis = 0, shown = 0, visOn = 0, selN = 0, tot = 0, est = false, unk = 0;
    g.ordered.forEach(function (R) {
      var v = isVisible(g, R);
      if (R.el.hidden === v) R.el.hidden = !v;
      if (v) { vis++; if (R.checked) visOn++; }
      if (!hiddenSmall(g, R)) {
        shown++;
        if (R.checked) {
          selN++;
          var sz = itemSize(R);
          if (sz.n > 0) { tot += sz.n; est = est || sz.est; } else if (!R.it.live) unk++;
        }
      }
    });

    // header
    if (g.mode === "list") {
      var host = s.mode === "text" ? "" : hostOf(s.url || g.params.url || "");
      setHTML(g, "host", g.host, host ? I("ui-globe", 12) + "<span>" + esc(host) + "</span>" : "");
      var t2 = s.mode === "text" ? "Links from pasted text" : (s.pageTitle || s.title || g.params.title || s.url || g.params.url || "");
      if (g.t2.textContent !== t2) { g.t2.textContent = t2; g.t2.title = s.url || ""; }
    }
    var icon, text, cls = "";
    if (g.stopped) { icon = I("st-paused", 14); text = "Stopped — <b>" + shown + "</b> item" + (shown === 1 ? "" : "s"); }
    else if (s.status === "probing") { icon = '<span class="spinner"></span>'; text = s.mode === "text" ? "Checking links…" : "Opening page…"; }
    else if (s.status === "scanning") {
      icon = '<span class="spinner"></span>';
      text = "Scanning… <b>" + shown + "</b> found" + (s.ytdlp === "running" ? " · reading video info" : "");
    } else if (s.status === "done") { icon = I("st-completed", 14); text = "Done — <b>" + shown + "</b> item" + (shown === 1 ? "" : "s"); cls = "ok"; }
    else if (s.status === "error") { icon = I("st-failed", 14); text = esc(s.error || "Could not read the page"); cls = "err"; }
    else { icon = ""; text = ""; }
    setHTML(g, "si", g.si, icon);
    setHTML(g, "st", g.st, text);
    g.stat.className = "gb-stat" + (cls ? " " + cls : "");
    g.st.title = s.status === "error" ? (s.error || "") : (s.ytdlpError ? "yt-dlp: " + s.ytdlpError : "");
    setHTML(g, "stop", g.stop, g.running ? I("ui-stop", 14) + '<span class="lg">Stop</span>' : I("ui-refresh", 14) + '<span class="lg">Rescan</span>');
    g.stop.title = g.running ? "Stop scanning (keep what was found)" : "Scan again";
    g.stop.hidden = !g.running && !s.status;

    // chips + small toggle
    CHIPS.forEach(function (c) {
      var el = g.chipEls[c[0]], n = chipCount(g, c[0]);
      var nEl = el.querySelector(".n");
      if (nEl.textContent !== String(n)) nEl.textContent = String(n);
      el.classList.toggle("on", g.filter === c[0]);
      el.classList.toggle("zero", !n && c[0] !== "all");
      el.setAttribute("aria-selected", g.filter === c[0] ? "true" : "false");
    });
    var smallN = (s.counts && s.counts.small) || 0;
    var sn = smallN ? " (" + smallN + ")" : "";
    if (g.smallN.textContent !== sn) g.smallN.textContent = sn;

    // header checkbox
    g.all.checked = vis > 0 && visOn === vis;
    g.all.indeterminate = visOn > 0 && visOn < vis;

    // empty state
    var html = "", mode = "";
    if (!vis) {
      if (!shown && g.running) {
        mode = "scan";
        html = I("ill-grab", 120, "gb-ill") + '<div class="gb-e1">' + (s.mode === "text" ? "Checking the links…" : "Looking for files on this page…") + "</div>" +
          '<div class="gb-e2">Videos, streams, music, archives, documents and programs show up here as they are found.</div>';
      } else if (!shown) {
        mode = "none";
        var extra = "";
        if (smallN) extra = " Only small images were found — tick “Small images” to see them.";
        else if (s.ytdlp === "none") extra = " yt-dlp isn't installed, so videos on streaming sites can't be detected.";
        html = I("ill-nothing", 120, "gb-ill") +
          '<div class="gb-e1">' + (s.status === "error" ? esc(s.error || "Could not read the page") : g.stopped ? "Stopped before anything was found" : "Nothing downloadable found") + "</div>" +
          '<div class="gb-e2">If the video only appears after pressing play, use the browser extension\'s button on that page.' + esc(extra) + "</div>";
      } else {
        mode = "slim";
        html = I("ui-filter", 22) + '<div class="gb-e1">Nothing matches this filter.</div>';
      }
    }
    g.empty.hidden = !html;
    g.empty.className = "gb-empty" + (mode ? " " + mode : "");
    setHTML(g, "empty", g.empty, html);

    // footer
    var sum;
    if (!selN) sum = "Nothing selected";
    else if (tot > 0) sum = "<b>" + selN + "</b> selected · total <b>" + (est ? "~" : "") + humanBytes(tot) + "</b>" +
      (unk ? ' <span class="gb-dim">+ ' + unk + " unknown</span>" : "");
    else sum = "<b>" + selN + "</b> selected · size unknown";
    sum += '<span class="gb-note"> · saved to category folders by type</span>';
    setHTML(g, "sum", g.sum, sum);
    g.go.disabled = !selN || g.adding;
  }

  // ---- rename ----------------------------------------------------------------------
  function startRename(g, R) {
    if (R.editing || g.adding) return;
    var cell = R.el.querySelector(".c-nm");
    if (!cell) return;
    R.editing = true;
    previewHide(g);
    var cur = R.rename || R.it.name || "";
    var inp = document.createElement("input");
    inp.className = "field gb-ren";
    inp.value = cur;
    inp.spellcheck = false;
    cell.textContent = "";
    cell.appendChild(inp);
    inp.focus();
    var dot = cur.lastIndexOf(".");
    inp.setSelectionRange(0, dot > 0 ? dot : cur.length);
    var done = false;
    function finish(save) {
      if (done) return;
      done = true;
      R.editing = false;
      if (save) {
        var v = inp.value.trim().replace(/[\\/:*?"<>|\u0000-\u001f]+/g, "_");
        R.rename = v && v !== R.it.name ? v : "";
        if (R.rename) { R.checked = true; R.touched = true; }
      }
      R.sig = "";
      paintRow(g, R);
      refresh(g);
      if (G === g && !g.root.contains(document.activeElement)) g.box.focus();
    }
    inp.addEventListener("keydown", function (e) {
      if (e.key === "Enter") { e.preventDefault(); finish(true); }
      else if (e.key === "Escape") { e.preventDefault(); finish(false); }
    });
    inp.addEventListener("blur", function () { finish(true); });
    inp.addEventListener("click", function (e) { e.stopPropagation(); });
    inp.addEventListener("dblclick", function (e) { e.stopPropagation(); });
  }

  // ---- hover preview (images, video thumbnails) ------------------------------------
  function previewOver(g, e) {
    var R = rowOf(g, e.target);
    var src = "";
    if (R && !R.editing && e.target.closest(".c-ic, .c-nm")) {
      if (R.it.kind === "image" && R.it.status !== "error") src = R.it.url;
      else if (R.it.thumb) src = R.it.thumb;
    }
    if (!src) { previewHide(g); return; }
    if (g.prevId === R.id) return;
    previewHide(g);
    g.prevId = R.id;
    var rowEl = R.el;
    g.prevT = setTimeout(function () {
      if (G !== g || g.prevId !== R.id) return;
      var box = document.createElement("div");
      box.className = "gb-prev";
      box.style.visibility = "hidden";
      var img = new Image();
      img.referrerPolicy = "no-referrer";
      img.onload = function () {
        if (g.prev !== box) return;
        var r = rowEl.getBoundingClientRect(), bw = box.offsetWidth, bh = box.offsetHeight;
        var left = Math.min(r.left + 44, window.innerWidth - bw - 8);
        var top = r.bottom + 4;
        if (top + bh > window.innerHeight - 8) top = Math.max(8, r.top - bh - 4);
        box.style.left = Math.max(8, left) + "px";
        box.style.top = top + "px";
        box.style.visibility = "visible";
      };
      img.onerror = function () { if (g.prev === box) previewHide(g); };
      img.src = src;
      box.appendChild(img);
      document.body.appendChild(box);
      g.prev = box;
    }, 350);
  }
  function previewHide(g) {
    clearTimeout(g.prevT);
    g.prevId = "";
    if (g.prev) { g.prev.remove(); g.prev = null; }
  }

  // ---- queueing the checked rows ----------------------------------------------------
  function requestFor(g, R, later) {
    var it = R.it, p = g.params, s = g.snap || {};
    var referer = it.foundOn || p.referer || (s.mode === "page" ? s.url : "") || undefined;
    if (it.video) {
      var o = currentOpt(R) || {};
      var size = o.size > 0 ? o.size : (it.size > 0 ? it.size : 0);
      return ["/api/video", {
        url: it.url,
        title: R.rename ? stripExt(R.rename) : (it.video.title || stripExt(it.name) || it.name),
        selector: o.selector || "", ext: o.ext || it.ext || "", audio: !!(o.audio || (!o.selector && it.kind === "audio")),
        size: size || undefined, playlistItem: it.video.playlistItem || it.playlistItem || undefined,
        referer: referer, ctx: p.ctx || undefined, category: it.category || undefined, later: later || undefined,
      }];
    }
    var fileName;
    if (R.rename) fileName = R.rename;
    else if (it.kind !== "stream" && it.kind !== "page" && hasExt(it.name)) fileName = it.name;
    return ["/api/tasks", {
      url: it.url, fileName: fileName, title: s.title || p.title || undefined,
      referer: referer, ctx: p.ctx || undefined, category: it.category || undefined, later: later || undefined,
    }];
  }

  async function addSelected(g) {
    if (g.adding) return;
    var sel = g.ordered.filter(function (R) { return R.checked && !hiddenSmall(g, R); });
    if (!sel.length) return;
    var later = g.later.checked, ok = 0, fail = 0, lastErr = "";
    g.adding = true;
    g.go.disabled = true;
    g.goL.textContent = "Adding… 0/" + sel.length;
    for (var i = 0; i < sel.length; i++) {
      var req = requestFor(g, sel[i], later);
      try { await api("POST", req[0], req[1]); ok++; } catch (e) { fail++; lastErr = e.message; }
      if (G !== g) break;
      g.goL.textContent = "Adding… " + (i + 1) + "/" + sel.length;
    }
    if (ok) {
      toast("Added " + plural(ok, "item") + (later ? " to Download Later" : "") + (fail ? " — " + fail + " failed: " + lastErr : ""));
      if (G === g) close();
      return;
    }
    toast("Could not add: " + (lastErr || "unknown error"));
    if (G !== g) return;
    g.adding = false;
    g.goL.innerHTML = '<span class="lg">Download </span>selected';
    refresh(g);
  }

  // ---- open / close --------------------------------------------------------------
  function close() {
    var g = G;
    if (!g) return;
    G = null;
    clearTimeout(g.timer);
    previewHide(g);
    forget(g.id);
    document.removeEventListener("keydown", g.onKey, true);
    g.root.remove();
    var f = g.prevFocus;
    if (f && f.focus && document.contains(f)) { try { f.focus(); } catch (e) { /* ignore */ } }
  }

  async function fromUrl(url, o) {
    o = o || {};
    url = String(url || "").trim();
    if (!url) { openPaste(); return; }
    var seq = ++openSeq;
    var params = { url: url, ctx: o.ctx || "", referer: o.referer || "", title: o.title || "" };
    function busy(b) { try { if (o.onBusy) o.onBusy(b); } catch (e) { /* caller's problem */ } }
    busy(true);
    var s;
    try {
      s = await api("POST", "/api/grab", reqBody(params));
      var t0 = Date.now();
      while (s.status === "probing" && !s.direct && Date.now() - t0 < FIRST_WAIT_MS && seq === openSeq) {
        await sleep(300);
        s = await api("GET", "/api/grab/" + encodeURIComponent(s.id));
      }
    } catch (e) {
      busy(false);
      if (s && s.id) forget(s.id);
      if (seq !== openSeq) return;
      if (o.onDirect) { o.onDirect(url); return; } // the classic dialog reports the problem
      toast(e.message || "Could not open that address");
      return;
    }
    busy(false);
    if (seq !== openSeq) { forget(s.id); return; }
    if (s.direct && o.onDirect) { forget(s.id); o.onDirect(url); return; }
    var g = ensure();
    reset(g, params);
    g.fromPaste = false;
    g.id = s.id;
    showList(g);
    apply(g, s);
    if (g.running) schedule(g);
  }

  function openText(text) {
    text = String(text || "").trim();
    if (!text) { openPaste(); return; }
    openSeq++;
    var g = ensure();
    reset(g, { text: text });
    g.fromPaste = false;
    startJob(g);
  }

  function openPaste() {
    openSeq++;
    var g = ensure();
    reset(g, {});
    g.fromPaste = true;
    showPaste(g, "", "");
    refresh(g);
  }

  window.DBGrab = {
    fromUrl: fromUrl,
    openText: openText,
    openPaste: openPaste,
    isOpen: function () { return !!G; },
    close: close,
  };
})();
