# Publishing D BOX Integration to the Chrome Web Store

Brave installs from the **Chrome Web Store**, so publishing there covers Brave,
Chrome, Edge (Chromium) and gives you **automatic updates** — users never have to
reload the extension again.

Everything you need is in this `store/` folder:
- `dbox-integration-3.4.0.zip` — the packaged extension to upload
- `LISTING.md` — copy/paste text for every dashboard field
- `promo-1280x800.png` — the required screenshot
- `icons/icon128.png` (in the extension) — the store icon
- Privacy policy is live at https://github.com/66lexel999/dbox/blob/main/PRIVACY.md

---

## Step 1 — One-time developer account ($5, you must do this)
1. Go to https://chrome.google.com/webstore/devconsole
2. Sign in with the Google account you want to own the extension.
3. Pay the **one-time $5 registration fee** (Google Payments). *I can't do this
   for you — it needs your Google account and card.*
4. Accept the developer agreement.

## Step 2 — Create the item
1. In the dashboard click **+ New item**.
2. Upload **`store/dbox-integration-3.4.0.zip`**.
3. It unpacks and shows the item draft.

## Step 3 — Fill the listing (from `LISTING.md`)
1. **Store listing** tab → paste the name, summary, description; set category
   *Productivity*; language *English*.
2. Upload **`promo-1280x800.png`** under Screenshots (at least one is required).
3. The 128×128 store icon is read from the package automatically.

## Step 4 — Privacy tab (the important one)
1. **Single purpose** → paste from `LISTING.md`.
2. **Permission justifications** → paste the one-liner for each permission
   (downloads, storage, notifications, tabs, cookies, webRequest, host access).
3. **Remote code** → *No*.
4. **Data usage** → tick the three compliance boxes; note that data goes only to
   the user's local machine (127.0.0.1), never off-device.
5. **Privacy policy URL** →
   `https://github.com/66lexel999/dbox/blob/main/PRIVACY.md`

## Step 5 — Distribution
- Set visibility to **Unlisted** (recommended to start).
- All regions.

## Step 6 — Submit
- Click **Submit for review**. Review typically takes a few days (sometimes up to
  ~2 weeks for first submissions with sensitive permissions).

---

## Shipping updates later (this is the payoff)
1. Bump `"version"` in `manifest.json` (e.g. `3.2.1`).
2. Re-zip the extension (same file list; keep forward-slash paths).
3. Dashboard → your item → **Package** → **Upload new package** → Submit.
4. Once approved, **every user auto-updates** within hours — no manual reload.

---

## Honest heads-up on approval
Your riskiest permissions for review are **`cookies`** combined with
**`<all_urls>`** — reviewers scrutinize any extension that can read cookies on
all sites. Your justification is strong (cookies go only to the user's own
localhost app, never off-device, only for downloads the user initiates), and download-manager companions like IDM's own extension are on the
store, so it's very publishable. But if it's rejected:
- **Easiest fix:** ship a store build WITHOUT the `cookies` permission (drop it
  from `manifest.json` + the `ensureCookies` / `cookieHeader` calls). You lose only the automatic
  Instagram-login passthrough; everything else — file capture, video/iframe
  detection, HLS streams — still works. I can produce that trimmed build in a
  minute if needed.
- Or reply to the reviewer with the privacy-policy link and the "all data stays
  on localhost" explanation.

Tell me if you'd rather I prepare the cookies-free store build up front to make
approval smoother.
