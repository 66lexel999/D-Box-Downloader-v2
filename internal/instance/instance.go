// Package instance keeps D BOX to one running copy per user and data folder.
// A second launch — the desktop icon, the Start menu — can then hand over to
// the copy already running (for example the one that started hidden in the
// tray at sign-in) instead of starting a second download engine on the same
// tasks and files.
package instance

import (
	"fmt"
	"hash/fnv"
	"path/filepath"
	"strings"
)

// name turns a data folder into a stable lock name: the same folder, however
// it's spelled (case, trailing slash, relative), gives the same name.
func name(dataDir string) string {
	p, err := filepath.Abs(dataDir)
	if err != nil {
		p = dataDir
	}
	h := fnv.New64a()
	h.Write([]byte(strings.ToLower(filepath.Clean(p))))
	return fmt.Sprintf("DBox-%016x", h.Sum64())
}

// Acquire claims the data folder for this process. first is false when another
// running D BOX already holds it. release gives it up early; it's also freed
// when the process exits, however it exits.
func Acquire(dataDir string) (release func(), first bool) { return acquire(name(dataDir)) }
