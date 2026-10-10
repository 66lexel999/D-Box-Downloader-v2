//go:build windows

// Package wailsui hosts MyIDM's window with Wails v2. Instead of embedding a
// separate frontend, it serves the existing HTTP handler (the IDM-skinned HTML
// UI + JSON API) straight through Wails' AssetServer — so the whole proven web
// UI is reused unchanged, and because it renders in WebView2 there is no native
// ListView to flicker (the problem that plagued the lxn/walk front end).
package wailsui

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"myidm/internal/gui"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// the live window context, captured at startup so Activate can reach the runtime.
var (
	ctxMu      sync.Mutex
	winCtx     context.Context
	reallyQuit bool // set by Quit() so OnBeforeClose lets the window actually close
	// pendingShow: Activate was called before the window existed (a second launch
	// or the browser extension right after sign-in); OnStartup shows it then.
	pendingShow bool
)

// themeBG is each UI theme's window-chrome colour (--bg in web/ui/theme.css), so
// the native window paints the page's colour before the first frame; light is the
// only theme with a light title bar.
var themeBG = map[string]options.RGBA{
	"slate":    {R: 0x26, G: 0x2b, B: 0x32, A: 255},
	"midnight": {R: 0x1a, G: 0x1d, B: 0x23, A: 255},
	"ocean":    {R: 0x1d, G: 0x2c, B: 0x35, A: 255},
	"forest":   {R: 0x23, G: 0x2b, B: 0x24, A: 255},
	"classic":  {R: 0x3c, G: 0x40, B: 0x43, A: 255},
	"light":    {R: 0xee, G: 0xf0, B: 0xf3, A: 255},
}

var curTheme = "slate" // guarded by ctxMu

// SetTheme sets the window background + title bar for a UI theme id. Before Run
// it picks the startup colours; afterwards it repaints the live window (the
// server calls it when the user picks another theme).
func SetTheme(id string) {
	if _, ok := themeBG[id]; !ok {
		id = "slate"
	}
	ctxMu.Lock()
	curTheme = id
	ctx := winCtx
	ctxMu.Unlock()
	if ctx == nil {
		return
	}
	c := themeBG[id]
	wruntime.WindowSetBackgroundColour(ctx, c.R, c.G, c.B, c.A)
	if id == "light" {
		wruntime.WindowSetLightTheme(ctx)
	} else {
		wruntime.WindowSetDarkTheme(ctx)
	}
}

