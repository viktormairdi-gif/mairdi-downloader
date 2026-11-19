package main

// Мастер установки расширения Chrome/Edge: программа помнит, когда расширение
// последний раз выходило на связь и какой оно версии, и открывает страницу расширений.
// Поставить расширение сама программа не может: Chrome на Windows запрещает
// установку не из Web Store, остаётся «Загрузить распакованное» руками.

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"
)

type extSeenInfo struct {
	Version string    `json:"version"` // "" — старое расширение (до 1.2.0), версию не присылало
	Seen    time.Time `json:"seen"`
	saved   time.Time
}

var (
	extMu     sync.Mutex
	extInfo   *extSeenInfo
	extLoaded string // папка данных, из которой прочитан файл
)

func (a *App) extFile() string { return filepath.Join(a.paths.Data, "extension.json") }

func (a *App) extLoad() *extSeenInfo {
	if extInfo == nil || extLoaded != a.paths.Data {
		extInfo, extLoaded = &extSeenInfo{}, a.paths.Data
		if b, err := os.ReadFile(a.extFile()); err == nil {
			_ = json.Unmarshal(b, extInfo)
		}
	}
	return extInfo
}

// Любое сообщение от расширения — отметка «на связи».
func (a *App) extTouch(ver string) {
	extMu.Lock()
	defer extMu.Unlock()
	e := a.extLoad()
	changed := e.Version != ver || e.Seen.IsZero()
	e.Version, e.Seen = ver, time.Now()
	if changed || time.Since(e.saved) > 10*time.Minute {
		e.saved = e.Seen
		b, _ := json.Marshal(e)
		_ = os.WriteFile(a.extFile(), b, 0o644)
	}
}

// Версия расширения в папке рядом с программой (то, что надо поставить).
func (a *App) extWantVersion() string {
	b, err := os.ReadFile(filepath.Join(a.extensionDir(), "manifest.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Version
}

// "1.10.0" > "1.9.3"
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func (a *App) extStatus() map[string]any {
	extMu.Lock()
	e := *a.extLoad()
	extMu.Unlock()
	want := a.extWantVersion()
	state := "none" // не выходило на связь ни разу
	if !e.Seen.IsZero() {
		state = "ok"
		if e.Version == "" || (want != "" && versionLess(e.Version, want)) {
			state = "old"
		}
	}
	var seen int64
	if !e.Seen.IsZero() {
		seen = e.Seen.UnixMilli()
	}
	return map[string]any{
		"ok": true, "state": state, "version": e.Version, "want": want, "seen": seen,
		"path": a.extensionDir(), "chrome": chromeExe() != "", "edge": edgeExe() != "",
	}
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func chromeExe() string {
	return firstExisting(
		getRegString(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`, ""),
		getRegString(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`, ""),
		os.Getenv("ProgramFiles")+`\Google\Chrome\Application\chrome.exe`,
		os.Getenv("ProgramFiles(x86)")+`\Google\Chrome\Application\chrome.exe`,
		os.Getenv("LOCALAPPDATA")+`\Google\Chrome\Application\chrome.exe`,
	)
}

func edgeExe() string {
	return firstExisting(
		getRegString(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`, ""),
		os.Getenv("ProgramFiles(x86)")+`\Microsoft\Edge\Application\msedge.exe`,
		os.Getenv("ProgramFiles")+`\Microsoft\Edge\Application\msedge.exe`,
	)
}

// Открыть страницу расширений в нужном браузере (новая вкладка в уже открытом окне).
func openExtensionsPage(browser string) error {
	exe, page := chromeExe(), "chrome://extensions/"
	if browser == "edge" {
		exe, page = edgeExe(), "edge://extensions/"
	}
	if exe == "" {
		return errors.New("браузер не найден")
	}
	return exec.Command(exe, page).Start()
}
