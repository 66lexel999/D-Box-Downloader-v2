# D BOX Integration — Privacy Policy

_Last updated: 2026-09-27_

**D BOX Integration** is the companion browser extension for the **D BOX**
desktop download manager. This policy explains exactly what the extension
accesses and where that data goes.

## The short version

**Nothing you do is sent to us or to any third party.** The extension has no
analytics, no tracking, and no remote servers. Every piece of data it touches is
sent only to the **D BOX application running on your own computer**
(`http://127.0.0.1:8081`, i.e. localhost). It never leaves your machine.

## What the extension accesses, and why

- **Downloads you start** — when you download a file, the extension reads its URL
  and filename so it can hand the download to D BOX instead of the browser's
  basic downloader.
- **Media on the page you're viewing** — the extension observes the network
  requests of the current tab to detect video/audio streams (e.g. an embedded
  player) and the page address they were requested from, so it can offer them to
  you for download. It only *observes*; it never blocks or modifies any request.
- **Site cookies (only for a download you start)** — when you download a file or
  video, the extension reads that site's cookies (for example your sign-in to
  Instagram, or a site's anti-bot clearance) so D BOX's request is the same as
  the browser's and the site serves the file. These cookies are passed only to
  your local D BOX app and are used only for that one download.
- **The page a download came from** — the page address (and, for streams, the
  Referer/Origin the video player sent) and the browser's User-Agent are passed
  to D BOX for the same reason.
- **Settings** — your on/off toggle and the D BOX address are stored in the
  browser's local extension storage.

## Where the data goes

Exclusively to `http://127.0.0.1:8081` — the D BOX app on your own computer.
The extension makes **no** connections to the developer or to any external
service. No data is collected, stored remotely, sold, or shared.

## Your control

- Toggle capture off any time: right-click the extension's toolbar icon →
  **Capture downloads**.
- Remove the extension to stop all access immediately.
- Cookies are only read when you choose to download something; if D BOX isn't
  running, nothing is sent anywhere.

## Contact

Questions or issues: open an issue at
<https://github.com/66lexel999/dbox/issues>.
