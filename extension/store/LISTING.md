# Chrome Web Store listing — copy/paste fields

Everything below goes into the Chrome Web Store **Developer Dashboard** when you
create the item. (Brave uses the same Chrome Web Store, so this covers both.)

---

## Store listing tab

**Item name**
```
D BOX Integration
```

**Summary** (132 chars max)
```
Send browser downloads and videos to D BOX — the free segmented download manager. Multi-connection speed, like IDM.
```

**Description**
```
D BOX Integration is the companion extension for D BOX, the free segmented
download manager for Windows. It captures your downloads and hands them to D BOX
so you get multi-connection speed, pause/resume, and crash recovery — instead of
the browser's basic downloader.

FEATURES
• One-click capture — file downloads are caught and sent to D BOX's New Download
  dialog automatically.
• Video downloads — a "Download this video" button appears on pages with video,
  including players embedded in an iframe.
• Stream detection — when a site streams video (HLS/MP4), the extension finds the
  real media and offers it for download, similar to IDM.
• Image grabber — hover any large image to download it.
• Works everywhere — no browser settings to change.

REQUIRES THE FREE D BOX APP
This extension does nothing on its own — it talks to the D BOX desktop app
running on your computer. Get D BOX (free, Windows) here:
https://66lexel999.github.io/dbox/

PRIVACY
Nothing leaves your computer. The extension sends data only to the D BOX app on
localhost (127.0.0.1). No analytics, no tracking, no remote servers.
Full policy: https://github.com/66lexel999/dbox/blob/main/PRIVACY.md
```

**Category:** `Productivity` (or `Workflow & Planning`)

**Language:** English

---

## Privacy tab (REQUIRED — this is what usually blocks approval)

**Single purpose**
```
Capture downloads and detected media from web pages and send them to the D BOX
desktop download manager running on the user's own computer.
```

**Permission justifications** (paste one per permission):

- **downloads** —
  `Intercept file downloads the user starts and hand them to the D BOX app instead of the browser's basic downloader.`
- **storage** —
  `Save the user's settings (capture on/off, D BOX address) and briefly remember media stream URLs found on the current tab.`
- **notifications** —
  `Tell the user when D BOX isn't running so a captured download isn't silently lost.`
- **tabs** —
  `Open the New Download popup window and associate detected media with the correct tab.`
- **contextMenus** —
  `Add "Download link/video with D BOX" and "Grab all links from this page" to the page's right-click menu, and a "Capture downloads" switch, the media found on the page and the Link Grabber to the extension's own toolbar-icon menu.`
- **cookies** —
  `When the user downloads a file or video, pass that site's cookies (e.g. their existing sign-in session) to the local D BOX app so its request matches the browser's and the site serves the file. Cookies are sent only to 127.0.0.1 (the user's own computer) and never to any remote server.`
- **webRequest** —
  `Observe (never block or modify) media requests, and the page address (Referer/Origin) they were made from, so the extension can offer the video/audio streams a page loads and D BOX can request them the same way.`
- **host permission `<all_urls>`** —
  `The user may download from any website, so the content script and media detection must be able to run on all sites.`

**Are you using remote code?** → **No.**

**Data usage** (check the boxes honestly):
- Data handled: *Website content* (media/download URLs) and *Authentication information* (the site's cookies, only for a download the user starts).
- ✔ "I do not sell or transfer user data to third parties, outside of the approved use cases."
- ✔ "I do not use or transfer user data for purposes unrelated to my item's single purpose."
- ✔ "I do not use or transfer user data to determine creditworthiness or for lending purposes."
- **Data is NOT transmitted off the user's device** — everything goes to localhost only. State this in the justification box if offered.

**Privacy policy URL**
```
https://github.com/66lexel999/dbox/blob/main/PRIVACY.md
```

---

## Distribution tab

- **Visibility:** choose **Unlisted** (recommended) — anyone with the link can
  install and it auto-updates, but it won't show up in store searches, which
  keeps review lighter. Switch to **Public** later if you want discoverability.
- **Regions:** All regions.
