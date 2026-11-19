package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{
		{"1.1.0", "1.2.0", true}, {"1.2.0", "1.2.0", false}, {"1.10.0", "1.9.3", false},
		{"1.2", "1.2.1", true}, {"2.0.0", "1.99.99", false},
	} {
		if got := versionLess(c.a, c.b); got != c.less {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestExtStatus(t *testing.T) {
	dir := t.TempDir()
	extDir := filepath.Join(dir, "extension")
	_ = os.MkdirAll(extDir, 0o755)
	_ = os.WriteFile(filepath.Join(extDir, "manifest.json"), []byte(`{"version":"1.2.0"}`), 0o644)
	data := filepath.Join(dir, "data")
	_ = os.MkdirAll(data, 0o755)
	a := &App{paths: Paths{Exe: filepath.Join(dir, "bin", "x.exe"), Data: data}}

	if st := a.extStatus(); st["state"] != "none" || st["want"] != "1.2.0" {
		t.Fatalf("без связи: %v", st)
	}
	a.extTouch("") // старое расширение версию не присылает
	if st := a.extStatus(); st["state"] != "old" {
		t.Fatalf("старое: %v", st)
	}
	a.extTouch("1.2.0")
	if st := a.extStatus(); st["state"] != "ok" || st["version"] != "1.2.0" {
		t.Fatalf("текущее: %v", st)
	}
	// Отметка переживает перезапуск программы.
	extInfo = nil
	if st := a.extStatus(); st["state"] != "ok" {
		t.Fatalf("после перезапуска: %v", st)
	}
}
