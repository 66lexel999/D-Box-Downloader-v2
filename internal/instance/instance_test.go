package instance

import (
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusivePerDataDir(t *testing.T) {
	dir := t.TempDir()
	release, first := Acquire(dir)
	if !first {
		t.Fatal("first Acquire reported another instance")
	}
	// The same folder spelled differently is the same instance.
	if r, ok := Acquire(filepath.Join(dir, ".") + string(filepath.Separator)); ok {
		r()
		t.Fatal("second Acquire of the same data folder succeeded")
	}
	// Another data folder is another, independent instance.
	r2, ok := Acquire(t.TempDir())
	if !ok {
		t.Fatal("a different data folder was blocked")
	}
	r2()

	release()
	r3, ok := Acquire(dir)
	if !ok {
		t.Fatal("Acquire after release failed")
	}
	r3()
}

func TestNameIgnoresCase(t *testing.T) {
	if name(`C:\Users\Me\AppData\Local\flowerX`) != name(`c:\users\me\appdata\local\FLOWERX`) {
		t.Fatal("data folder case changed the lock name")
	}
}
