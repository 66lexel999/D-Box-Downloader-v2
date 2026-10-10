/* D BOX icon library — every icon in the app (toolbar, sidebar, file types,
   status, menus, dialogs), drawn as one SVG sprite so all windows share it.

     <script src="/ui/icons.js"></script>          (in <head>; injects the sprite)
     DBI.icon("tb-add", 24)                         → '<svg class="dbi dbi-tb-add" …><use href="#dbi-tb-add"/></svg>'
     DBI.cat("Video")  DBI.file("clip.mp4")  DBI.kindOf("x.m3u8") → "stream"
     DBI.badge("setup.exe", 48)                     → big file icon with an "EXE" pill

   Families: "tb-" toolbar (colour glyphs, 24px), "cat-", "view-" and "ft-"
   (gradient tiles, 16-48px), "st-" status discs, "ui-" line icons in
   currentColor, "md-" dialog badges, "ill-" illustrations. */
(function () {
  "use strict";
  if (window.DBI) return;

  // ---- palette: top → bottom gradient stops per hue ------------------------
  var HUES = {
    video: ["#b69cff", "#7443e6"], audio: ["#ff8cc6", "#e0357f"], image: ["#4fe0c6", "#0f9b8e"],
    archive: ["#ffb566", "#e5680f"], pdf: ["#ff8a80", "#dc3a33"], doc: ["#79b4ff", "#2d6fe0"],
    sheet: ["#5fe08f", "#179a4c"], slides: ["#ffd25c", "#e0930a"], text: ["#aeb8c6", "#6b7688"],
    program: ["#9aa4ff", "#5146e0"], disk: ["#6fd3ff", "#1686c9"], subtitle: ["#f5d94a", "#c29a06"],
    torrent: ["#b5ec5a", "#5c9a12"], stream: ["#ff8a9c", "#e42c55"], web: ["#4fdcf2", "#0a93b3"],
    code: ["#8b9bb3", "#3d4b61"], font: ["#f08cff", "#b92fd1"], file: ["#c4bdb6", "#857c74"],
    folder: ["#ffd76a", "#f0a818"], folderBack: ["#e9b43e", "#c98506"],
    brand: ["#62e38e", "#18a650"], amber: ["#ffcb5c", "#e8900c"], sky: ["#72ccff", "#1c8fd6"],
    green: ["#62e38e", "#18a650"], red: ["#ff8a80", "#d9342c"], slate: ["#a7b2c2", "#5f6b7d"],
    blue: ["#7db8ff", "#2a6fe0"], violet: ["#b69cff", "#7443e6"], rose: ["#ff8fb0", "#e03565"],
    teal: ["#4fe0c6", "#0f9b8e"], orange: ["#ffb566", "#e5680f"], indigo: ["#9aa4ff", "#5146e0"],
    steel: ["#c3ccd8", "#7d8a9c"], box: ["#f4f6f9", "#c9d1dc"],
  };
  var TINT = { green: "#2fcc66", blue: "#4b9bff", amber: "#f0a31c", red: "#f0544c", violet: "#9670ff",
    slate: "#8a97aa", pink: "#f0509a", brand: "#2fcc66", sky: "#35a9f0" };

  var defs = "";
  Object.keys(HUES).forEach(function (k) {
    defs += '<linearGradient id="dbi-g-' + k + '" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="' +
      HUES[k][0] + '"/><stop offset="1" stop-color="' + HUES[k][1] + '"/></linearGradient>';
  });
  defs += '<linearGradient id="dbi-hl" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#fff" stop-opacity=".42"/>' +
    '<stop offset="1" stop-color="#fff" stop-opacity="0"/></linearGradient>';

  var S = {}; // name → inner SVG markup (viewBox 0 0 24 24 unless VB says otherwise)
  var VB = {};

  // ---- building blocks ------------------------------------------------------
  var W = 'fill="#fff"';
  function st(w, color) { // stroke attrs
    return 'fill="none" stroke="' + (color || "#fff") + '" stroke-width="' + w + '" stroke-linecap="round" stroke-linejoin="round"';
  }
  // A solid gradient app tile with the glyph on top. (No sheen overlay or edge
  // line: at 32-48px their seams read as a see-through frame around the tile.)
  function tile(h, glyph) {
    return '<rect x="2" y="2" width="20" height="20" rx="5.5" fill="url(#dbi-g-' + h + ')"/>' + glyph;
  }
  // A small solid status disc.
  function disc(h, glyph) {
    return '<circle cx="12" cy="12" r="10" fill="url(#dbi-g-' + h + ')"/>' + glyph;
  }
  // A dialog header badge: tinted circle + glyph stroked in the hue.
  function badge(tint, inner) {
    return '<circle cx="12" cy="12" r="11.5" fill="' + TINT[tint] + '" fill-opacity=".17"/>' +
      '<circle cx="12" cy="12" r="11" fill="none" stroke="' + TINT[tint] + '" stroke-opacity=".35" stroke-width="1"/>' +
      '<g transform="translate(5.4 5.4) scale(.55)" ' + st(2.4, TINT[tint]) + '>' + inner + '</g>';
  }
  function gearPath(cx, cy, ro, ri, n, hole) {
    var d = "", step = Math.PI * 2 / n;
    for (var i = 0; i < n; i++) {
      var a = i * step - Math.PI / 2;
      var pts = [[a - step * .23, ri], [a - step * .13, ro], [a + step * .13, ro], [a + step * .23, ri], [a + step * .5, ri]];
      for (var j = 0; j < pts.length; j++) {
        var x = (cx + Math.cos(pts[j][0]) * pts[j][1]).toFixed(2), y = (cy + Math.sin(pts[j][0]) * pts[j][1]).toFixed(2);
        d += (i === 0 && j === 0 ? "M" : "L") + x + " " + y;
      }
    }
    d += "Z";
    if (hole) d += "M" + (cx + hole) + " " + cy + "a" + hole + " " + hole + " 0 1 0-" + hole * 2 + " 0a" + hole + " " + hole + " 0 1 0 " + hole * 2 + " 0z";
    return d;
  }

  // ---- white glyphs for tiles (drawn inside ~6..18) --------------------------
  var GL = {
    video: '<path d="M9.6 7.6v8.8c0 .7.8 1.1 1.4.7l6.6-4.4c.5-.4.5-1.1 0-1.4L11 6.9c-.6-.4-1.4 0-1.4.7z" ' + W + '/>',
    audio: '<path d="M10.3 15.9V8.7l6.4-1.6v7.3" ' + st(1.9) + '/><circle cx="8.6" cy="16" r="2.1" ' + W + '/><circle cx="15" cy="14.5" r="2.1" ' + W + '/>',
    image: '<circle cx="15.2" cy="8.9" r="1.8" ' + W + '/><path d="M5.8 17.2l3.9-5.1c.3-.4.9-.4 1.2 0l2.4 3 1.4-1.6c.3-.4.9-.4 1.2 0l2.4 3.7z" ' + W + '/>',
    archive: '<rect x="6.3" y="5.8" width="11.4" height="12.4" rx="2.2" fill="#fff" fill-opacity=".22" stroke="#fff" stroke-width="1.5"/>' +
      '<path d="M11.2 6.2h1.6v1.4h-1.6zM12.2 7.6h1.6V9h-1.6zM11.2 9h1.6v1.4h-1.6zM12.2 10.4h1.6v1.4h-1.6z" ' + W + '/>' +
      '<rect x="10.3" y="12" width="3.4" height="4.6" rx="1" ' + W + '/><rect x="11.3" y="13.1" width="1.4" height="1.7" rx=".5" fill="#e5680f"/>',
    page: '<path d="M8 5.8h5.3l3.3 3.3v8.3c0 .5-.4.8-.8.8H8c-.5 0-.8-.3-.8-.8V6.6c0-.5.3-.8.8-.8z" ' + W + '/>',
    pdf: '<path d="M8 5.8h5.3l3.3 3.3v8.3c0 .5-.4.8-.8.8H8c-.5 0-.8-.3-.8-.8V6.6c0-.5.3-.8.8-.8z" ' + W + '/>' +
      '<path d="M9.4 15.4c1.6-.9 3.1-3 3.5-5.2.2-1-1-1.1-1-.1 0 1.8 1.7 4.2 3.6 4.9" fill="none" stroke="#dc3a33" stroke-width="1.1" stroke-linecap="round"/>',
    doc: '<path d="M8 5.8h5.3l3.3 3.3v8.3c0 .5-.4.8-.8.8H8c-.5 0-.8-.3-.8-.8V6.6c0-.5.3-.8.8-.8z" ' + W + '/>' +
      '<path d="M9.3 11.4h5.2M9.3 13.6h5.2M9.3 15.8h3.4" fill="none" stroke="#2d6fe0" stroke-width="1.1" stroke-linecap="round"/>',
    sheet: '<rect x="6.3" y="6.8" width="11.4" height="10.4" rx="1.6" ' + st(1.6) + '/><path d="M6.6 10.5h10.8M6.6 13.9h10.8M10.4 7v10" ' + st(1.4) + '/>',
    slides: '<rect x="5.8" y="6.4" width="12.4" height="8.6" rx="1.4" ' + W + '/><path d="M12 15v2.6M9.4 18h5.2" ' + st(1.6) + '/>' +
      '<path d="M8.4 12.6l2.2-2.3 1.7 1.5 3-3" fill="none" stroke="#e0930a" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/>',
    text: '<path d="M7 7.8h10M7 10.6h10M7 13.4h10M7 16.2h6" ' + st(1.7) + '/>',
    program: '<rect x="6.4" y="6.4" width="4.9" height="4.9" rx="1.3" ' + W + '/><rect x="12.7" y="6.4" width="4.9" height="4.9" rx="1.3" ' + W + '/>' +
      '<rect x="6.4" y="12.7" width="4.9" height="4.9" rx="1.3" ' + W + '/><rect x="12.7" y="12.7" width="4.9" height="4.9" rx="2.45" ' + W + ' fill-opacity=".7"/>',
    disk: '<circle cx="12" cy="12" r="5.8" ' + st(1.8) + '/><circle cx="12" cy="12" r="1.7" ' + W + '/>',
    subtitle: '<rect x="5.8" y="7.2" width="12.4" height="9.6" rx="2" ' + W + '/><path d="M8.3 11h4.2M14.2 11h1.5M8.3 13.5h2M11.9 13.5h3.8" fill="none" stroke="#c29a06" stroke-width="1.3" stroke-linecap="round"/>',
    torrent: '<path d="M7.6 6.6v5.2a4.4 4.4 0 0 0 8.8 0V6.6" fill="none" stroke="#fff" stroke-width="2.8"/>' +
      '<path d="M6.2 6.6h2.8v2.3H6.2zM15 6.6h2.8v2.3H15z" fill="#e8f7d0"/><path d="M6.2 8.9h2.8M15 8.9h2.8" stroke="#5c9a12" stroke-width=".8"/>',
    stream: '<circle cx="12" cy="12" r="1.9" ' + W + '/><path d="M9.1 9.1a4.1 4.1 0 0 0 0 5.8M14.9 9.1a4.1 4.1 0 0 1 0 5.8M6.9 6.9a7.2 7.2 0 0 0 0 10.2M17.1 6.9a7.2 7.2 0 0 1 0 10.2" ' + st(1.6) + '/>',
    web: '<circle cx="12" cy="12" r="5.9" ' + st(1.6) + '/><ellipse cx="12" cy="12" rx="2.5" ry="5.9" ' + st(1.3) + '/><path d="M6.3 12h11.4" ' + st(1.3) + '/>',
    code: '<path d="M9.6 8.4 6 12l3.6 3.6M14.4 8.4 18 12l-3.6 3.6M12.9 7.5l-1.8 9" ' + st(1.7) + '/>',
    font: '<path d="M7.6 17.2 12 6.8l4.4 10.4M9.3 13.6h5.4" ' + st(1.9) + '/>',
    file: '<path d="M8 5.8h5.3l3.3 3.3v8.3c0 .5-.4.8-.8.8H8c-.5 0-.8-.3-.8-.8V6.6c0-.5.3-.8.8-.8z" ' + W + ' fill-opacity=".92"/><path d="M13.3 5.8v3.3h3.3" fill="none" stroke="#857c74" stroke-width="1" stroke-linejoin="round"/>',
    tray: '<path d="M6.2 13.2h2.7l1.2 1.8h3.8l1.2-1.8h2.7v3c0 .7-.5 1.2-1.2 1.2H7.4c-.7 0-1.2-.5-1.2-1.2z" ' + W + '/><path d="M12 6.3v5.2M9.8 9.4 12 11.6l2.2-2.2" ' + st(1.7) + '/>',
    progress: '<path d="M16.6 9.3A5.6 5.6 0 1 0 17.6 12" ' + st(1.8) + '/><path d="M12 8.6v5M10 11.8l2 2 2-2" ' + st(1.7) + '/>',
    check: '<path d="M7.4 12.4l3.1 3.1 6.1-6.3" ' + st(2.1) + '/>',
    clock: '<circle cx="12" cy="12" r="5.8" ' + st(1.7) + '/><path d="M12 8.9V12l2.2 1.5" ' + st(1.7) + '/>',
  };

  // ---- tiles: file types, categories, views ---------------------------------
  var FT = { video: "video", audio: "audio", image: "image", archive: "archive", pdf: "pdf", doc: "doc", sheet: "sheet",
    slides: "slides", text: "text", program: "program", disk: "disk", subtitle: "subtitle", torrent: "torrent",
    stream: "stream", web: "web", code: "code", font: "font", file: "file" };
  Object.keys(FT).forEach(function (k) { S["ft-" + k] = tile(k, GL[k]); });

  S["cat-general"] = '<path d="M2.5 6.6c0-1.2 1-2.1 2.1-2.1h4.3c.6 0 1.1.2 1.5.6l1.3 1.4h7.7c1.2 0 2.1 1 2.1 2.1v9.3c0 1.2-1 2.1-2.1 2.1H4.6c-1.2 0-2.1-1-2.1-2.1z" fill="url(#dbi-g-folderBack)"/>' +
    '<path d="M2.5 10.1c0-1.2 1-2.1 2.1-2.1h14.8c1.2 0 2.1 1 2.1 2.1v7.8c0 1.2-1 2.1-2.1 2.1H4.6c-1.2 0-2.1-1-2.1-2.1z" fill="url(#dbi-g-folder)"/>' +
    '';
  S["cat-compressed"] = S["ft-archive"]; S["cat-documents"] = S["ft-doc"]; S["cat-music"] = S["ft-audio"];
  S["cat-video"] = S["ft-video"]; S["cat-programs"] = S["ft-program"]; S["cat-images"] = S["ft-image"];

  S["view-all"] = tile("brand", GL.tray);
  S["view-unfinished"] = tile("amber", GL.progress);
  S["view-finished"] = tile("teal", GL.check);
  S["view-later"] = tile("sky", GL.clock);

  // ---- toolbar: colourful glyphs (no tile) ----------------------------------
  var OL = 'stroke="#000" stroke-opacity=".22" stroke-width=".8"';
  S["tb-add"] = '<circle cx="12" cy="12" r="9.6" fill="url(#dbi-g-green)" ' + OL + '/><path d="M7.4 7.2a6.6 6.6 0 0 1 9.2 0" fill="none" stroke="#fff" stroke-opacity=".3" stroke-width="1.6" stroke-linecap="round"/>' +
    '<path d="M12 7.6v8.8M7.6 12h8.8" ' + st(2.4) + '/>';
  S["tb-grab"] = '<path d="M10.2 13.8a3.6 3.6 0 0 0 5.1 0l3.3-3.3a3.6 3.6 0 0 0-5.1-5.1l-1.3 1.3" fill="none" stroke="url(#dbi-g-violet)" stroke-width="3" stroke-linecap="round"/>' +
    '<path d="M13.8 10.2a3.6 3.6 0 0 0-5.1 0l-3.3 3.3a3.6 3.6 0 0 0 5.1 5.1l1.3-1.3" fill="none" stroke="url(#dbi-g-blue)" stroke-width="3" stroke-linecap="round"/>' +
    '<circle cx="18.6" cy="18.6" r="3.3" fill="url(#dbi-g-amber)" ' + OL + '/><path d="M17.3 18.6h2.6M18.6 17.3v2.6" ' + st(1.3) + '/>';
  S["tb-resume"] = '<path d="M3.5 15.5v2.7c0 1.3 1 2.3 2.3 2.3h12.4c1.3 0 2.3-1 2.3-2.3v-2.7" fill="none" stroke="url(#dbi-g-slate)" stroke-width="2.4" stroke-linecap="round"/>' +
    '<path d="M10 3.5h4c.6 0 1 .4 1 1v5.3h2.4c.7 0 1 .8.6 1.3l-5.2 5.5c-.4.4-1 .4-1.4 0l-5.2-5.5c-.4-.5-.1-1.3.6-1.3H9V4.5c0-.6.4-1 1-1z" fill="url(#dbi-g-blue)" ' + OL + '/>';
  S["tb-stop"] = '<rect x="4" y="4" width="16" height="16" rx="4.2" fill="url(#dbi-g-red)" ' + OL + '/><path d="M7.4 7.2h9.2" stroke="#fff" stroke-opacity=".3" stroke-width="1.6" stroke-linecap="round"/>' +
    '<rect x="9" y="9" width="6" height="6" rx="1.2" ' + W + '/>';
  S["tb-stopall"] = '<rect x="7.2" y="2.8" width="14" height="14" rx="3.6" fill="url(#dbi-g-orange)" ' + OL + ' opacity=".75"/>' +
    '<rect x="2.8" y="7.2" width="14" height="14" rx="3.6" fill="url(#dbi-g-amber)" ' + OL + '/><path d="M7.8 11v6.4M11.8 11v6.4" ' + st(2.2) + '/>';
  S["tb-delete"] = '<path d="M5.6 7.5h12.8l-1 11.6c-.1 1.1-1 1.9-2.1 1.9H8.7c-1.1 0-2-.8-2.1-1.9z" fill="url(#dbi-g-rose)" ' + OL + '/>' +
    '<rect x="3.6" y="4.8" width="16.8" height="3.1" rx="1.5" fill="url(#dbi-g-red)" ' + OL + '/><path d="M9.7 4.8V3.9c0-.6.4-1 1-1h2.6c.6 0 1 .4 1 1v.9" fill="none" stroke="url(#dbi-g-red)" stroke-width="1.6"/>' +
    '<path d="M10 11v6.4M14 11v6.4" ' + st(1.6) + ' stroke-opacity=".85"/>';
  S["tb-deletecompleted"] = '<path d="M4.6 7.5h11.2l-.9 10.2c-.1 1-.9 1.7-1.8 1.7H7.3c-1 0-1.8-.7-1.8-1.7z" fill="url(#dbi-g-steel)" ' + OL + '/>' +
    '<rect x="3" y="4.9" width="14.4" height="2.8" rx="1.4" fill="url(#dbi-g-slate)" ' + OL + '/>' +
    '<circle cx="17" cy="16.6" r="5.2" fill="url(#dbi-g-green)" ' + OL + '/><path d="M14.7 16.7l1.6 1.6 3.1-3.2" ' + st(1.7) + '/>';
  S["tb-options"] = '<path d="' + gearPath(12, 12, 10, 7.3, 8, 3.1) + '" fill="url(#dbi-g-slate)" fill-rule="evenodd" ' + OL + '/>' +
    '<circle cx="12" cy="12" r="4.9" fill="none" stroke="#fff" stroke-opacity=".35" stroke-width="1.2"/>';
  S["tb-scheduler"] = '<path d="M4.2 6.6 7 3.8M19.8 6.6 17 3.8" stroke="url(#dbi-g-red)" stroke-width="2.4" stroke-linecap="round"/>' +
    '<path d="M7.3 19.4 6 21M16.7 19.4 18 21" stroke="url(#dbi-g-slate)" stroke-width="2" stroke-linecap="round"/>' +
    '<circle cx="12" cy="12.6" r="8" fill="url(#dbi-g-amber)" ' + OL + '/><circle cx="12" cy="12.6" r="5.9" fill="#fff" fill-opacity=".95"/>' +
    '<path d="M12 9.2v3.6l2.4 1.6" fill="none" stroke="#c46f06" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/>';
  S["tb-later"] = '<path d="M6.5 3h11M6.5 21h11" stroke="url(#dbi-g-slate)" stroke-width="2.4" stroke-linecap="round"/>' +
    '<path d="M7.6 3.8h8.8v2.6c0 1.6-.9 3.1-2.3 3.9L12.6 11a1.2 1.2 0 0 0 0 2l1.5.8c1.4.8 2.3 2.3 2.3 3.9v2.5H7.6v-2.5c0-1.6.9-3.1 2.3-3.9l1.5-.8a1.2 1.2 0 0 0 0-2l-1.5-.8C8.5 9.4 7.6 8 7.6 6.4z" fill="url(#dbi-g-sky)" ' + OL + '/>' +
    '<path d="M9.3 18.6c0-1.2 1.2-2.4 2.7-3 1.5.6 2.7 1.8 2.7 3z" fill="#fff" fill-opacity=".9"/><path d="M10 7.2h4" stroke="#fff" stroke-opacity=".8" stroke-width="1.4" stroke-linecap="round"/>';
  S["tb-start"] = '<path d="M7 4.9v14.2c0 1 1.1 1.6 2 1.1l11-7.1c.8-.5.8-1.7 0-2.2L9 3.8c-.9-.5-2 .1-2 1.1z" fill="url(#dbi-g-green)" ' + OL + '/>' +
    '<path d="M9 6.8l5.5 3.5" stroke="#fff" stroke-opacity=".4" stroke-width="1.4" stroke-linecap="round"/>';
  S["tb-startall"] = '<path d="M11.5 6.2v11.6c0 .9 1 1.4 1.7.9l8.4-5.8c.6-.4.6-1.4 0-1.8l-8.4-5.8c-.7-.5-1.7 0-1.7.9z" fill="url(#dbi-g-teal)" ' + OL + '/>' +
    '<path d="M2.4 6.2v11.6c0 .9 1 1.4 1.7.9l8.4-5.8c.6-.4.6-1.4 0-1.8L4.1 5.3c-.7-.5-1.7 0-1.7.9z" fill="url(#dbi-g-green)" ' + OL + '/>';
  S["tb-skip"] = '<path d="M4 5.3v13.4c0 .9 1 1.5 1.8 1L15 13c.7-.5.7-1.5 0-2L5.8 4.3C5 3.8 4 4.4 4 5.3z" fill="url(#dbi-g-blue)" ' + OL + '/>' +
    '<rect x="16.4" y="4" width="3.8" height="16" rx="1.9" fill="url(#dbi-g-slate)" ' + OL + '/>';
  S["tb-preview"] = '<rect x="7" y="3.2" width="14" height="11" rx="2.4" fill="url(#dbi-g-indigo)" ' + OL + ' opacity=".8"/>' +
    '<rect x="3" y="8.8" width="14" height="11.6" rx="2.4" fill="url(#dbi-g-violet)" ' + OL + '/><path d="M3.4 12.2h13.2" stroke="#fff" stroke-opacity=".7" stroke-width="1.3"/>' +
    '<circle cx="5.4" cy="10.6" r=".75" ' + W + '/><circle cx="7.6" cy="10.6" r=".75" ' + W + '/>';
  S["tb-ui"] = '<path d="M12 2.8a9.2 9.2 0 1 0 0 18.4c1.2 0 1.9-.8 1.9-1.8 0-1.3-1.1-1.6-1.1-2.7 0-1 .8-1.8 1.9-1.8h2.3a4.4 4.4 0 0 0 4.4-4.4C21.4 6.3 17.2 2.8 12 2.8z" fill="url(#dbi-g-steel)" ' + OL + '/>' +
    '<circle cx="7.4" cy="11.2" r="1.7" fill="#e0357f"/><circle cx="9.6" cy="6.9" r="1.7" fill="#f0a818"/><circle cx="14.5" cy="6.6" r="1.7" fill="#18a650"/><circle cx="17.6" cy="10.2" r="1.7" fill="#2a6fe0"/>';

  // ---- status discs ---------------------------------------------------------
  S["st-downloading"] = disc("blue", '<path d="M12 6.8v8.4M8.6 11.9 12 15.3l3.4-3.4" ' + st(2.2) + '/>');
  S["st-queued"] = disc("slate", '<circle cx="8" cy="12" r="1.4" ' + W + '/><circle cx="12" cy="12" r="1.4" ' + W + '/><circle cx="16" cy="12" r="1.4" ' + W + '/>');
  S["st-paused"] = disc("slate", '<path d="M9.8 8.2v7.6M14.2 8.2v7.6" ' + st(2.4) + '/>');
  S["st-completed"] = disc("green", '<path d="M7.6 12.3l3 3 5.8-6" ' + st(2.3) + '/>');
  S["st-failed"] = disc("red", '<path d="M12 7v6" ' + st(2.4) + '/><circle cx="12" cy="16.6" r="1.4" ' + W + '/>');
  S["st-scheduled"] = disc("amber", '<path d="M12 7.4V12l3 2" ' + st(2.2) + '/>');
  S["st-later"] = disc("sky", '<path d="M12 7.4V12l3 2" ' + st(2.2) + '/>');
  S["st-skipped"] = disc("slate", '<path d="M8.4 8v8l5.6-4z" ' + W + '/><path d="M15.8 8v8" ' + st(2) + '/>');
  S["st-live"] = '<circle cx="12" cy="12" r="10" fill="#e42c55" fill-opacity=".2"/><circle cx="12" cy="12" r="6.2" fill="url(#dbi-g-stream)"/><circle cx="12" cy="12" r="2.6" ' + W + '/>';
  S["st-resolving"] = '<circle cx="12" cy="12" r="8.4" fill="none" stroke="#5f6b7d" stroke-opacity=".35" stroke-width="3"/>' +
    '<path d="M12 3.6a8.4 8.4 0 0 1 8.4 8.4" fill="none" stroke="url(#dbi-g-blue)" stroke-width="3" stroke-linecap="round"/>';

  // ---- ui-*: line icons in currentColor ---------------------------------------
  var U = {
    open: '<path d="M14 3.5h6.5V10M20.2 3.8 11 13"/><path d="M18 14.5v4.5c0 .8-.7 1.5-1.5 1.5H5c-.8 0-1.5-.7-1.5-1.5V7.5C3.5 6.7 4.2 6 5 6h4.5"/>',
    folder: '<path d="M3.5 7.2c0-1 .8-1.7 1.7-1.7h4l2 2h7.6c1 0 1.7.8 1.7 1.7v8.6c0 1-.8 1.7-1.7 1.7H5.2c-1 0-1.7-.8-1.7-1.7z"/>',
    copy: '<rect x="8.5" y="8.5" width="12" height="12" rx="2"/><path d="M15.5 5.5V5c0-.8-.7-1.5-1.5-1.5H5c-.8 0-1.5.7-1.5 1.5v9c0 .8.7 1.5 1.5 1.5h.5"/>',
    rename: '<path d="M4 20h4.2L19.4 8.8a2.1 2.1 0 0 0-3-3L5.2 17v3z"/><path d="M14.6 7.6l3 3"/>',
    move: '<path d="M3.5 7.2c0-1 .8-1.7 1.7-1.7h4l2 2h7.6c1 0 1.7.8 1.7 1.7v8.6c0 1-.8 1.7-1.7 1.7H5.2c-1 0-1.7-.8-1.7-1.7z"/><path d="M9 13.5h6.5M13 11l2.5 2.5L13 16"/>',
    trash: '<path d="M4 6.8h16M9.3 6.8V4.6c0-.6.4-1.1 1-1.1h3.4c.6 0 1 .5 1 1.1v2.2M6.2 6.8l.9 12.3c.1.8.7 1.4 1.5 1.4h6.8c.8 0 1.4-.6 1.5-1.4l.9-12.3M10 10.8v5.6M14 10.8v5.6"/>',
    pause: '<path d="M9 5.5v13M15 5.5v13"/>',
    play: '<path d="M7.5 5.3v13.4c0 .8.9 1.3 1.6.8l10-6.7c.6-.4.6-1.2 0-1.6l-10-6.7c-.7-.5-1.6 0-1.6.8z"/>',
    stop: '<rect x="5.5" y="5.5" width="13" height="13" rx="2.5"/>',
    close: '<path d="M6.5 6.5l11 11M17.5 6.5l-11 11"/>',
    check: '<path d="M5 12.6l4.6 4.6L19 7.6"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    link: '<path d="M10.2 13.8a3.8 3.8 0 0 0 5.4 0l3.1-3.1a3.8 3.8 0 0 0-5.4-5.4l-1.2 1.2"/><path d="M13.8 10.2a3.8 3.8 0 0 0-5.4 0l-3.1 3.1a3.8 3.8 0 0 0 5.4 5.4l1.2-1.2"/>',
    search: '<circle cx="10.8" cy="10.8" r="6.3"/><path d="M15.5 15.5 20 20"/>',
    filter: '<path d="M4 5.5h16l-6.2 7.2v5.6l-3.6 1.8v-7.4z"/>',
    "chevron-down": '<path d="M6.5 9.5 12 15l5.5-5.5"/>',
    "chevron-right": '<path d="M9.5 6.5 15 12l-5.5 5.5"/>',
    refresh: '<path d="M19.5 12a7.5 7.5 0 1 1-2.2-5.3"/><path d="M19.8 4.5v4.4h-4.4"/>',
    clock: '<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>',
    calendar: '<rect x="4" y="5.5" width="16" height="14.5" rx="2.2"/><path d="M4 10h16M8.5 3.5v4M15.5 3.5v4"/>',
    gear: '<circle cx="12" cy="12" r="3"/><path d="' + gearPath(12, 12, 9.2, 6.9, 8, 0) + '"/>',
    palette: '<path d="M12 3.5a8.5 8.5 0 1 0 0 17c1 0 1.6-.7 1.6-1.6 0-1.2-1-1.4-1-2.5 0-.9.7-1.6 1.6-1.6h2.1a4.2 4.2 0 0 0 4.2-4.2c0-4-3.8-7.1-8.5-7.1z"/><circle cx="7.8" cy="11.5" r=".6"/><circle cx="10" cy="7.5" r=".6"/><circle cx="14.5" cy="7.3" r=".6"/>',
    info: '<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5.2M12 7.9v.1"/>',
    warning: '<path d="M10.4 4.6 3 17.8c-.7 1.2.2 2.7 1.6 2.7h14.8c1.4 0 2.3-1.5 1.6-2.7L13.6 4.6c-.7-1.2-2.5-1.2-3.2 0z"/><path d="M12 9.5v4.4M12 17v.1"/>',
    download: '<path d="M12 4v10.5M7.5 10l4.5 4.5 4.5-4.5M5 19.5h14"/>',
    update: '<path d="M19.5 12a7.5 7.5 0 1 1-7.5-7.5"/><path d="M12 8.5v7M9 12.5l3 3 3-3"/><path d="M16.5 3.8l2.8.2.2 2.8"/>',
    list: '<path d="M9 6.5h11M9 12h11M9 17.5h11M4.8 6.5v.01M4.8 12v.01M4.8 17.5v.01"/>',
    clipboard: '<rect x="5" y="5" width="14" height="15.5" rx="2"/><rect x="9" y="3.2" width="6" height="3.6" rx="1"/>',
    external: '<path d="M14 4h6v6M20 4l-8.5 8.5"/><path d="M18 14v5c0 .6-.4 1-1 1H5c-.6 0-1-.4-1-1V7c0-.6.4-1 1-1h5"/>',
    status: '<path d="M5 20v-7M10 20V6M15 20v-9M20 20v-5"/>',
    retry: '<path d="M4.5 12a7.5 7.5 0 1 0 2.2-5.3"/><path d="M4.2 4.5v4.4h4.4"/>',
    sort: '<path d="M8 5v14M5 16l3 3 3-3M16 19V5M13 8l3-3 3 3"/>',
    eye: '<path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z"/><circle cx="12" cy="12" r="3"/>',
    globe: '<circle cx="12" cy="12" r="8.5"/><ellipse cx="12" cy="12" rx="3.8" ry="8.5"/><path d="M3.5 12h17"/>',
    video: '<rect x="3" y="6" width="13" height="12" rx="2.2"/><path d="M16 10.2l5-3v9.6l-5-3"/>',
    music: '<path d="M9 17.5V6.2l11-2.2v11.3"/><circle cx="6.5" cy="17.5" r="2.5"/><circle cx="17.5" cy="15.3" r="2.5"/>',
    image: '<rect x="3.5" y="4.5" width="17" height="15" rx="2.2"/><circle cx="9" cy="10" r="1.7"/><path d="M20.5 15.5 15.8 11 7 19.5"/>',
    file: '<path d="M6.5 3.5h7l4.5 4.5v11.5c0 .6-.4 1-1 1h-10.5c-.6 0-1-.4-1-1v-15c0-.6.4-1 1-1z"/><path d="M13.5 3.5V8H18"/>',
    archive: '<rect x="3.5" y="4.5" width="17" height="4.5" rx="1.2"/><path d="M5 9v9.5c0 .6.4 1 1 1h12c.6 0 1-.4 1-1V9M10 13h4"/>',
    "select-all": '<rect x="4" y="4" width="16" height="16" rx="3.5"/><path d="M8.2 12.2l2.6 2.6 5-5.2"/>',
    "select-none": '<rect x="4" y="4" width="16" height="16" rx="3.5"/>',
    speed: '<path d="M4.4 17.5a8.5 8.5 0 1 1 15.2 0"/><path d="M12 13.5l4.2-4.2"/><circle cx="12" cy="13.5" r="1"/>',
    shield: '<path d="M12 3.2 19.5 6v5.8c0 4.3-3.1 7.9-7.5 9-4.4-1.1-7.5-4.7-7.5-9V6z"/>',
    menu: '<path d="M4 7h16M4 12h16M4 17h16"/>',
    paste: '<rect x="5" y="5" width="14" height="15.5" rx="2"/><rect x="9" y="3.2" width="6" height="3.6" rx="1"/><path d="M9 12h6M9 15.5h4"/>',
  };
  Object.keys(U).forEach(function (k) {
    S["ui-" + k] = '<g fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">' + U[k] + "</g>";
  });

  // ---- md-*: dialog badges ------------------------------------------------------
  S["md-done"] = badge("green", '<path d="M5 12.6l4.6 4.6L19 7.6"/>');
  S["md-edit"] = badge("blue", U.rename);
  S["md-settings"] = badge("slate", '<circle cx="12" cy="12" r="3"/><path d="' + gearPath(12, 12, 9.2, 6.9, 8, 0) + '"/>');
  S["md-about"] = badge("brand", '<path d="M12 3 20 7.2v9.6L12 21l-8-4.2V7.2z"/><path d="M4 7.2 12 11.4l8-4.2M12 11.4V21"/>');
  S["md-update"] = badge("blue", U.update);
  S["md-schedule"] = badge("amber", U.clock);
  S["md-playlist"] = badge("red", '<path d="M4 6.5h11M4 11.5h11M4 16.5h6"/><path d="M15 14v6l5-3z"/>');
  S["md-grab"] = badge("violet", U.link);
  S["md-warn"] = badge("amber", U.warning);
  S["md-ui"] = badge("pink", U.palette);
  S["md-error"] = badge("red", '<circle cx="12" cy="12" r="8.5"/><path d="M9 9l6 6M15 9l-6 6"/>');

  // ---- illustrations (viewBox 0 0 120 100) -----------------------------------
  VB["ill-empty"] = VB["ill-grab"] = VB["ill-nothing"] = "0 0 120 100";
  var boxBase =
    '<ellipse cx="60" cy="88" rx="38" ry="6" fill="#000" fill-opacity=".18"/>' +
    '<path d="M24 44 60 56v31L24 75z" fill="url(#dbi-g-box)" stroke="#8a96a8" stroke-width="2" stroke-linejoin="round"/>' +
    '<path d="M96 44 60 56v31l36-12z" fill="#d7dee8" stroke="#8a96a8" stroke-width="2" stroke-linejoin="round"/>' +
    '<path d="M24 44 60 56 50 66 14 54z" fill="#fff" stroke="#8a96a8" stroke-width="2" stroke-linejoin="round"/>' +
    '<path d="M96 44 60 56l10 10 36-12z" fill="#eef2f7" stroke="#8a96a8" stroke-width="2" stroke-linejoin="round"/>' +
    '<path d="M24 44 60 33l36 11" fill="none" stroke="#8a96a8" stroke-width="2" stroke-linejoin="round" stroke-dasharray="3 3" opacity=".7"/>';
  S["ill-empty"] = boxBase +
    '<path d="M52 6h16c1.7 0 3 1.3 3 3v19h8.2c2.3 0 3.4 2.7 1.8 4.3L62.1 50.9a3 3 0 0 1-4.2 0L39 32.3c-1.6-1.6-.5-4.3 1.8-4.3H49V9c0-1.7 1.3-3 3-3z" fill="url(#dbi-g-brand)" stroke="#12823d" stroke-width="1.5" stroke-linejoin="round"/>' +
    '<path d="M54 10v16" stroke="#fff" stroke-opacity=".45" stroke-width="3" stroke-linecap="round"/>';
  S["ill-nothing"] = boxBase +
    '<circle cx="60" cy="24" r="15" fill="url(#dbi-g-slate)" stroke="#4b5566" stroke-width="1.5"/>' +
    '<path d="M55 19.5a5 5 0 1 1 7 4.6c-1.3.6-2 1.6-2 3v1" fill="none" stroke="#fff" stroke-width="3.2" stroke-linecap="round"/><circle cx="60" cy="33" r="2" fill="#fff"/>';
  S["ill-grab"] =
    '<ellipse cx="60" cy="90" rx="40" ry="5.5" fill="#000" fill-opacity=".16"/>' +
    '<rect x="22" y="8" width="62" height="78" rx="7" fill="#f4f6f9" stroke="#8a96a8" stroke-width="2"/>' +
    '<path d="M22 20h62" stroke="#8a96a8" stroke-width="2"/><circle cx="30" cy="14" r="2" fill="#f0544c"/><circle cx="37" cy="14" r="2" fill="#f0a31c"/><circle cx="44" cy="14" r="2" fill="#2fcc66"/>' +
    '<rect x="30" y="28" width="16" height="16" rx="4" fill="url(#dbi-g-video)"/><path d="M36 32.5v7l5.5-3.5z" fill="#fff"/>' +
    '<rect x="30" y="50" width="16" height="16" rx="4" fill="url(#dbi-g-archive)"/><rect x="35" y="55" width="6" height="6" rx="1.5" fill="none" stroke="#fff" stroke-width="1.6"/>' +
    '<path d="M52 33h24M52 38h16M52 55h24M52 60h12M30 74h40" stroke="#b8c2cf" stroke-width="3" stroke-linecap="round"/>' +
    '<circle cx="84" cy="62" r="17" fill="#fff" fill-opacity=".35" stroke="url(#dbi-g-violet)" stroke-width="6"/>' +
    '<path d="M96.5 74.5 108 86" stroke="url(#dbi-g-slate)" stroke-width="8" stroke-linecap="round"/>' +
    '<path d="M76 55a10 10 0 0 1 8-4" fill="none" stroke="#fff" stroke-width="3" stroke-linecap="round" opacity=".8"/>';

  // ---- sprite ----------------------------------------------------------------
  var sprite = '<svg xmlns="http://www.w3.org/2000/svg" id="dbi-sprite" aria-hidden="true" ' +
    'style="position:absolute;width:0;height:0;overflow:hidden"><defs>' + defs + "</defs>";
  Object.keys(S).forEach(function (k) {
    sprite += '<symbol id="dbi-' + k + '" viewBox="' + (VB[k] || "0 0 24 24") + '">' + S[k] + "</symbol>";
  });
  sprite += "</svg>";
  function inject() {
    if (document.getElementById("dbi-sprite") || !document.body) return;
    document.body.insertAdjacentHTML("afterbegin", sprite);
  }
  if (document.body) inject();
  else document.addEventListener("DOMContentLoaded", inject);

  // ---- file kinds ----------------------------------------------------------------
  var KINDS = {
    video: "mp4 m4v mkv webm mov avi flv wmv mpg mpeg ts m2ts mts 3gp ogv vob rm rmvb f4v divx asf",
    audio: "mp3 m4a aac flac wav ogg oga opus wma aiff aif alac amr mka weba mid midi ape",
    image: "jpg jpeg png gif webp bmp svg tif tiff ico heic heif avif psd raw cr2 nef dng jfif",
    archive: "zip rar 7z gz tgz bz2 xz zst tar cab lz lzma z arj",
    pdf: "pdf", doc: "doc docx odt rtf epub mobi azw3 djvu pages wpd",
    sheet: "xls xlsx ods csv tsv numbers", slides: "ppt pptx odp key",
    text: "txt md log nfo ini cfg conf yml yaml toml json xml",
    program: "exe msi msix msixbundle appx appxbundle apk xapk aab dmg pkg deb rpm appimage bat cmd ps1 jar run bin sh com scr",
    disk: "iso img vhd vhdx vmdk ova ovf wim esd",
    subtitle: "srt vtt ass ssa sub idx",
    torrent: "torrent", stream: "m3u8 m3u mpd ism f4m",
    web: "html htm xhtml mht mhtml php asp aspx jsp shtml",
    code: "js ts css py go java c cpp h cs rb rs swift kt sql lua pl",
    font: "ttf otf woff woff2 eot fon",
  };
  var EXT = {};
  Object.keys(KINDS).forEach(function (k) { KINDS[k].split(" ").forEach(function (e) { EXT[e] = k; }); });
  function extOf(x) {
    x = String(x || "").trim();
    if (/^[a-z0-9]{1,10}$/i.test(x)) return x.toLowerCase(); // a bare extension
    x = x.replace(/[?#].*$/, "");
    var m = /\.([a-z0-9]{1,10})$/i.exec(x);
    return m ? m[1].toLowerCase() : "";
  }
  function kindOf(x) {
    var e = extOf(x);
    return (e === "ts" && /\.ts$/i.test(String(x))) ? "video" : (EXT[e] || "file");
  }
  var CAT = { General: "cat-general", Compressed: "cat-compressed", Documents: "cat-documents", Music: "cat-music",
    Video: "cat-video", Programs: "cat-programs", Images: "cat-images" };

  function esc(s) { return String(s).replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; }); }
  function icon(name, size, cls) {
    if (!S[name]) name = "ft-file";
    size = size || 16;
    return '<svg class="dbi dbi-' + name + (cls ? " " + esc(cls) : "") + '" width="' + size + '" height="' + size +
      '" aria-hidden="true" focusable="false"><use href="#dbi-' + name + '"/></svg>';
  }

  window.DBI = {
    icon: icon,
    cat: function (name, size) { return icon(CAT[name] || "cat-general", size); },
    kindOf: kindOf,
    ext: extOf,
    file: function (x, size) { return icon("ft-" + kindOf(x), size); },
    badge: function (x, size) {
      size = size || 48;
      var e = extOf(x).toUpperCase().slice(0, 5);
      var pill = e ? '<span style="position:absolute;left:50%;bottom:' + (-Math.round(size / 12)) + 'px;transform:translateX(-50%);' +
        "padding:0 " + Math.round(size / 9) + "px;border-radius:" + Math.round(size / 8) + "px;background:rgba(17,20,26,.86);color:#fff;" +
        "font:700 " + Math.max(9, Math.round(size / 4.6)) + 'px/1.45 \'Segoe UI\',sans-serif;letter-spacing:.05em;white-space:nowrap;' +
        'box-shadow:0 1px 3px rgba(0,0,0,.35)">' + esc(e) + "</span>" : "";
      return '<span class="dbi-badge" style="position:relative;display:inline-block;width:' + size + "px;height:" + size +
        'px;line-height:0">' + icon("ft-" + kindOf(x), size) + pill + "</span>";
    },
    names: function () { return Object.keys(S); },
    has: function (n) { return !!S[n]; },
  };
})();
