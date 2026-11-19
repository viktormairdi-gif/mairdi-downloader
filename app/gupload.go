package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Загрузка файлов с компьютера на Google Диск: resumable-сессии Google.
// Файл уходит кусками по 16 МБ; после обрыва и даже после перезапуска программы
// спрашиваем у Google, сколько он уже принял, и продолжаем с этого места.

const upChunk = 16 << 20    // кратно 256 КБ, как требует Google
const maxActiveUploads = 6  // всего одновременно
const maxBigUploads = 3     // из них крупных (кусками по 16 МБ) — канал всё равно один
const smallUpload = 5 << 20 // до 5 МБ — одним запросом, без resumable-сессии

var errSessionGone = errors.New("сессия загрузки устарела")
var errFolderGone = fatalUpErr{"папка на Диске не найдена — возможно, её удалили"}

type upRun struct{ cancel context.CancelFunc }

// ---------- постановка в очередь ----------

type UploadReq struct {
	Acc        string   `json:"acc"`
	Parent     string   `json:"parent"`     // id папки на Диске ("" — корень «Моего диска»)
	ParentName string   `json:"parentName"` // для показа: «Мой диск › Прайсы»
	Paths      []string `json:"paths"`
	Spooled    bool     `json:"-"` // файлы — временные копии (перетащили в окно), удалить после загрузки
	SpoolRoot  string   `json:"-"`
}

type UploadRes struct {
	OK      bool     `json:"ok"`
	Added   int      `json:"added"`
	Folders int      `json:"folders"`
	Errors  []string `json:"errors,omitempty"`
}

const upFileLimit = 50000

