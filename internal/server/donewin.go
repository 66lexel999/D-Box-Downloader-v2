package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Open "Download complete" windows ---------------------------------------------
//
// Several downloads finishing together pop several completion windows. The one
// the user is on — the window focused last — offers "Close all"; the others
// don't. In the default UI each window is its own process, so they keep count
// through the server: every open window heartbeats here (with whether it has
// focus) and learns how many are open, whether it is the active one, and when
// "Close all" (or "Don't show again") asked it to close.

// doneWinTTL forgets a window that stopped heartbeating (closed without
// saying so, or its process died); windows beat every ~300 ms.
const doneWinTTL = 3 * time.Second

// doneWinMax bounds the registry; a key is a window's own random id.
const (
	doneWinMax    = 256
	doneWinKeyMax = 64
)

type doneWindows struct {
	mu   sync.Mutex
	wins map[string]*doneWin
	seq  uint64
	now  func() time.Time // tests move it; nil = time.Now
}

type doneWin struct {
	seen     time.Time // last heartbeat
	focused  time.Time // when it last gained focus; opening counts as gaining it
	hasFocus bool      // focused at its last heartbeat
	seq      uint64    // opening order, breaks ties (the newer window wins)
	closing  bool      // asked to close (Close all / Don't show again)
}

func (d *doneWindows) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// pruneLocked drops windows that stopped heartbeating.
func (d *doneWindows) pruneLocked(now time.Time) {
	for k, w := range d.wins {
		if now.Sub(w.seen) > doneWinTTL {
			delete(d.wins, k)
		}
	}
}

// beat records a heartbeat from window key and reports how many completion
// windows are open, whether key is the active one (the last to gain focus),
// and whether it should close itself. Only gaining focus counts, not keeping
// it: two windows that both claim focus can't take turns being active.
func (d *doneWindows) beat(key string, focused bool) (count int, active, closing bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	d.pruneLocked(now)
	w := d.wins[key]
	if w == nil {
		if d.wins == nil {
			d.wins = map[string]*doneWin{}
		}
		if len(d.wins) >= doneWinMax {
			return 0, false, false
		}
		d.seq++
		w = &doneWin{focused: now, seq: d.seq}
		d.wins[key] = w
	}
	w.seen = now
	if focused && !w.hasFocus {
		w.focused = now
	}
	w.hasFocus = focused
	var top *doneWin
	for _, o := range d.wins {
		if o.closing {
			continue
		}
		count++
		if top == nil || o.focused.After(top.focused) || (o.focused.Equal(top.focused) && o.seq > top.seq) {
			top = o
		}
	}
	return count, w == top, w.closing
}

// gone forgets a window that is closing.
func (d *doneWindows) gone(key string) {
	d.mu.Lock()
	delete(d.wins, key)
	d.mu.Unlock()
}

// closeAll asks every open completion window to close and returns how many.
func (d *doneWindows) closeAll() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked(d.clock())
	n := 0
	for _, w := range d.wins {
		if !w.closing {
			w.closing = true
			n++
		}
	}
	return n
}

func validDoneKey(k string) bool {
	if k == "" || len(k) > doneWinKeyMax {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// handleDoneWindowBeat: POST /api/done-windows/{key} {focused} →
// {count, active, close}.
func (s *Server) handleDoneWindowBeat(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !validDoneKey(key) {
		writeError(w, http.StatusBadRequest, "bad window key")
		return
	}
	var req struct {
		Focused bool `json:"focused"`
	}
	json.NewDecoder(r.Body).Decode(&req) // missing body = not focused
	count, active, closing := s.doneWins.beat(key, req.Focused)
	writeJSON(w, http.StatusOK, map[string]any{"count": count, "active": active, "close": closing})
}

// handleDoneWindowGone: DELETE /api/done-windows/{key} — the window closed.
func (s *Server) handleDoneWindowGone(w http.ResponseWriter, r *http.Request) {
	s.doneWins.gone(r.PathValue("key"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDoneWindowsCloseAll: POST /api/done-windows/close-all — the "Close
// all" button; every open completion window closes on its next heartbeat.
func (s *Server) handleDoneWindowsCloseAll(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"closed": s.doneWins.closeAll()})
}

// handleSetDonePopup: POST /api/settings/done-popup {on} — the completion
// window's "Don't show again" (on=false, which also closes the open ones) and
// its switch in Options.
func (s *Server) handleSetDonePopup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	s.eng.SetDonePopupEnabled(req.On)
	if !req.On {
		s.doneWins.closeAll()
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}
