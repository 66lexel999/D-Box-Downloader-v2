//go:build windows

package autostart

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// Supported reports whether this OS can start D BOX at sign-in.
func Supported() bool { return true }

// runKeys is the Windows registry backend. Its keys are fields so tests can
// point it at a scratch key instead of the user's real startup list.
type runKeys struct {
	root     registry.Key
	run      string // the Run key: value name -> command line
	approved string // Task Manager's on/off marks for the Run key's entries
}

var system backend = runKeys{
	root:     registry.CURRENT_USER,
	run:      `Software\Microsoft\Windows\CurrentVersion\Run`,
	approved: `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`,
}

func (k runKeys) Status() (State, error) {
	var st State
	run, err := registry.OpenKey(k.root, k.run, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	cmd, _, err := run.GetStringValue(ValueName)
	run.Close()
	switch {
	case err == nil:
		st.Registered, st.Command = true, cmd
	case errors.Is(err, registry.ErrNotExist):
		return st, nil
	case errors.Is(err, registry.ErrUnexpectedType):
		st.Registered = true // not a command line D BOX wrote; Enable rewrites it
	default:
		return st, err
	}
	// Task Manager → Startup apps keeps its switch in a 12-byte value per entry:
	// the first byte is odd (3, 7) when the entry is disabled, even (2, 6) when
	// enabled. No value means enabled.
	if ap, err := registry.OpenKey(k.root, k.approved, registry.QUERY_VALUE); err == nil {
		if b, _, err := ap.GetBinaryValue(ValueName); err == nil && len(b) > 0 {
			st.DisabledByUser = b[0]&1 == 1
			st.ApprovedByUser = b[0]&1 == 0
		}
		ap.Close()
	}
	return st, nil
}

func (k runKeys) Enable(command string) error {
	run, _, err := registry.CreateKey(k.root, k.run, registry.SET_VALUE)
	if err != nil {
		return err
	}
	err = run.SetStringValue(ValueName, command)
	run.Close()
	if err != nil {
		return err
	}
	return k.clearApproved() // a leftover "disabled" mark would keep Windows from starting it
}

func (k runKeys) Disable() error {
	run, err := registry.OpenKey(k.root, k.run, registry.SET_VALUE)
	if err == nil {
		err = run.DeleteValue(ValueName)
		run.Close()
	}
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return k.clearApproved()
}

// clearApproved drops Task Manager's mark for the entry (no mark = enabled,
// Windows' default for a new entry).
func (k runKeys) clearApproved() error {
	ap, err := registry.OpenKey(k.root, k.approved, registry.SET_VALUE)
	if err == nil {
		err = ap.DeleteValue(ValueName)
		ap.Close()
	}
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}
