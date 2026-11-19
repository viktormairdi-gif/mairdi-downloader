package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

var partialExt = extSet("crdownload part partial download tmp aria2 opdownload !ut bc! ytdl temp")

func hideFile(p string) {
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return
	}
	attrs, err := windows.GetFileAttributes(ptr)
	if err != nil || attrs&windows.FILE_ATTRIBUTE_HIDDEN != 0 {
		return
	}
	_ = windows.SetFileAttributes(ptr, attrs|windows.FILE_ATTRIBUTE_HIDDEN)
}

// Раскладывает файлы, которые лежат прямо в корне папки загрузок
// (их туда положил браузер или другая программа). all=false — только новые.
func (a *App) SortRoot(all bool) (int, int) {
	s := a.getSettings()
	root := s.Root
	ents, err := os.ReadDir(root)
	if err != nil {
		return 0, 0
	}
	moved, dupes := 0, 0
	lib := a.library()
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		low := strings.ToLower(name)
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$") || low == "desktop.ini" || low == "thumbs.db" {
			continue
		}
		if partialExt[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		if time.Since(info.ModTime()) < 15*time.Second {
			continue // ещё пишется
		}
		if !all && info.ModTime().Unix() < s.AutoSortSince {
			continue
		}
		p := filepath.Join(root, name)
		if !canOpenExclusive(p) {
			continue
		}
		hostURL, ref := zoneIdentifier(p)
		src := Source{URL: hostURL, PageURL: ref}
		if s.SkipDupes {
			if ex := lib.findSameContent(p); ex != "" {
				if recycle(p) == nil {
					dupes++
					a.addHistory(name, ex, relTo(root, filepath.Dir(ex)), "Дубликат уже лежащего файла — копия отправлена в Корзину", hostURL, "duplicate")
				}
				continue
			}
		}
		v := a.Classify(p, name, src)
		if v.NewExt != "" {
			name = strings.TrimSuffix(name, filepath.Ext(name)) + v.NewExt
		}
		dst := uniquePath(filepath.Join(root, v.Dir, name))
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := moveFile(p, dst); err != nil {
			continue
		}
		moved++
		lib.add(dst, info.Size())
		a.addHistory(filepath.Base(dst), dst, v.Dir, v.Reason+" (разобрано из папки загрузок)", hostURL, "done")
	}
	return moved, dupes
}

func canOpenExclusive(p string) bool {
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

func (a *App) addHistory(name, path, dir, reason, srcURL, status string) {
	j := &Job{Kind: "file", Name: name, Path: path, Dir: dir, Category: topCategory(dir), Reason: reason,
		Status: status, Added: time.Now(), Finished: time.Now(), OrigURL: srcURL, Domain: hostOf(srcURL), Source: "folder"}
	if status == "duplicate" {
		j.DupOf = path
	}
	if st, err := os.Stat(path); err == nil {
		j.Total, j.Done = st.Size(), st.Size()
	}
	a.mu.Lock()
	j.ID = a.newID()
	a.jobs = append(a.jobs, j)
	a.markDirty()
	a.mu.Unlock()
}

func (a *App) autoSortLoop() {
	a.scanLibrary()
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			return
		case <-t.C:
		}
		if a.getSettings().AutoSortRoot {
			a.SortRoot(false)
		}
	}
}

// ---------- буфер обмена (как в классических менеджерах загрузок) ----------

var reURL = regexp.MustCompile(`^(https?|ftp)://\S+$|^magnet:\?\S+$`)

var clipExts = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range []map[string]bool{extSetup, extDisk, extAndroid, extOther, extArchive, extVideo, extAudio, extBook, extWord, extTable, extSlides, extLib, extWinUpd} {
		for k := range s {
			m[k] = true
		}
	}
	m[".pdf"] = true
	m[".torrent"] = true
	return m
}()

func clipboardWorthy(t string) bool {
	if !reURL.MatchString(t) {
		return false
	}
	if strings.HasPrefix(t, "magnet:") {
		return true
	}
	h := hostOf(t)
	for _, c := range []string{"drive.google.com", "disk.yandex", "yadi.sk", "dropbox.com", "1drv.ms", "cloud.mail.ru"} {
		if strings.Contains(h, c) {
			return true
		}
	}
	p := t
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return clipExts[strings.ToLower(pathExt(p))]
}

func (a *App) clipboardLoop() {
	last := clipboardSeq()
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			return
		case <-t.C:
		}
		seq := clipboardSeq()
		if seq == last {
			continue
		}
		last = seq
		if !a.getSettings().Clipboard {
			continue
		}
		txt := strings.TrimSpace(clipboardText())
		if txt == "" || strings.Contains(txt, "\n") || !clipboardWorthy(txt) {
			continue
		}
		if a.downloadedBefore(txt) != nil {
			continue
		}
		a.mu.Lock()
		a.prompt = uiPrompt{Seq: a.prompt.Seq + 1, URL: txt}
		a.mu.Unlock()
		a.win.show()
	}
}
