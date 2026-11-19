package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Library — что уже лежит в папке загрузок: защита от повторного скачивания.
type Library struct {
	mu     sync.Mutex
	bySize map[int64][]string
	hashes map[string]string // path|size|mtime → sha256
}

func newLibrary() *Library {
	return &Library{bySize: map[int64][]string{}, hashes: map[string]string{}}
}

func (l *Library) add(p string, size int64) {
	if size <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.bySize[size] {
		if strings.EqualFold(x, p) {
			return
		}
	}
	l.bySize[size] = append(l.bySize[size], p)
}

func (l *Library) candidates(size int64, except string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out, keep []string
	for _, p := range l.bySize[size] {
		st, err := os.Stat(p)
		if err != nil || st.Size() != size {
			continue // файл удалили или изменили
		}
		keep = append(keep, p)
		if !strings.EqualFold(p, except) {
			out = append(out, p)
		}
	}
	l.bySize[size] = keep
	return out
}

func (a *App) scanLibrary() {
	root := a.root()
	inc := incomingDir(root, a.paths.Data)
	lib := newLibrary()
	for _, c := range Categories {
		_ = filepath.WalkDir(filepath.Join(root, c), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if strings.EqualFold(p, inc) {
					return filepath.SkipDir
				}
				return nil
			}
			if info, err := d.Info(); err == nil {
				lib.add(p, info.Size())
			}
			return nil
		})
	}
	a.mu.Lock()
	a.lib = lib
	a.mu.Unlock()
}

func (a *App) library() *Library {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lib == nil {
		a.lib = newLibrary()
	}
	return a.lib
}

var reCopySuffix = regexp.MustCompile(`(?i)( \(\d+\)|\(\d+\)|_\d+| - копия| - copy)$`)

func normName(n string) string {
	ext := strings.ToLower(filepath.Ext(n))
	b := strings.TrimSuffix(strings.ToLower(n), ext)
	b = reCopySuffix.ReplaceAllString(b, "")
	return strings.TrimSpace(b) + ext
}

// Файл с тем же именем и размером уже есть — скачивать незачем.
func (l *Library) findByNameSize(name string, size int64) string {
	if size <= 0 || name == "" {
		return ""
	}
	n := normName(name)
	for _, p := range l.candidates(size, "") {
		if normName(filepath.Base(p)) == n {
			return p
		}
	}
	return ""
}

// Точно такой же файл по содержимому (SHA-256).
func (l *Library) findSameContent(p string) string {
	st, err := os.Stat(p)
	if err != nil || st.Size() == 0 {
		return ""
	}
	c := l.candidates(st.Size(), p)
	if len(c) == 0 {
		return ""
	}
	h := l.hash(p)
	if h == "" {
		return ""
	}
	for _, x := range c {
		if l.hash(x) == h {
			return x
		}
	}
	return ""
}

func (l *Library) hash(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	key := strings.ToLower(p) + "|" + st.ModTime().Format(time.RFC3339Nano)
	l.mu.Lock()
	if h, ok := l.hashes[key]; ok {
		l.mu.Unlock()
		return h
	}
	l.mu.Unlock()
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	hs := sha256.New()
	if _, err := io.Copy(hs, f); err != nil {
		return ""
	}
	h := hex.EncodeToString(hs.Sum(nil))
	l.mu.Lock()
	l.hashes[key] = h
	l.mu.Unlock()
	return h
}

// Ключ ссылки без мусора: у YouTube — id ролика, у остальных — адрес без меток utm_*.
func urlKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	h = strings.TrimPrefix(h, "m.")
	if h == "youtu.be" {
		return "yt:" + strings.Trim(u.Path, "/")
	}
	if h == "youtube.com" {
		if v := u.Query().Get("v"); v != "" {
			return "yt:" + v
		}
		if strings.HasPrefix(u.Path, "/shorts/") {
			return "yt:" + strings.TrimPrefix(u.Path, "/shorts/")
		}
	}
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || lk == "fbclid" || lk == "gclid" || lk == "si" || lk == "feature" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return h + u.EscapedPath() + "?" + u.RawQuery
}

// Ссылка уже скачана раньше и файл на месте?
func (a *App) downloadedBefore(raw string) *Job {
	k := urlKey(raw)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.jobs {
		if j.OrigURL == "" || urlKey(j.OrigURL) != k {
			continue
		}
		switch j.Status {
		case "done", "seeding":
			if j.LocalGone && j.DriveStep == "done" { // скачано и перенесено на Google Диск
				return j
			}
			if j.Path != "" {
				if _, err := os.Stat(j.Path); err == nil {
					return j
				}
			}
		case "active", "waiting", "paused", "retrying", "resolving", "metadata", "merging", "moving":
			return j
		}
	}
	return nil
}

// В корзину (с возможностью восстановить), а не насовсем.
func recycle(p string) error {
	type shFileOp struct {
		Hwnd                 uintptr
		Func                 uint32
		From                 *uint16
		To                   *uint16
		Flags                uint16
		AnyOperationsAborted int32
		NameMappings         uintptr
		ProgressTitle        *uint16
	}
	from, err := windows.UTF16FromString(p)
	if err != nil {
		return err
	}
	from = append(from, 0) // двойной ноль в конце
	op := shFileOp{Func: 3 /*FO_DELETE*/, From: &from[0],
		Flags: 0x40 | 0x10 | 0x400 | 0x4 /*ALLOWUNDO|NOCONFIRMATION|NOERRORUI|SILENT*/}
	r, _, _ := shell32.NewProc("SHFileOperationW").Call(uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		return os.Remove(p)
	}
	return nil
}
