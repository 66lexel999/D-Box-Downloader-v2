// Settings: enable toggle + MyIDM address.

const DEFAULTS = { enabled: true, myidmBase: "http://127.0.0.1:8081" };
const $ = (id) => document.getElementById(id);

const flash = (msg, ok = true) => {
  $("status").textContent = msg;
  $("status").style.color = ok ? "" : "#dc2626";
  if (msg) setTimeout(() => { $("status").textContent = ""; }, 3000);
};

async function load() {
  const { settings } = await chrome.storage.local.get("settings");
  const s = Object.assign({}, DEFAULTS, settings || {});
  $("enabled").checked = s.enabled;
  $("myidmBase").value = s.myidmBase;
}

async function save() {
  const settings = {
    enabled: $("enabled").checked,
    myidmBase: ($("myidmBase").value || DEFAULTS.myidmBase).trim().replace(/\/+$/, "")
  };
  await chrome.storage.local.set({ settings });
  flash("Saved.");
}

async function test() {
  const base = ($("myidmBase").value || DEFAULTS.myidmBase).trim().replace(/\/+$/, "");
  try {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), 2000);
    const r = await fetch(base + "/api/categories", { signal: ctrl.signal });
    clearTimeout(t);
    if (!r.ok) throw new Error("HTTP " + r.status);
    const data = await r.json();
    flash(`Connected — ${data.categories.length} categories.`);
  } catch (e) {
    flash("Not reachable. Is D BOX running?", false);
  }
}

$("save").addEventListener("click", save);
$("test").addEventListener("click", test);
load();
