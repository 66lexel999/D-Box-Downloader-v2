// Media panel, opened from the toolbar icon's right-click menu ("Media on this
// page…"; a click on the icon itself opens D BOX): enable toggle, live D BOX
// status, the media captured on the current tab (every frame, best first — the
// same list the overlay shows), "Grab all links" (D BOX's Link Grabber) and
// quick links. Opened as a small window (no anchored popup available), it
// gets its tab as ?tab=<id>.

const DEFAULTS = { enabled: true, myidmBase: "http://127.0.0.1:8081" };
const H = globalThis.DBoxHelpers;

async function settings() {
  const { settings } = await chrome.storage.local.get("settings");
  return Object.assign({}, DEFAULTS, settings || {});
}
const send = (msg) => new Promise((resolve) => {
  try { chrome.runtime.sendMessage(msg, (r) => { void chrome.runtime.lastError; resolve(r || { ok: false }); }); }
  catch (_) { resolve({ ok: false }); }
});

async function showCaptures(tab) {
  const box = document.getElementById("caps");
  box.textContent = "";
  if (!tab) return;
  const r = await send({ type: "sniffed", tabId: tab.id });
  const list = ((r && r.streams) || []).slice(0, 15);
  const h = document.createElement("h2");
  h.textContent = list.length ? "Media on this page" : "";
  if (list.length) box.appendChild(h);
  else {
    const e = document.createElement("div");
    e.className = "empty";
    e.textContent = "No video or audio captured on this tab yet — play it for a moment.";
    box.appendChild(e);
  }
  for (const s of list) {
    const row = document.createElement("div");
    row.className = "cap";
    row.title = s.url;
    const l = document.createElement("span"); l.className = "l"; l.textContent = s.label || s.ext;
    const z = document.createElement("span"); z.className = "s"; z.textContent = s.kind === "hls" ? "" : H.humanBytes(s.size);
    row.append(l, z);
    row.addEventListener("click", async () => {
      l.textContent = "Sending…";
      const res = await send({ type: "promptStream", tabId: tab.id, payload: { key: s.key, url: s.url, kind: s.kind, ext: s.ext } });
      l.textContent = res && res.ok ? "Sent to D BOX ✓" : "Failed: " + ((res && res.error) || "error");
    });
    box.appendChild(row);
  }
}

async function init() {
  const s = await settings();
  document.getElementById("enabled").checked = s.enabled;

  const base = s.myidmBase.replace(/\/+$/, "");
  const dot = document.getElementById("dot");
  const txt = document.getElementById("statusText");
  try {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), 2500);
    const r = await fetch(base + "/api/categories", { signal: ctrl.signal });
    clearTimeout(t);
    if (r.ok) { dot.className = "dot up"; txt.textContent = "D BOX running"; }
    else throw new Error();
  } catch {
    dot.className = "dot down"; txt.textContent = "D BOX not reachable";
  }

  let tab = null;
  const want = new URLSearchParams(location.search).get("tab");
  try {
    if (want !== null) tab = await chrome.tabs.get(Number(want));
    else [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  } catch (_) {}
  showCaptures(tab);

  document.getElementById("enabled").addEventListener("change", async (e) => {
    const cur = await settings();
    cur.enabled = e.target.checked;
    await chrome.storage.local.set({ settings: cur });
  });
  const grab = document.getElementById("grab");
  grab.disabled = !(tab && /^https?:/i.test(tab.url || ""));
  grab.addEventListener("click", async () => {
    grab.textContent = "Opening Link Grabber…";
    const r = await send({ type: "grabPage", tabId: tab.id });
    grab.textContent = r && r.ok ? "Opened in D BOX ✓" : "Failed: " + ((r && r.error) || "error");
  });
  document.getElementById("open").addEventListener("click", async () => {
    await send({ type: "openApp" }); // brings up the D BOX window (or its page in a tab)
    window.close();
  });
  document.getElementById("opts").addEventListener("click", () =>
    chrome.runtime.openOptionsPage());
}

init();
