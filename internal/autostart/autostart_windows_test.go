//go:build windows

package autostart

import (
	"slices"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// scratch points the backend at a throwaway key, never the real startup list.
func scratch(t *testing.T) runKeys {
	t.Helper()
	base := `Software\DBoxAutostartTest`
	k := runKeys{root: registry.CURRENT_USER, run: base + `\Run`, approved: base + `\StartupApproved\Run`}
	t.Cleanup(func() {
		registry.DeleteKey(registry.CURRENT_USER, k.approved)
		registry.DeleteKey(registry.CURRENT_USER, base+`\StartupApproved`)
		registry.DeleteKey(registry.CURRENT_USER, k.run)
		registry.DeleteKey(registry.CURRENT_USER, base)
	})
	return k
}

func setApproved(t *testing.T, k runKeys, first byte) {
	t.Helper()
	ap, _, err := registry.CreateKey(k.root, k.approved, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	b := make([]byte, 12)
	b[0] = first
	if err := ap.SetBinaryValue(ValueName, b); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	k := scratch(t)
	const exe = `A:\PersonalApps\D BOX\DBox.exe`

	if st, err := k.Status(); err != nil || st.Registered {
		t.Fatalf("fresh: %+v %v", st, err)
	}
	if err := k.Disable(); err != nil {
		t.Fatalf("Disable with nothing registered: %v", err)
	}
	if err := k.Enable(Command(exe)); err != nil {
		t.Fatal(err)
	}
	st, err := k.Status()
	if err != nil || !st.On() || !st.PointsTo(exe) {
		t.Fatalf("after Enable: %+v %v", st, err)
	}
	// The value is a plain REG_SZ command line, as Windows' Run key expects.
	run, _ := registry.OpenKey(k.root, k.run, registry.QUERY_VALUE)
	v, typ, err := run.GetStringValue(ValueName)
	run.Close()
	if err != nil || typ != registry.SZ || v != `"A:\PersonalApps\D BOX\DBox.exe" -autostart` {
		t.Fatalf("value = %q type %d err %v", v, typ, err)
	}

	setApproved(t, k, 0x03) // Task Manager: disabled
	if st, _ := k.Status(); !st.DisabledByUser || st.On() {
		t.Fatalf("Task Manager's disabled mark not seen: %+v", st)
	}
	if st, _ := k.Status(); st.ApprovedByUser {
		t.Fatalf("a disabled mark is not an approval: %+v", st)
	}
	setApproved(t, k, 0x02) // Task Manager: enabled
	if st, _ := k.Status(); st.DisabledByUser || !st.On() || !st.ApprovedByUser {
		t.Fatalf("enabled mark not read as Task Manager's approval: %+v", st)
	}
	// Saved "off" (Task Manager had turned it off) + switched back on there: on.
	off := false
	if on, err := sync(k, &off, exe); err != nil || !on {
		t.Fatalf("re-enabled in Task Manager: on=%v err=%v", on, err)
	}
	if st, _ := k.Status(); !st.On() || !st.PointsTo(exe) {
		t.Fatalf("Task Manager's re-enable was undone: %+v", st)
	}
	setApproved(t, k, 0x07)
	if err := k.Enable(Command(exe)); err != nil { // re-enabling from D BOX clears the mark
		t.Fatal(err)
	}
	if st, _ := k.Status(); !st.On() || st.ApprovedByUser {
		t.Fatalf("Enable left Task Manager's mark: %+v", st)
	}

	if err := k.Disable(); err != nil {
		t.Fatal(err)
	}
	if st, _ := k.Status(); st.Registered {
		t.Fatalf("after Disable: %+v", st)
	}
}

func TestRegistryForeignValueType(t *testing.T) {
	k := scratch(t)
	run, _, err := registry.CreateKey(k.root, k.run, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	run.SetDWordValue(ValueName, 1)
	run.Close()
	st, err := k.Status()
	if err != nil || !st.Registered || st.PointsTo(`C:\DBox.exe`) {
		t.Fatalf("non-string entry: %+v %v", st, err)
	}
	if on, err := sync(k, nil, `C:\DBox.exe`); err != nil || !on {
		t.Fatalf("sync: %v %v", on, err)
	}
	if st, _ := k.Status(); !st.PointsTo(`C:\DBox.exe`) {
		t.Fatalf("not rewritten: %+v", st)
	}
}

// TestCommandLineRoundTrip: Windows splits the sign-in command back into
// exactly the exe, -autostart and the launch's flags.
func TestCommandLineRoundTrip(t *testing.T) {
	exe := `C:\Program Files\D BOX\DBox.exe`
	args := []string{"-listen=127.0.0.1:9090", `-data=C:\Users\Jo Smith\AppData\Local\flowerX\`, `-x=a"b`}
	got, err := windows.DecomposeCommandLine(Command(exe, args...))
	if err != nil {
		t.Fatal(err)
	}
	if want := append([]string{exe, Flag}, args...); !slices.Equal(got, want) {
		t.Fatalf("parsed %q, want %q", got, want)
	}
}