// Run opens the window and blocks on the Wails event loop until it closes.
// handler serves both the UI (GET /) and the JSON API (/api/*). onClose runs on
// shutdown (engine + server teardown). startHidden (the Windows sign-in launch)
// keeps the window hidden — D BOX waits in the tray until it's opened.
func Run(handler http.Handler, log *slog.Logger, onClose func(), startHidden bool) error {
	ctxMu.Lock()
	theme := curTheme
	ctxMu.Unlock()
	bg := themeBG[theme]
	winTheme := windows.Dark
	if theme == "light" {
		winTheme = windows.Light
	}
	return wails.Run(&options.App{
		Title:     "D BOX — Download Manager",
		Width:     763, // default (your current size ÷ 1.39 DPI); overridden by the remembered size
		Height:    343,
		MinWidth:  500,
		MinHeight: 280,
		// Assets nil + Handler set => every request (GET UI + POST/DELETE API)
		// is served by our existing mux.
		AssetServer:      &assetserver.Options{Handler: handler},
		BackgroundColour: &bg, // the saved theme's --bg (web/ui/theme.css), avoids a flash of the wrong colour
		StartHidden:      startHidden,
		OnStartup: func(ctx context.Context) {
			ctxMu.Lock()
			winCtx = ctx
			show := pendingShow
			pendingShow = false
			ctxMu.Unlock()
			if w, h, ok := gui.LoadWinSize("main"); ok {
				wruntime.WindowSetSize(ctx, w, h) // restore the user's last window size
			}
			wruntime.WindowCenter(ctx) // center at the restored (or default) size
			// IDM-style tray: closing the window then hides to the notification area
			// instead of quitting (see OnBeforeClose). Left-click / "Open D BOX"
			// restores it; "Exit" really quits.
			ready := make(chan bool, 1)
			go gui.RunTray("D BOX — Download Manager", Activate, Quit, ready)
			if ok := <-ready; !ok && startHidden {
				// Started hidden but there's no tray icon to open it from: show it
				// minimized on the taskbar so D BOX is still reachable.
				log.Warn("tray icon unavailable at startup; showing the window minimized")
				wruntime.WindowShow(ctx)
				wruntime.WindowMinimise(ctx)
			}
			if show {
				go Activate() // asked for while the window was still being created
			}
		},
		// Wails doesn't set a window icon, so the title bar shows the generic
		// program icon — put our own there once the window/DOM exists.
		OnDomReady: func(_ context.Context) {
			go applyWindowIcon()
		},
		// Close (the X) hides to tray and keeps downloading in the background,
		// rather than quitting — only a real Exit (tray menu / File → Exit) closes.
		OnBeforeClose: func(ctx context.Context) bool {
			if w, h := wruntime.WindowGetSize(ctx); w > 0 && h > 0 {
				gui.SaveWinSize("main", w, h) // remember size on every close (hide-to-tray and real exit)
			}
			ctxMu.Lock()
			rq := reallyQuit
			ctxMu.Unlock()
			if rq {
				return false // allow the close
			}
			if gui.TrayActive() {
				wruntime.WindowHide(ctx) // hide to tray (taskbar button gone, like IDM)
			} else {
				wruntime.WindowMinimise(ctx) // no tray: keep running, restorable from taskbar
			}
			return true // prevent the close
		},
		OnShutdown: func(_ context.Context) {
			gui.RemoveTray()
			if onClose != nil {
				onClose()
			}
		},
		Windows: &windows.Options{
			Theme:                winTheme, // title bar / frame follow the theme (SetTheme updates it live)
			WebviewIsTransparent: false,
		},
	})
}

// Quit requests a real application exit (the tray's "Exit", or File → Exit in the
// UI via POST /api/quit). It removes the tray icon and tells Wails to close;
// OnBeforeClose then lets the window through because reallyQuit is set.
func Quit() {
	ctxMu.Lock()
	reallyQuit = true
	ctx := winCtx
	ctxMu.Unlock()
	gui.RemoveTray()
	if ctx != nil {
		wruntime.Quit(ctx)
	}
}

// Quitting reports that a real exit is under way (the window is going away).
func Quitting() bool {
	ctxMu.Lock()
	defer ctxMu.Unlock()
	return reallyQuit
}

// Activate brings the window to the foreground. Called when the browser
// extension captures a download (POST /api/prompt) so the in-page New Download
// dialog surfaces over the browser instead of only flashing in the taskbar.
// Syncs Wails' own state, then uses the Win32 AttachThreadInput trick to win the
// foreground from a background thread (Wails' WindowShow alone loses the race).
func Activate() {
	ctxMu.Lock()
	ctx := winCtx
	if ctx == nil {
		// The window is still being created (D BOX just started): show it as
		// soon as it exists instead of dropping the request.
		pendingShow = true
		ctxMu.Unlock()
		return
	}
	ctxMu.Unlock()
	wruntime.WindowUnminimise(ctx)
	wruntime.WindowShow(ctx)
	wruntime.WindowCenter(ctx) // re-center every time it reappears (e.g. from the tray)
	// WindowShow is queued to the UI thread: wait (briefly) until the window is
	// really up before raising it, or a window that started hidden is missed.
	for i := 0; i < 40 && ourWindow() == 0; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	forceForeground()
	if !iconApplied.Load() && ourWindow() != 0 {
		applyWindowIcon() // a window that started hidden had nothing to put it on
	}
}
