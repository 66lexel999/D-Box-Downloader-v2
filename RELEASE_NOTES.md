D BOX 1.2.1

• D BOX now starts when Windows starts. It opens quietly in the tray when you
  sign in, so downloads from your browser work right away. Don't want that?
  Untick "Start D BOX when Windows starts" in Options, or "Start with
  Windows" in the tray icon's right-click menu. Turning it off in Task
  Manager's Startup apps is respected too.
• Opening D BOX while it's already running (for example from the desktop
  icon, while it waits in the tray) now brings up the open window instead of
  silently doing nothing.
• The tray icon comes back if Windows Explorer restarts.
• Browser extension 3.4.0: one click on its toolbar icon opens the D BOX
  window. What the old popup had moved to the icon's right-click menu:
  Capture downloads on/off, "Media on this page…" (the list of captured
  videos/streams, one click to send) and "Grab all links from this page".

D BOX 1.2.0

• New look: a new default dark theme, "Slate" — darker and richer than the old
  gray, but not black (the old gray is still there as "Classic gray"). Every
  window (main list, New Download, status, completion) uses it, and the theme
  you pick in View → Appearance… now carries over to the popup windows too.
• New icons everywhere: toolbar, categories, file types, status, menus and
  dialogs (the D BOX logo is unchanged). Optional: "Use Windows file icons"
  in Appearance brings back the real Windows icons for finished files.
• Link Grabber (like JDownloader): paste a web page into the address bar and
  D BOX lists everything it can download there — videos (with quality
  choice), streams, audio, images, archives, documents, programs — with real
  names and sizes. Tick what you want and press Download. Direct file links
  still open the usual New Download window. "Grab Links" accepts a page
  address or any text full of links.
• Pages with several videos: D BOX now lists all of them and picks the main
  one instead of silently grabbing the first (often an ad or intro).
• Fixed files saved as web pages: hosts that answer with a landing page
  (MediaFire, VLC/SourceForge-style mirrors, ModDB, Google Drive's "can't scan
  for viruses" page, Dropbox preview links) now lead to the real file;
  sites that need a Referer (e.g. dev-c.com) work; expired/blocked links show a
  clear message instead of saving an HTML page under the file's name.
• Correct names and sizes: wrong extensions are fixed from the file's content
  (an .mp3 offered as .mp4 is saved as .mp3), titles lose site names and
  random ids, titles with "/" keep their full text, stream sizes use the real
  average bitrate (and include the audio track), and video-site sizes come from
  the actual formats.
• Streams: never picks an audio-only variant over video, prefers AAC audio,
  rejects junk/HTML segments, decrypts AES-256 streams, saves audio-only streams
  as .m4a/.mp3, and live recordings start at the live edge.
• Downloads can no longer be silently corrupted by servers that ignore ranges
  or redirect to a different file; Retry-After is honoured.
• Browser extension 3.3.0: catches players that build their stream in
  JavaScript (hls.js, Shaka, dash.js, Video.js, JW Player…), ignores ads, UI
  sounds and stream segments, sends better titles, and adds right-click
  "Grab all links from this page with D BOX".
