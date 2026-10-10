//go:build !windows

package gui

import (
	"errors"
	"fmt"
	"os"
)

var errUnsupported = errors.New("native GUI is only available on Windows; run with -gui=false")

func EnableDPIAwareness()                                     {}
func AllowForeground()                                        {}
func RunMain(serverURL, dataDir string, onClose func()) error { return errUnsupported }
func RunDialog(serverURL, query string) error                 { return errUnsupported }
func RunDone(serverURL, id string) error                      { return errUnsupported }
func RunDetail(serverURL, id string) error                    { return errUnsupported }

func SetTrayStartupToggle(get func() bool, set func(on bool) error) {}
func TrayActive() bool                                              { return false }

// MessageBox prints the message where there is no native box.
func MessageBox(title, text string) { fmt.Fprintln(os.Stderr, title+": "+text) }
