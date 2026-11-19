package main

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// «Скачать в Google Диск»: закачка идёт как обычно (с раскладкой по папкам), а готовый
// файл сразу уходит на Диск — в «Загрузки\<папка раскладки>» или в выбранную папку Диска.
// После проверки, что Google принял файл целиком, копия на компьютере удаляется
// (если не просили оставить). Каждый файл — отдельная загрузка в разделе «Отправлено на Диск».

// driveAccFor — аккаунт для «в Диск»: названный, из настроек или первый подключённый.
func (a *App) driveAccFor(want string) (string, error) {
	accs := a.gd.accounts()
	if len(accs) == 0 {
		return "", errors.New("Google Диск не подключён — подключите его в окне программы (слева «Google Диск»)")
	}
	pick := func(e string) string {
		for _, x := range accs {
			if e != "" && strings.EqualFold(x.Email, e) {
				return x.Email
			}
		}
		return ""
	}
	if want != "" && want != "1" && want != "default" {
		if e := pick(want); e != "" {
			return e, nil
		}
		return "", errors.New("аккаунт " + want + " не подключён")
	}
	if e := pick(a.getSettings().DriveAcc); e != "" {
		return e, nil
	}
	return accs[0].Email, nil
}

// drivePathFor — путь папок на Диске для закачки: «Загрузки/Прайсы/сайт».
func (a *App) drivePathFor(j *Job, s Settings) string {
	if j.DriveParent != "" {
		return "" // папку выбрали в файловом менеджере Диска — кладём прямо в неё
	}
	gp := s.DriveFolder
	if j.Dir != "" && !filepath.IsAbs(j.Dir) {
		if rel := cleanRel(j.Dir); rel != "" {
			gp = strings.Trim(gp+"/"+strings.ReplaceAll(rel, `\`, "/"), "/")
		}
	}
	return gp
}

type driveForward struct {
	src     *Job
	path    string
	acc     string
	base    string // id папки, от которой строится путь ("" — корень «Моего диска»)
	gpath   string
	show    string
	del     bool
	stopGid string
}

// forwardToDrive вызывается планировщиком раз в секунду: готовые закачки «в Диск»
// превращаются в загрузки, а у уже отправленных обновляется итог.
func (a *App) forwardToDrive() {
	var list []driveForward
	a.mu.Lock()
	s := a.settings
	for _, j := range a.jobs {
		if j.ToDrive == "" || j.Kind == "upload" {
			continue
		}
		switch j.DriveStep {
		case "":
		case "queued", "error":
			a.driveProgress(j)
			continue
		default:
			continue
		}
		ready := j.Status == "done" || j.Status == "duplicate" || j.Kind == "torrent" && j.Status == "seeding"
		if !ready {
			continue
		}
		if j.Path == "" { // плейлист: каждый ролик — своя закачка; дубль ещё не готовой закачки
			j.DriveStep = "skip"
			a.markDirty()
			continue
		}
		f := driveForward{src: j, path: j.Path, acc: j.ToDrive, base: j.DriveParent, gpath: a.drivePathFor(j, s),
			del: !j.DriveKeep && j.Status != "duplicate"}
		f.show = "Мой диск › " + strings.ReplaceAll(f.gpath, "/", " › ")
		if j.DriveParent != "" {
			f.show = firstNonEmpty(j.DriveParentName, "папка на Диске")
		}
		if j.Status == "seeding" { // раздачу останавливаем — файлы уходят на Диск
			if len(j.Parts) > 0 {
				f.stopGid = j.Parts[0].Gid
			}
			j.Status, j.Speed, j.UpSpeed = "done", 0, 0
		}
		j.DriveStep, j.DriveErr = "queued", ""
		a.markDirty()
		list = append(list, f)
	}
	a.mu.Unlock()
	for _, f := range list {
		a.queueTransfer(f)
	}
}

func (a *App) queueTransfer(f driveForward) {
	if f.stopGid != "" && a.aria != nil {
		a.aria.remove(f.stopGid)
		time.Sleep(500 * time.Millisecond) // движок отпускает файлы не мгновенно
	}
	fail := func(msg string) {
		a.mu.Lock()
		f.src.DriveStep, f.src.DriveErr = "error", msg
		a.markDirty()
		a.mu.Unlock()
	}
	st, err := os.Stat(f.path)
	if err != nil {
		fail("файла уже нет на компьютере")
		return
	}
	a.mu.Lock()
	f.src.DriveBase, f.src.DrivePath = f.base, f.gpath
	a.mu.Unlock()
	if !st.IsDir() {
		a.addTransfer(f, f.path, st.Size(), f.gpath, f.show, "")
		go a.saveState()
		return
	}
	// папка торрента — вся структура уходит в одноимённую папку на Диске
	top := strings.Trim(f.gpath+"/"+st.Name(), "/")
	a.mu.Lock()
	f.src.DrivePath = top
	a.mu.Unlock()
	n := 0
	_ = filepath.WalkDir(f.path, func(fp string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || n >= upFileLimit {
			return nil
		}
		name := d.Name()
		if strings.EqualFold(name, "desktop.ini") || strings.EqualFold(name, "thumbs.db") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(f.path, filepath.Dir(fp))
		gp := top
		if rel != "." && rel != "" {
			gp += "/" + strings.ReplaceAll(rel, `\`, "/")
		}
		show := f.show + " › " + st.Name()
		if rel != "." && rel != "" {
			show += " › " + strings.ReplaceAll(rel, `\`, " › ")
		}
		a.addTransfer(f, fp, info.Size(), gp, show, f.path)
		n++
		return nil
	})
	if n == 0 {
		fail("в папке нет файлов")
		return
	}
	go a.saveState()
}

func (a *App) addTransfer(f driveForward, path string, size int64, gpath, show, delRoot string) {
	j := &Job{Kind: "upload", Name: filepath.Base(path), Path: path, Status: "waiting", Added: time.Now(),
		Total: size, GAcc: f.acc, GBase: f.base, GPath: gpath, Dir: show, Source: "transfer",
		Note: "на Google Диск", SrcJob: f.src.ID, DelLocal: f.del, DelRoot: delRoot}
	if gpath == "" {
		j.GParent = firstNonEmpty(f.base, "root")
	}
	a.mu.Lock()
	j.ID = a.newID()
	a.jobs = append(a.jobs, j)
	a.markDirty()
	a.mu.Unlock()
}

// driveProgress — итог по загрузкам закачки (вызывается под a.mu).
func (a *App) driveProgress(src *Job) {
	n, done, errs, gone := 0, 0, 0, 0
	var first *Job
	errMsg := ""
	for _, u := range a.jobs {
		if u.Kind != "upload" || u.SrcJob != src.ID {
			continue
		}
		n++
		if first == nil {
			first = u
		}
		switch u.Status {
		case "done", "duplicate":
			done++
		case "error":
			errs++
			if errMsg == "" {
				errMsg = u.Error
			}
		}
		if u.LocalGone {
			gone++
		}
	}
	step, msg := "queued", ""
	switch {
	case n == 0:
		step = "cancelled" // загрузки убрали из списка
	case done == n:
		step = "done"
	case errs > 0 && done+errs == n:
		step, msg = "error", errMsg
	}
	if step == src.DriveStep && src.DriveDone == done && src.DriveTotal == n && src.DriveErr == msg && (gone == 0 || src.LocalGone) {
		return
	}
	src.DriveStep, src.DriveDone, src.DriveTotal, src.DriveErr = step, done, n, msg
	if step == "done" {
		if n == 1 && first != nil {
			src.DriveFile = first.GFileID
		}
		if gone == n {
			src.LocalGone = true
		}
	}
	a.markDirty()
}

// ---------- папки на Диске по пути ----------

// resolveGPath находит (или создаёт) цепочку папок «Загрузки/Прайсы/сайт» от base.
// Под общей блокировкой — три одновременные загрузки не создадут три одинаковые папки.
func (a *App) resolveGPath(acc, base, gpath string) (string, error) {
	a.gfMu.Lock()
	defer a.gfMu.Unlock()
	if a.gfolders == nil {
		a.gfolders = map[string]string{}
	}
	id := firstNonEmpty(base, "root")
	key := acc + "|" + id
	for _, seg := range strings.Split(gpath, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		key += "/" + seg
		if c, ok := a.gfolders[key]; ok {
			id = c
			continue
		}
		nid, err := a.gd.ensureFolder(acc, id, seg)
		if err != nil {
			return "", err
		}
		a.gfolders[key] = nid
		id = nid
	}
	return id, nil
}

func (a *App) forgetGFolders() {
	a.gfMu.Lock()
	a.gfolders = nil
	a.gfMu.Unlock()
	a.gkMu.Lock()
	a.gkids = nil
	a.gkMu.Unlock()
}

// ---------- после загрузки: убрать копию с компьютера ----------

func (a *App) insideDownloads(p string) bool {
	p = filepath.Clean(p)
	for _, r := range []string{a.root(), incomingDir(a.root(), a.paths.Data)} {
		rel, err := filepath.Rel(filepath.Clean(r), p)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func (a *App) cleanupTransfer(j *Job) {
	a.mu.Lock()
	del, p, root, acc, id := j.DelLocal, j.Path, j.DelRoot, j.GAcc, j.GFileID
	a.mu.Unlock()
	if !del || id == "" || !a.insideDownloads(p) {
		return
	}
	note := func(s string) {
		a.mu.Lock()
		j.DelLocal = false
		j.Reason += s
		a.markDirty()
		a.mu.Unlock()
	}
	st, err := os.Stat(p)
	if err != nil {
		return
	}
	// удаляем только если Google действительно хранит файл того же размера
	var m struct {
		Size string `json:"size"`
	}
	if err := a.gd.call(acc, "GET", "/drive/v3/files/"+url.PathEscape(id),
		url.Values{"fields": {"size"}, "supportsAllDrives": {"true"}}, nil, &m); err != nil || m.Size != strconv.FormatInt(st.Size(), 10) {
		note(" · копию на компьютере оставил: не удалось сверить размер на Диске")
		return
	}
	if err := os.Remove(p); err != nil {
		note(" · копию на компьютере удалить не удалось: " + err.Error())
		return
	}
	// опустевшие папки торрента тоже убираем
	if root != "" {
		root = filepath.Clean(root)
		for d := filepath.Dir(p); strings.HasPrefix(strings.ToLower(d), strings.ToLower(root)) && a.insideDownloads(d); d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
	}
	a.mu.Lock()
	j.DelLocal, j.LocalGone = false, true
	j.Reason += " · копия на компьютере удалена"
	a.markDirty()
	a.mu.Unlock()
}

// ---------- действия с закачкой «в Диск» ----------

// openOnDrive открывает на Диске файл закачки (или её папку, если файлов несколько).
func (a *App) openOnDrive(j *Job) error {
	a.mu.Lock()
	acc, file, base, gp, step := j.ToDrive, j.DriveFile, j.DriveBase, j.DrivePath, j.DriveStep
	a.mu.Unlock()
	if acc == "" || step != "done" && file == "" {
		return errors.New("на Google Диск ещё не загружено")
	}
	if file != "" {
		return a.driveOpen(acc, file)
	}
	id, err := a.resolveGPath(acc, base, gp)
	if err != nil {
		return err
	}
	return a.driveOpen(acc, id)
}

// sendToDrive — «Загрузить на Google Диск» для уже скачанного файла (копия остаётся).
func (a *App) sendToDrive(j *Job) error {
	acc, err := a.driveAccFor("")
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !j.finished() && j.Status != "duplicate" {
		return errors.New("файл ещё не скачан")
	}
	if j.DriveStep == "queued" {
		return nil
	}
	if j.DriveStep == "done" && j.LocalGone {
		return errors.New("файл уже на Google Диске")
	}
	j.ToDrive, j.DriveKeep, j.DriveStep, j.DriveErr = acc, true, "", ""
	j.DriveParent, j.DriveParentName = "", ""
	a.markDirty()
	return nil
}

// driveRetry — повторить загрузки, которые закончились ошибкой.
func (a *App) driveRetry(j *Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.jobs {
		if u.Kind == "upload" && u.SrcJob == j.ID && u.Status == "error" {
			u.Status, u.Error, u.Retries = "waiting", "", 0
		}
	}
	if j.DriveStep == "error" || j.DriveStep == "cancelled" {
		if j.DriveStep == "cancelled" {
			j.DriveStep = "" // загрузки убрали — ставим заново
		} else {
			j.DriveStep = "queued"
		}
		j.DriveErr = ""
	}
	a.markDirty()
}
