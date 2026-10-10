# D BOX Integration (Brave / Chrome extension)

The browser side of D BOX, like IDM's integration module. Manifest V3.

- **Download capture** — when the browser starts a download D BOX should own
  (archives incl. split parts, installers, video/audio; not files under 50 KB
  unless archives/installers), the browser's copy is **paused**, handed to D BOX's
  New Download window, and only cancelled once D BOX accepted it. If D BOX is
  unreachable or refuses, the browser download is resumed — nothing is lost.
- **"Download this video" button** — appears over the largest video on a page
  (including players embedded in iframes, late-sized iframes and open shadow
  roots). One menu shows both **Found by D BOX** (yt-dlp formats for the page)
  and **Detected on the network** (the streams the page actually loaded). When
  yt-dlp only answered with its generic extractor, or its duration is >10% off
  the playing video, the network list goes first. **Drag the button** to put it
  anywhere on the video; the spot is remembered per site.
- **Right-click menu** — *Download link with D BOX*, *Download video with D BOX*
  (uses the captured stream for that player, or opens the page in the Link
  Grabber when nothing was captured) and *Grab all links from this page with
  D BOX* (opens D BOX's Link Grabber via `POST /api/grab/show`).
- **Toolbar** — one click on the icon brings up the D BOX window (`POST
  /api/show`; a headless or older D BOX opens its page in a tab). The badge
  counts the media captured on the tab; right-click the icon for *Media on this
  page…* (the panel listing them, every frame, best first), *Grab all links
  from this page* and the *Capture downloads* switch.
- **Image grabber** — hover a large image for a download badge (largest
  `srcset` candidate; the file name is only forced when the address has one).

## How streams are detected

| Source | What it sees |
| ------ | ------------ |
| `webRequest` (service worker) | every media/XHR response: URL, Content-Type, `Content-Range` totals, the Referer/Origin/custom headers the player sent; cache hits too (`onResponseStarted`); service-worker fetches (tabId -1) are filed under the tabs of the initiator's origin |
| `hook.js` (page's MAIN world, `document_start`, every frame incl. about:blank) | `fetch`/XHR bodies that are HLS/DASH/Smooth manifests (even as `text/plain`, octet-stream or behind extension-less URLs), JSON source lists, hls.js / shaka / dash.js / flv.js / mpegts sources, video.js & JW Player playlists (and titles), MSE use, custom request headers (hls.js `xhrSetup`) |

Rules (all in `lib/helpers.js`, unit-tested): HLS/DASH segments (`.ts`, `.m4s`,
`.cmfv`, `.cmfa`, `video/mp2t`, `video/iso.segment`, MSE XHR chunks, files under a
known manifest's folder) are never listed; ad hosts (IMA, DoubleClick,
googlesyndication…), UI sounds (`/s/search/audio/`) and VAST paths are dropped;
tiny clips (audio < 100 KB, video < 512 KB) are hidden when anything bigger
exists; a master playlist hides its variants; LL-HLS `_HLS_msn`/`_HLS_part`
reloads, `range`/`bytestart`/`byteend` chunks and cache busters collapse into one
entry. Manifests never show a byte size.

Titles come from the player (JW Player item title), JSON-LD, `og:title`,
`twitter:title`, the document title and `<h1>` — of the top page first, then of
the embed frame — with `" | Site"` suffixes and "Watch"/"شاهد" prefixes
stripped and random embed codes (`4172q6dt4qljs4kfrvr7`) rejected.

## What D BOX receives with a download

So that D BOX's request looks like the browser's own (many video hosts refuse
anything else — hotlink protection, Cloudflare rules), each hand-off carries:

| Field          | Source                                                                  |
| -------------- | ----------------------------------------------------------------------- |
| `referer`      | the Referer the player really sent, else its frame, else the tab (downloads: `item.referrer`, else the tab) |
| `headers`      | `Origin` and custom headers (Authorization, X-…) the player sent        |
| `cookies`      | `chrome.cookies` for the file's/stream's URL                            |
| `userAgent`    | `navigator.userAgent` (Cloudflare ties its clearance cookie to it)      |
| `title`        | the cleaned page/player title — a naming hint for generic server names  |
| `expectedSize`, `expectedMime`, `origUrl`, `finalUrl` | captured downloads only: what the browser saw |

For yt-dlp formats of an embedded player, D BOX gets the URL the probe really
resolved (the embed frame's own address) with the embedding page as Referer —
the same context the probe used.

Everything goes only to D BOX on `127.0.0.1`. D BOX keeps cookies server-side
for that one download (the New Download window gets a short-lived token, never
the cookies themselves).

Captured HLS/DASH streams open in D BOX's New Download window as normal
downloads: D BOX downloads HLS natively (best quality, separate audio track
included) and hands DASH to yt-dlp; if the server still refuses, D BOX retries
through yt-dlp with browser impersonation.

## Install / update

1. Start **D BOX**.
2. Remove the real "IDM Integration Module" if present (both capture downloads).
3. `brave://extensions` → **Developer mode** on → **Load unpacked** → this folder
   (Chrome/Brave 111+, needed for the MAIN-world page hook).
   To update an unpacked copy, replace the files and click the extension's
   **reload** (↻) button, then reload open tabs.
4. Pin the toolbar icon. **One click opens the D BOX window** (even when D BOX
   is waiting hidden in the tray). Right-click it for **Capture
   downloads** on/off, **Media on this page…**, **Grab all links from this
   page** and **Options** (D BOX's address).

## Why the overlay is in a Shadow DOM

The button, menu and image badge render inside a closed shadow root, so the
page's CSS can't restyle them (Bootstrap's `.row`, right-to-left pages, big
fonts, etc. used to add a horizontal scrollbar, wrap rows and clip the menu).

## Limits

- `blob:`/`data:` downloads (generated in-page) and POST downloads can't be
  handed off and stay with the browser — same as IDM.
- DRM-protected streams (Widevine/FairPlay) can't be downloaded.

## Tests

The pure rules and the hook's parsers have node unit tests:

```
node --test extension/test/helpers.test.js
```
