//go:build !windows

package autostart

import "errors"

// Supported reports whether this OS can start D BOX at sign-in.
func Supported() bool { return false }

var errUnsupported = errors.New("starting with the computer is only available on Windows")

type none struct{}

var system backend = none{}

func (none) Status() (State, error) { return State{}, nil }
func (none) Enable(string) error    { return errUnsupported }
func (none) Disable() error         { return nil }
