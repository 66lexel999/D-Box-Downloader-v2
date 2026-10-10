package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeReg is an in-memory sign-in entry.
type fakeReg struct {
	st                State
	enables, disables int
	err               error
}

func (f *fakeReg) Status() (State, error) { return f.st, f.err }
func (f *fakeReg) Enable(command string) error {
	f.enables++
	f.st = State{Registered: true, Command: command}
	return nil
}
func (f *fakeReg) Disable() error {
	f.disables++
	f.st = State{}
	return nil
}

func ptr(b bool) *bool { return &b }

func TestSync(t *testing.T) {
	const exe = `A:\PersonalApps\D-Box\release\DBox.exe`
	other := State{Registered: true, Command: Command(`C:\Old\DBox.exe`)}
	ours := State{Registered: true, Command: Command(exe)}
	cases := []struct {
		name           string
		pref           *bool
		st             State
		want           bool
		enab, disab    int
		wantRegistered bool
	}{
		{"first run turns it on", nil, State{}, true, 1, 0, true},
		{"on and already registered: no write", ptr(true), ours, true, 0, 0, true},
		{"path compare ignores case", ptr(true), State{Registered: true, Command: Command(`a:\personalapps\d-box\release\dbox.exe`)}, true, 0, 0, true},
		{"on, entry starts another copy: repoint", ptr(true), other, true, 1, 0, true},
		{"on, entry was deleted: re-add", ptr(true), State{}, true, 1, 0, true},
		{"off removes the entry", ptr(false), ours, false, 0, 1, false},
		{"off and nothing registered: no write", ptr(false), State{}, false, 0, 0, false},
		{"switched off in Task Manager wins", ptr(true), State{Registered: true, Command: Command(exe), DisabledByUser: true}, false, 0, 0, true},
		{"first run respects Task Manager too", nil, State{Registered: true, Command: Command(exe), DisabledByUser: true}, false, 0, 0, true},
		// Off because Task Manager turned it off, then switched back on there: on again, entry kept.
		{"switched back on in Task Manager wins", ptr(false), State{Registered: true, Command: Command(exe), ApprovedByUser: true}, true, 0, 0, true},
		{"switched back on in Task Manager: repoint a moved exe", ptr(false), State{Registered: true, Command: Command(`C:\Old\DBox.exe`), ApprovedByUser: true}, true, 1, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeReg{st: c.st}
			got, err := sync(f, c.pref, exe)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want || f.enables != c.enab || f.disables != c.disab || f.st.Registered != c.wantRegistered {
				t.Fatalf("got on=%v enables=%d disables=%d registered=%v; want on=%v enables=%d disables=%d registered=%v",
					got, f.enables, f.disables, f.st.Registered, c.want, c.enab, c.disab, c.wantRegistered)
			}
			if got && !f.st.PointsTo(exe) {
				t.Fatalf("entry = %q, want it to start %s", f.st.Command, exe)
			}
		})
	}
}

func TestSyncWithFlags(t *testing.T) {
	const exe = `C:\Apps\DBox.exe`
	args := []string{"-listen=127.0.0.1:9090", `-data=D:\My Data\dbox`}
	f := &fakeReg{st: State{Registered: true, Command: Command(exe)}} // registered without the flags
	on, err := sync(f, nil, exe, args...)
	if err != nil || !on || f.enables != 1 {
		t.Fatalf("on=%v err=%v enables=%d; want the entry rewritten with the flags", on, err, f.enables)
	}
	if want := `"C:\Apps\DBox.exe" -autostart -listen=127.0.0.1:9090 "-data=D:\My Data\dbox"`; f.st.Command != want {
		t.Fatalf("command = %s, want %s", f.st.Command, want)
	}
	if on, _ := sync(f, nil, exe, args...); !on || f.enables != 1 {
		t.Fatalf("same flags again rewrote the entry (enables=%d)", f.enables)
	}
	if f.st.PointsTo(exe) {
		t.Fatal("an entry with flags must not match a launch without them")
	}
}

func TestSyncStatusErrorKeepsChoice(t *testing.T) {
	f := &fakeReg{err: errors.New("access denied")}
	on, err := sync(f, ptr(true), `C:\DBox.exe`)
	if err == nil || !on || f.enables+f.disables != 0 {
		t.Fatalf("on=%v err=%v writes=%d; want the saved choice kept, the error reported and nothing written", on, err, f.enables+f.disables)
	}
}

func TestCommandAndFlag(t *testing.T) {
	if got := Command(`C:\Program Files\D BOX\DBox.exe`); got != `"C:\Program Files\D BOX\DBox.exe" -autostart` {
		t.Fatalf("Command = %q", got)
	}
	for in, want := range map[string]string{
		`-data=C:\x`:    `-data=C:\x`,
		`-data=C:\a b`:  `"-data=C:\a b"`,
		`-data=C:\a b\`: `"-data=C:\a b\\"`, // trailing backslash must not eat the closing quote
		`say "hi"`:      `"say \"hi\""`,
		`a\"b c`:        `"a\\\"b c"`,
		``:              `""`,
	} {
		if got := quoteArg(in); got != want {
			t.Errorf("quoteArg(%q) = %s, want %s", in, got, want)
		}
	}
	for arg, want := range map[string]bool{
		"-autostart": true, "--autostart": true, "-autostart=true": true, "--autostart=false": true,
		"autostart": false, "---autostart": false, "-autostarts": false, "-listen": false, "-": false, "": false,
	} {
		if got := IsFlag(arg); got != want {
			t.Errorf("IsFlag(%q) = %v, want %v", arg, got, want)
		}
	}
}

func TestManageable(t *testing.T) {
	tmp := os.TempDir()
	if Manageable(filepath.Join(tmp, "go-build123", "b001", "exe", "myidm.exe")) {
		t.Error("a `go run` build in the temp folder must not be registered")
	}
	if Manageable("") {
		t.Error("empty exe path")
	}
	home := filepath.Join(filepath.Dir(tmp), "not-temp-"+filepath.Base(tmp), "DBox.exe")
	if !Manageable(home) {
		t.Errorf("%s should be manageable", home)
	}
}