// QueueUpload ставит файлы и папки (со всей структурой) в очередь загрузки на Диск.
func (a *App) QueueUpload(r UploadReq) UploadRes {
	res := UploadRes{}
	if r.Acc == "" {
		return UploadRes{Errors: []string{"не выбран аккаунт Google"}}
	}
	parent := r.Parent
	if parent == "" {
		parent = "root"
	}
	if r.ParentName == "" {
		r.ParentName = "Мой диск"
	}
	folders := map[string]string{} // локальная папка → id на Диске
	count := 0
	for _, p := range r.Paths {
		p = filepath.Clean(strings.Trim(p, `"`))
		st, err := os.Stat(p)
		if err != nil {
			res.Errors = append(res.Errors, "нет такого файла: "+p)
			continue
		}
		if !st.IsDir() {
			a.addUpload(r, p, st.Size(), parent, r.ParentName)
			res.Added++
			continue
		}
		top, err := a.gd.ensureFolder(r.Acc, parent, st.Name())
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		res.Folders++
		folders[p] = top
		_ = filepath.WalkDir(p, func(fp string, d os.DirEntry, err error) error {
			if err != nil || fp == p {
				return nil
			}
			if count >= upFileLimit {
				return filepath.SkipAll
			}
			name := d.Name()
			if strings.EqualFold(name, "desktop.ini") || strings.EqualFold(name, "thumbs.db") || strings.HasPrefix(name, "~$") {
				return nil
			}
			pid := folders[filepath.Dir(fp)]
			if pid == "" {
				return nil
			}
			if d.IsDir() {
				id, err := a.gd.ensureFolder(r.Acc, pid, name)
				if err != nil {
					res.Errors = append(res.Errors, err.Error())
					return filepath.SkipDir
				}
				folders[fp] = id
				res.Folders++
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(filepath.Dir(p), filepath.Dir(fp))
			show := r.ParentName + " › " + strings.ReplaceAll(rel, `\`, " › ")
			a.addUpload(r, fp, info.Size(), pid, show)
			res.Added++
			count++
			return nil
		})
		if count >= upFileLimit {
			res.Errors = append(res.Errors, "больше "+strconv.Itoa(upFileLimit)+" файлов — остальные пропущены")
		}
	}
	res.OK = res.Added > 0 || res.Folders > 0
	if res.OK {
		a.sawWork = true
		go a.saveState()
	}
	return res
}

func (a *App) addUpload(r UploadReq, path string, size int64, parent, parentName string) {
	j := &Job{Kind: "upload", Name: filepath.Base(path), Path: path, Status: "waiting", Added: time.Now(),
		Total: size, GAcc: r.Acc, GParent: parent, Dir: parentName, Source: "upload", Note: "на Google Диск"}
	if r.Spooled {
		j.DeleteAfter = true
		j.SpoolRoot = r.SpoolRoot
	}
	a.mu.Lock()
	j.ID = a.newID()
	a.jobs = append(a.jobs, j)
	a.markDirty()
	a.mu.Unlock()
}

// ensureFolder — папка с таким именем в parent: берём существующую или создаём.
func (g *GDrive) ensureFolder(acc, parent, name string) (string, error) {
	q := url.Values{"q": {"name = " + gQuote(name) + " and " + gQuote(parent) + " in parents and mimeType = " + gQuote(gFolderMime) + " and trashed = false"},
		"fields": {"files(id)"}, "pageSize": {"1"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
	var out struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	if err := g.call(acc, "GET", "/drive/v3/files", q, nil, &out); err != nil {
		return "", err
	}
	if len(out.Files) > 0 {
		return out.Files[0].ID, nil
	}
	var f struct {
		ID string `json:"id"`
	}
	err := g.call(acc, "POST", "/drive/v3/files", url.Values{"fields": {"id"}, "supportsAllDrives": {"true"}},
		map[string]any{"name": name, "mimeType": gFolderMime, "parents": []string{parent}}, &f)
	return f.ID, err
}

// ---------- очередь ----------

// scheduleUploads вызывается раз в секунду: не больше трёх загрузок одновременно.
func (a *App) scheduleUploads() {
	a.mu.Lock()
	active, big := 0, 0
	for _, j := range a.jobs {
		if j.Kind == "upload" && j.up != nil {
			active++
			if j.Total > smallUpload {
				big++
			}
		}
	}
	now := time.Now()
	var start []*Job
	for _, j := range a.jobs {
		if active >= maxActiveUploads {
			break
		}
		if j.Kind != "upload" || j.up != nil {
			continue
		}
		if j.Status == "retrying" && now.After(j.RetryAt) {
			j.Status = "waiting"
		}
		isBig := j.Total > smallUpload
		if j.Status == "waiting" && (!isBig || big < maxBigUploads) {
			active++
			if isBig {
				big++
			}
			ctx, cancel := context.WithCancel(context.Background())
			j.up = &upRun{cancel: cancel}
			j.Status, j.stopReq = "active", ""
			start = append(start, j)
			go a.runUpload(ctx, j)
		}
	}
	a.mu.Unlock()
	_ = start
}

func (a *App) runUpload(ctx context.Context, j *Job) {
	err := a.uploadFile(ctx, j) // файл уже закрыт — временную копию можно удалять
	a.mu.Lock()
	fin := j.Status == "done" || j.Status == "duplicate"
	a.mu.Unlock()
	if fin {
		a.cleanupSpool(j)
		a.cleanupTransfer(j)
	}
	gone := err != nil && err.Error() == errFolderGone.Error()
	if gone {
		a.forgetGFolders()
	}
	a.mu.Lock()
	j.up = nil
	j.Speed, j.ETA = 0, 0
	stop := j.stopReq
	switch {
	case stop == "pause":
		j.Status = "paused"
	case stop != "":
		// убрана из списка
	case err == nil:
	case errors.Is(err, context.Canceled):
		j.Status = "waiting"
	case gone && j.GPath != "" && j.Retries < 2:
		// папку «Загрузки/…» удалили на Диске — создадим заново
		j.Retries++
		j.GParent, j.UploadURL, j.Status = "", "", "waiting"
	case isFatalUpload(err):
		j.Status, j.Error = "error", err.Error()
	default:
		if j.Retries < 50 {
			j.Retries++
			wait := retryWait(j.Retries)
			j.Status, j.RetryAt = "retrying", time.Now().Add(wait)
			j.Error = err.Error() + " — повтор через " + strconv.Itoa(int(wait.Seconds())) + " с"
		} else {
			j.Status, j.Error = "error", err.Error()
		}
	}
	a.markDirty()
	a.mu.Unlock()
}

type fatalUpErr struct{ msg string }

func (e fatalUpErr) Error() string { return e.msg }

func isFatalUpload(err error) bool {
	var f fatalUpErr
	return errors.As(err, &f)
}

// ---------- сама загрузка ----------

func (a *App) uploadFile(ctx context.Context, j *Job) error {
	a.mu.Lock()
	path, acc, parent, sess, total, force := j.Path, j.GAcc, j.GParent, j.UploadURL, j.Total, j.Force
	base, gpath := j.GBase, j.GPath
	a.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return fatalUpErr{"файл недоступен: " + err.Error()}
	}
	if parent == "" { // закачка «в Диск»: папки «Загрузки/<категория>» находим или создаём
		if parent, err = a.resolveGPath(acc, base, gpath); err != nil {
			f.Close()
			return err
		}
		a.mu.Lock()
		j.GParent = parent
		a.markDirty()
		a.mu.Unlock()
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size != total && sess != "" {
		sess = "" // файл изменился — начинаем заново
	}
	a.mu.Lock()
	j.Total = size
	a.mu.Unlock()

	// Такой файл уже лежит в этой папке Диска (имя, размер и MD5) — не загружаем повторно.
	if sess == "" && !force {
		if id, err := a.sameOnDrive(acc, parent, filepath.Base(path), size, f); err == nil && id != "" {
			a.mu.Lock()
			j.Status, j.GFileID, j.Done = "duplicate", id, size
			j.Reason, j.Finished, j.Error = "Такой файл уже есть в этой папке Диска — повторно не загружаю", time.Now(), ""
			a.mu.Unlock()
			return nil
		}
	}
	if size == 0 {
		id, err := a.gd.createEmpty(acc, parent, filepath.Base(path))
		if err != nil {
			return err
		}
		a.rememberOnDrive(acc, parent, filepath.Base(path), 0, "d41d8cd98f00b204e9800998ecf8427e", id)
		a.finishUpload(j, id)
		return nil
	}
	// Мелкий файл (фото, документ) — одним запросом, без отдельной сессии: на тысячах файлов это вдвое быстрее.
	if size <= smallUpload {
		data := make([]byte, size)
		if _, err := f.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
			return fatalUpErr{"не читается файл: " + err.Error()}
		}
		a.mu.Lock()
		j.Done, j.UploadURL = 0, ""
		a.mu.Unlock()
		start := time.Now()
		id, err := a.gd.uploadSimple(ctx, acc, parent, filepath.Base(path), data)
		if err != nil {
			return err
		}
		sum := md5.Sum(data)
		a.rememberOnDrive(acc, parent, filepath.Base(path), size, hex.EncodeToString(sum[:]), id)
		a.mu.Lock()
		if el := time.Since(start).Seconds(); el > 0 {
			j.Speed = int64(float64(size) / el)
		}
		a.sawWork = true
		a.mu.Unlock()
		a.finishUpload(j, id)
		return nil
	}

	var off int64
	if sess != "" {
		off, err = a.gd.uploadStatus(ctx, sess, size)
		if errors.Is(err, errSessionGone) {
			sess = ""
		} else if err != nil {
			return err
		} else if off < 0 { // Google уже собрал файл
			off = 0
			sess = ""
		}
	}
	if sess == "" {
		sess, err = a.gd.startSession(acc, parent, filepath.Base(path), size)
		if err != nil {
			return err
		}
		off = 0
		a.mu.Lock()
		j.UploadURL = sess
		a.markDirty()
		a.mu.Unlock()
	}

	// скорость и оставшееся время — раз в секунду
	var sent = off
	stopTick := make(chan struct{})
	defer close(stopTick)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		last := off
		for {
			select {
			case <-stopTick:
				return
			case <-t.C:
			}
			a.mu.Lock()
			cur := sent
			sp := cur - last
			last = cur
			j.Speed = (j.Speed + sp) / 2
			if j.Speed == 0 {
				j.Speed = sp
			}
			j.Done = cur
			j.ETA = 0
			if j.Speed > 0 {
				j.ETA = (size - cur) / j.Speed
			}
			if sp > 0 {
				a.sawWork = true
				if strings.Contains(j.Error, "повтор через") {
					j.Error = ""
				}
			}
			a.mu.Unlock()
		}
	}()

	buf := make([]byte, upChunk)
	fails := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n := int64(upChunk)
		if size-off < n {
			n = size - off
		}
		if _, err := f.ReadAt(buf[:n], off); err != nil && !errors.Is(err, io.EOF) {
			return fatalUpErr{"не читается файл: " + err.Error()}
		}
		next, fileID, err := a.gd.putChunk(ctx, acc, sess, buf[:n], off, size)
		if err == nil {
			fails = 0
			if fileID != "" {
				a.mu.Lock()
				sent = size
				a.mu.Unlock()
				a.rememberOnDrive(acc, parent, filepath.Base(path), size, "", fileID)
				a.finishUpload(j, fileID)
				return nil
			}
			a.mu.Lock()
			off, sent = next, next
			a.mu.Unlock()
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errSessionGone) {
			a.mu.Lock()
			j.UploadURL = ""
			a.mu.Unlock()
			return err
		}
		if isFatalUpload(err) {
			return err
		}
		// обрыв посреди куска — узнаём у Google, сколько дошло, и продолжаем
		fails++
		if fails > 6 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(fails) * 2 * time.Second):
		}
		if got, serr := a.gd.uploadStatus(ctx, sess, size); serr == nil && got >= 0 {
			a.mu.Lock()
			off, sent = got, got
			a.mu.Unlock()
		}
	}
}

func (a *App) finishUpload(j *Job, fileID string) {
	a.mu.Lock()
	j.Status, j.GFileID, j.UploadURL = "done", fileID, ""
	j.Done, j.Speed, j.ETA, j.Error = j.Total, 0, 0, ""
	j.Finished = time.Now()
	j.Reason = "Загружено на Google Диск"
	a.markDirty()
	a.mu.Unlock()
}

// Временные копии перетащенных файлов удаляем после загрузки.
func (a *App) cleanupSpool(j *Job) {
	a.mu.Lock()
	del, p, root := j.DeleteAfter, j.Path, j.SpoolRoot
	if del {
		j.DeleteAfter = false
		j.Reason += " (временная копия удалена)"
	}
	a.mu.Unlock()
	if !del || !strings.HasPrefix(filepath.Clean(p), filepath.Clean(a.spoolDir())) {
		return
	}
	_ = os.Remove(p)
	// пустые папки копии тоже убираем
	for d := filepath.Dir(p); root != "" && strings.HasPrefix(d, root) && d != filepath.Clean(a.spoolDir()); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			break
		}
	}
}

func (a *App) spoolDir() string { return filepath.Join(a.paths.Data, "upload-spool") }

// При запуске убираем временные копии, которые больше не ждут загрузки.
func (a *App) cleanSpoolOrphans() {
	ents, err := os.ReadDir(a.spoolDir())
	if err != nil {
		return
	}
	a.mu.Lock()
	used := map[string]bool{}
	for _, j := range a.jobs {
		if j.Kind == "upload" && j.DeleteAfter && j.SpoolRoot != "" {
			used[filepath.Clean(j.SpoolRoot)] = true
		}
	}
	a.mu.Unlock()
	for _, e := range ents {
		p := filepath.Join(a.spoolDir(), e.Name())
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour && !used[p] {
			_ = os.RemoveAll(p)
		}
	}
}

// ---------- протокол Google ----------

func (g *GDrive) startSession(acc, parent, name string, size int64) (string, error) {
	tok, err := g.accessToken(acc)
	if err != nil {
		return "", err
	}
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	meta, _ := json.Marshal(map[string]any{"name": name, "parents": []string{parent}})
	req, _ := http.NewRequest("POST", gAPI+"/upload/drive/v3/files?uploadType=resumable&supportsAllDrives=true&fields=id", bytes.NewReader(meta))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Type", ct)
	req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
	resp, err := g.client.Do(req)
	if err != nil {
		return "", friendlyNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", upHTTPError(resp)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("Google не выдал адрес загрузки")
	}
	return loc, nil
}

// uploadStatus — сколько байт Google уже принял. -1 — файл уже собран.
func (g *GDrive) uploadStatus(ctx context.Context, sess string, size int64) (int64, error) {
	req, _ := http.NewRequestWithContext(ctx, "PUT", sess, nil)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
	req.ContentLength = 0
	resp, err := upClient.Do(req)
	if err != nil {
		return 0, friendlyNetErr(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 308:
		return rangeEnd(resp.Header.Get("Range")), nil
	case resp.StatusCode == 200 || resp.StatusCode == 201:
		return -1, nil
	case resp.StatusCode == 404 || resp.StatusCode == 410:
		return 0, errSessionGone
	}
	return 0, upHTTPError(resp)
}

// «bytes=0-12345» → 12346 (следующий байт); нет заголовка — ничего не принято.
func rangeEnd(h string) int64 {
	if i := strings.LastIndex(h, "-"); i >= 0 {
		if n, err := strconv.ParseInt(h[i+1:], 10, 64); err == nil {
			return n + 1
		}
	}
	return 0
}

var upClient = &http.Client{Transport: &http.Transport{
	Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 120 * time.Second, TLSHandshakeTimeout: 20 * time.Second,
	ForceAttemptHTTP2: true, IdleConnTimeout: 60 * time.Second,
}}

// putChunk отправляет кусок; возвращает, с какого байта продолжать, или id готового файла.
func (g *GDrive) putChunk(ctx context.Context, acc, sess string, data []byte, off, size int64) (int64, string, error) {
	req, _ := http.NewRequestWithContext(ctx, "PUT", sess, bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+int64(len(data))-1, size))
	resp, err := upClient.Do(req)
	if err != nil {
		return 0, "", friendlyNetErr(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 308:
		next := rangeEnd(resp.Header.Get("Range"))
		return next, "", nil
	case resp.StatusCode == 200 || resp.StatusCode == 201:
		var f struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&f)
		if f.ID == "" {
			return 0, "", errors.New("Google не сообщил, что файл сохранён")
		}
		return size, f.ID, nil
	case resp.StatusCode == 404 || resp.StatusCode == 410:
		return 0, "", errSessionGone
	}
	return 0, "", upHTTPError(resp)
}

func upHTTPError(resp *http.Response) error {
	var e gAPIError
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
	reason := ""
	if len(e.Error.Errors) > 0 {
		reason = e.Error.Errors[0].Reason
	}
	switch {
	case reason == "storageQuotaExceeded" || reason == "quotaExceeded":
		return fatalUpErr{"на Google Диске не хватает места"}
	case reason == "teamDriveFileLimitExceeded":
		return fatalUpErr{"в общем диске достигнут предел числа файлов"}
	case resp.StatusCode == 404 && reason == "notFound":
		return errFolderGone
	case resp.StatusCode == 403 && !strings.Contains(reason, "ateLimit"):
		return fatalUpErr{"нет прав загружать в эту папку"}
	}
	return humanGError(resp.StatusCode, e)
}

func (g *GDrive) createEmpty(acc, parent, name string) (string, error) {
	var f struct {
		ID string `json:"id"`
	}
	err := g.call(acc, "POST", "/drive/v3/files", url.Values{"fields": {"id"}, "supportsAllDrives": {"true"}},
		map[string]any{"name": name, "parents": []string{parent}}, &f)
	return f.ID, err
}

// uploadSimple — мелкий файл одним multipart-запросом.
func (g *GDrive) uploadSimple(ctx context.Context, acc, parent, name string, data []byte) (string, error) {
	tok, err := g.accessToken(acc)
	if err != nil {
		return "", err
	}
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	meta, _ := json.Marshal(map[string]any{"name": name, "parents": []string{parent}})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", "application/json; charset=UTF-8")
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write(meta)
	h = textproto.MIMEHeader{}
	h.Set("Content-Type", ct)
	pw, _ = mw.CreatePart(h)
	_, _ = pw.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequestWithContext(ctx, "POST", gAPI+"/upload/drive/v3/files?uploadType=multipart&supportsAllDrives=true&fields=id", &body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	resp, err := upClient.Do(req)
	if err != nil {
		return "", friendlyNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", upHTTPError(resp)
	}
	var f struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&f)
	if f.ID == "" {
		return "", errors.New("Google не сообщил, что файл сохранён")
	}
	return f.ID, nil
}

// ---------- что уже лежит в папке Диска ----------
// Содержимое папки читаем один раз (до 1000 файлов за запрос) и дальше сверяем у себя:
// при загрузке тысяч фото это убирает запрос к Google на каждый файл.

const gKidsTTL = 10 * time.Minute

type gKid struct{ id, md5 string }

type gKids struct {
	at    time.Time
	files map[string][]gKidSized // имя → файлы с этим именем
}

type gKidSized struct {
	size int64
	gKid
}

func (a *App) folderKids(acc, parent string) (*gKids, error) {
	a.gkMu.Lock()
	defer a.gkMu.Unlock()
	if a.gkids == nil {
		a.gkids = map[string]*gKids{}
	}
	key := acc + "|" + parent
	if k := a.gkids[key]; k != nil && time.Since(k.at) < gKidsTTL {
		return k, nil
	}
	k := &gKids{at: time.Now(), files: map[string][]gKidSized{}}
	tok := ""
	for page := 0; page < 200; page++ {
		q := url.Values{"q": {gQuote(parent) + " in parents and trashed = false and mimeType != " + gQuote(gFolderMime)},
			"fields": {"nextPageToken,files(id,name,size,md5Checksum)"}, "pageSize": {"1000"},
			"supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
		if tok != "" {
			q.Set("pageToken", tok)
		}
		var out struct {
			Next  string `json:"nextPageToken"`
			Files []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Size string `json:"size"`
				MD5  string `json:"md5Checksum"`
			} `json:"files"`
		}
		if err := a.gd.call(acc, "GET", "/drive/v3/files", q, nil, &out); err != nil {
			return nil, err
		}
		for _, f := range out.Files {
			n, _ := strconv.ParseInt(f.Size, 10, 64)
			k.files[f.Name] = append(k.files[f.Name], gKidSized{n, gKid{f.ID, f.MD5}})
		}
		if tok = out.Next; tok == "" {
			break
		}
	}
	a.gkids[key] = k
	return k, nil
}

// rememberOnDrive — только что загруженный файл тоже учитываем (без нового запроса к Google).
func (a *App) rememberOnDrive(acc, parent, name string, size int64, sum, id string) {
	a.gkMu.Lock()
	defer a.gkMu.Unlock()
	if k := a.gkids[acc+"|"+parent]; k != nil {
		k.files[name] = append(k.files[name], gKidSized{size, gKid{id, sum}})
	}
}

// sameOnDrive — есть ли в папке файл с тем же именем, размером и MD5.
func (a *App) sameOnDrive(acc, parent, name string, size int64, f *os.File) (string, error) {
	k, err := a.folderKids(acc, parent)
	if err != nil {
		return a.gd.findSame(acc, parent, name, size, f) // не прочиталась папка — спросим по одному файлу
	}
	a.gkMu.Lock()
	cands := append([]gKidSized(nil), k.files[name]...)
	a.gkMu.Unlock()
	local := ""
	for _, c := range cands {
		if c.size != size {
			continue
		}
		if c.md5 == "" { // загружали сами в этом запуске, сумму Google ещё не спрашивали
			return a.gd.findSame(acc, parent, name, size, f)
		}
		if local == "" {
			h := md5.New()
			if _, err := io.Copy(h, io.NewSectionReader(f, 0, size)); err != nil {
				return "", err
			}
			local = hex.EncodeToString(h.Sum(nil))
		}
		if strings.EqualFold(c.md5, local) {
			return c.id, nil
		}
	}
	return "", nil
}

// findSame ищет в папке файл с тем же именем, размером и MD5.
func (g *GDrive) findSame(acc, parent, name string, size int64, f *os.File) (string, error) {
	q := url.Values{"q": {"name = " + gQuote(name) + " and " + gQuote(parent) + " in parents and trashed = false"},
		"fields": {"files(id,size,md5Checksum)"}, "pageSize": {"20"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
	var out struct {
		Files []struct {
			ID  string `json:"id"`
			Sz  string `json:"size"`
			MD5 string `json:"md5Checksum"`
		} `json:"files"`
	}
	if err := g.call(acc, "GET", "/drive/v3/files", q, nil, &out); err != nil {
		return "", err
	}
	local := ""
	for _, x := range out.Files {
		if x.Sz != strconv.FormatInt(size, 10) || x.MD5 == "" {
			continue
		}
		if local == "" {
			h := md5.New()
			if _, err := io.Copy(h, io.NewSectionReader(f, 0, size)); err != nil {
				return "", err
			}
			local = hex.EncodeToString(h.Sum(nil))
		}
		if strings.EqualFold(x.MD5, local) {
			return x.ID, nil
		}
	}
	return "", nil
}

// ---------- действия над загрузкой ----------

func (a *App) uploadAction(j *Job, action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch action {
	case "pause":
		if j.up != nil {
			j.stopReq = "pause"
			j.up.cancel()
		} else if j.Status == "waiting" || j.Status == "retrying" {
			j.Status = "paused"
		}
	case "resume", "retry":
		if j.Status == "paused" || j.Status == "error" || j.Status == "retrying" {
			j.Status, j.Error, j.stopReq = "waiting", "", ""
			if action == "retry" {
				j.Retries = 0
			}
		}
	case "remove", "removeWithFile": // локальный файл не трогаем никогда, кроме временной копии
		if j.up != nil {
			j.stopReq = "remove"
			j.up.cancel()
		}
		out := a.jobs[:0]
		for _, x := range a.jobs {
			if x != j {
				out = append(out, x)
			}
		}
		a.jobs = out
		a.markDirty()
		if j.DeleteAfter {
			p := j.Path
			go func() {
				time.Sleep(time.Second)
				if strings.HasPrefix(filepath.Clean(p), filepath.Clean(a.spoolDir())) {
					_ = os.Remove(p)
				}
			}()
		}
	case "open":
		if j.DeleteAfter || j.LocalGone {
			return errors.New("копии на компьютере нет — откройте файл на Диске")
		}
		go shellOpen(j.Path)
	case "folder":
		if !j.DeleteAfter && !j.LocalGone {
			go revealInExplorer(j.Path)
		}
	case "drive":
		if j.GFileID == "" {
			return errors.New("файл ещё не загружен")
		}
		acc, id := j.GAcc, j.GFileID
		go func() { _ = a.driveOpen(acc, id) }()
	default:
		return errors.New("неизвестное действие")
	}
	return nil
}

// ---------- перетаскивание в окно: файл приходит потоком и сохраняется во временную копию ----------

func (a *App) handleSpool(w http.ResponseWriter, r *http.Request) {
	if !a.hostOK(r) || r.Header.Get("X-Token") != a.token || r.Method != "PUT" {
		http.Error(w, "forbidden", 403)
		return
	}
	batch := cleanRel(r.URL.Query().Get("batch"))
	rel := cleanRel(r.URL.Query().Get("rel"))
	if batch == "" || rel == "" {
		http.Error(w, "bad request", 400)
		return
	}
	dst := filepath.Join(a.spoolDir(), batch, rel)
	if r.URL.Query().Get("dir") == "1" { // пустая папка
		_ = os.MkdirAll(dst, 0o755)
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	f, err := os.Create(dst)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_, err = io.Copy(f, r.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// spoolCommit — все файлы партии сохранены: ставим их (и верхние папки) в очередь.
func (a *App) spoolCommit(acc, parent, parentName, batch string, tops []string) UploadRes {
	batch = cleanRel(batch)
	root := filepath.Join(a.spoolDir(), batch)
	var paths []string
	for _, t := range tops {
		p := filepath.Join(root, cleanRel(t))
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	return a.QueueUpload(UploadReq{Acc: acc, Parent: parent, ParentName: parentName, Paths: paths, Spooled: true, SpoolRoot: root})
}
