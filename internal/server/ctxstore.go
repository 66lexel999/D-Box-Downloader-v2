package server

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// ctxStore keeps the browser context (Referer + headers such as cookies) of a
// captured download between the extension's POST /api/prompt and the New
// Download dialog's submit, keyed by an unguessable token. Entries expire so
// cookies don't linger in memory.
type ctxStore struct {
	mu sync.Mutex
	m  map[string]ctxEntry
}

type ctxEntry struct {
	referer string
	headers map[string]string
	at      time.Time
}

const ctxTTL = 3 * time.Hour // a dialog can sit open (or be scheduled) for a while

func (c *ctxStore) put(referer string, headers map[string]string) string {
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]ctxEntry{}
	}
	now := time.Now()
	for k, e := range c.m {
		if now.Sub(e.at) > ctxTTL {
			delete(c.m, k)
		}
	}
	c.m[tok] = ctxEntry{referer: referer, headers: headers, at: now}
	return tok
}

func (c *ctxStore) get(tok string) (ctxEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[tok]
	if !ok || time.Since(e.at) > ctxTTL {
		return ctxEntry{}, false
	}
	return e, true
}
