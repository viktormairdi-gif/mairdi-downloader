package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Поддельный Google для загрузок: resumable-сессии, папки, поиск по имени.
type fakeUp struct {
	mu       sync.Mutex
	url      string
	n        int
	sessions map[string]*fakeSess
	files    map[string]fakeFile // id → файл
	dropOnce atomic.Bool         // оборвать соединение посреди второго куска
	bytesIn  atomic.Int64
	simple   atomic.Int64 // загрузок одним запросом
	lists    atomic.Int64 // запросов списка файлов
}

type fakeSess struct {
	name, parent string
	size         int64
	data         []byte
}

type fakeFile struct {
	name, parent, mime string
	data               []byte
}

func (f *fakeUp) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "AT1", "expires_in": 3600})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/upload/drive/v3/files" && r.Method == "POST" && r.URL.Query().Get("uploadType") == "multipart":
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		var meta struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		p1, err := mr.NextPart()
		if err != nil {
			w.WriteHeader(400)
			return
		}
		_ = json.NewDecoder(p1).Decode(&meta)
		p2, err := mr.NextPart()
		if err != nil {
			w.WriteHeader(400)
			return
		}
		data, _ := io.ReadAll(p2)
		f.bytesIn.Add(int64(len(data)))
		f.simple.Add(1)
		f.n++
		id := "f" + strconv.Itoa(f.n)
		f.files[id] = fakeFile{name: meta.Name, parent: meta.Parents[0], data: data}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	case r.URL.Path == "/upload/drive/v3/files" && r.Method == "POST":
		var meta struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&meta)
		size, _ := strconv.ParseInt(r.Header.Get("X-Upload-Content-Length"), 10, 64)
		f.n++
		id := "s" + strconv.Itoa(f.n)
		f.sessions[id] = &fakeSess{name: meta.Name, parent: meta.Parents[0], size: size}
		w.Header().Set("Location", f.url+"/sess/"+id)
		w.WriteHeader(200)
	case strings.HasPrefix(r.URL.Path, "/sess/"):
		s := f.sessions[strings.TrimPrefix(r.URL.Path, "/sess/")]
		if s == nil {
			w.WriteHeader(404)
			return
		}
		cr := r.Header.Get("Content-Range")
		if strings.HasPrefix(cr, "bytes */") {
			if int64(len(s.data)) == s.size {
				f.finish(w, s)
				return
			}
			if len(s.data) > 0 {
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(s.data)-1))
			}
			w.WriteHeader(308)
			return
		}
		var from, to, total int64
		fmt.Sscanf(cr, "bytes %d-%d/%d", &from, &to, &total)
		body, _ := io.ReadAll(r.Body)
		f.bytesIn.Add(int64(len(body)))
		if from != int64(len(s.data)) {
			w.WriteHeader(400)
			return
		}
		if from > 0 && f.dropOnce.CompareAndSwap(true, false) {
			// приняли только половину куска и оборвали соединение
			s.data = append(s.data, body[:len(body)/2]...)
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				c.Close()
			}
			return
		}
		s.data = append(s.data, body...)
		if int64(len(s.data)) == s.size {
			f.finish(w, s)
			return
		}
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(s.data)-1))
		w.WriteHeader(308)
	case r.URL.Path == "/drive/v3/files" && r.Method == "POST":
		var meta struct {
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Parents  []string `json:"parents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&meta)
		f.n++
		id := "f" + strconv.Itoa(f.n)
		f.files[id] = fakeFile{name: meta.Name, parent: meta.Parents[0], mime: meta.MimeType}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	case r.URL.Path == "/drive/v3/files" && r.Method == "GET":
		f.lists.Add(1)
		q := r.URL.Query().Get("q")
		var list []map[string]any
		for id, x := range f.files {
			if strings.Contains(q, "name = ") && !strings.Contains(q, "name = "+gQuote(x.name)) || !strings.Contains(q, gQuote(x.parent)+" in parents") {
				continue
			}
			if strings.Contains(q, "mimeType = ") != (x.mime == gFolderMime) || strings.Contains(q, "mimeType != ") && x.mime == gFolderMime {
				continue
			}
			sum := md5.Sum(x.data)
			list = append(list, map[string]any{"id": id, "size": strconv.Itoa(len(x.data)), "md5Checksum": hex.EncodeToString(sum[:])})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": list})
	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && r.Method == "GET":
		x, ok := f.files[strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"size": strconv.Itoa(len(x.data))})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeUp) finish(w http.ResponseWriter, s *fakeSess) {
	f.n++
	id := "f" + strconv.Itoa(f.n)
	f.files[id] = fakeFile{name: s.name, parent: s.parent, data: s.data}
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
}

func (f *fakeUp) find(name string) *fakeFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.files {
		if x.name == name && x.mime != gFolderMime {
			return &x
		}
	}
	return nil
}

func uploadApp(t *testing.T) (*App, *fakeUp) {
	fu := &fakeUp{sessions: map[string]*fakeSess{}, files: map[string]fakeFile{}}
	srv := httptest.NewServer(http.HandlerFunc(fu.handler))
	t.Cleanup(srv.Close)
	fu.url = srv.URL
	oldAPI, oldTok := gAPI, gTokenURL
	gAPI, gTokenURL = srv.URL, srv.URL+"/token"
	t.Cleanup(func() { gAPI, gTokenURL = oldAPI, oldTok })
	a := nativeApp(t)
	a.gd = newGDrive(t.TempDir())
	enc, _ := dpapiProtect([]byte("RT"))
	a.gd.cfg = GConfig{ClientID: "x.apps.googleusercontent.com", ClientSecret: "SEC", Accounts: []*GAccount{{Email: "me@gmail.com", RefreshEnc: enc}}}
	return a, fu
}

func runUploads(t *testing.T, a *App) []*Job {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		a.scheduleUploads()
		a.mu.Lock()
		busy := false
		for _, j := range a.jobs {
			if j.Kind == "upload" && (j.Status == "waiting" || j.Status == "active" || j.up != nil) {
				busy = true
			}
			if j.Status == "retrying" {
				j.RetryAt = time.Now() // в тесте не ждём паузу перед повтором
				busy = true
			}
		}
		js := append([]*Job(nil), a.jobs...)
		a.mu.Unlock()
		if !busy {
			return js
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("загрузки не завершились")
	return nil
}

func randFile(t *testing.T, p string, n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUploadResumableWithDrop(t *testing.T) {
	a, fu := uploadApp(t)
	p := filepath.Join(t.TempDir(), "Прайс.xlsx")
	data := randFile(t, p, 40<<20) // три куска по 16 МБ
	fu.dropOnce.Store(true)
	res := a.QueueUpload(UploadReq{Acc: "me@gmail.com", Parent: "P1", ParentName: "Мой диск › Прайсы", Paths: []string{p}})
	if res.Added != 1 {
		t.Fatalf("не встало в очередь: %+v", res)
	}
	js := runUploads(t, a)
	if js[0].Status != "done" || js[0].GFileID == "" {
		t.Fatalf("статус %s: %s", js[0].Status, js[0].Error)
	}
	got := fu.find("Прайс.xlsx")
	if got == nil || !bytes.Equal(got.data, data) || got.parent != "P1" {
		t.Fatal("на Диске файл собрался с ошибкой или не в той папке")
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("исходный файл на компьютере должен остаться")
	}
}

func TestUploadResumeAfterRestart(t *testing.T) {
	a, fu := uploadApp(t)
	p := filepath.Join(t.TempDir(), "big.bin")
	data := randFile(t, p, 40<<20)
	// «прошлый запуск»: сессия есть, Google принял первые 16 МБ
	sess, err := a.gd.startSession("me@gmail.com", "root", "big.bin", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	fu.mu.Lock()
	for _, s := range fu.sessions {
		s.data = append([]byte(nil), data[:16<<20]...)
	}
	fu.mu.Unlock()
	j := &Job{ID: "u1", Kind: "upload", Name: "big.bin", Path: p, Status: "waiting", GAcc: "me@gmail.com", GParent: "root", UploadURL: sess, Total: int64(len(data))}
	a.jobs = []*Job{j}
	runUploads(t, a)
	if j.Status != "done" {
		t.Fatalf("статус %s: %s", j.Status, j.Error)
	}
	if got := fu.find("big.bin"); got == nil || !bytes.Equal(got.data, data) {
		t.Fatal("после продолжения файл собрался с ошибкой")
	}
	if sent := fu.bytesIn.Load(); sent > int64(len(data)-16<<20) {
		t.Errorf("уже принятое отправлено повторно: %d байт", sent)
	}
}

func TestUploadFolderAndDedup(t *testing.T) {
	a, fu := uploadApp(t)
	root := filepath.Join(t.TempDir(), "Проект")
	randFile(t, filepath.Join(root, "a.txt"), 1000)
	randFile(t, filepath.Join(root, "Фото", "b.jpg"), 300000)
	_ = os.WriteFile(filepath.Join(root, "пустой.txt"), nil, 0o644)
	_ = os.MkdirAll(filepath.Join(root, "Пустая папка"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "desktop.ini"), []byte("x"), 0o644)

	res := a.QueueUpload(UploadReq{Acc: "me@gmail.com", Paths: []string{root}})
	if res.Added != 3 || res.Folders != 3 {
		t.Fatalf("ожидалось 3 файла и 3 папки: %+v", res)
	}
	runUploads(t, a)
	fu.mu.Lock()
	byName := map[string]fakeFile{}
	ids := map[string]string{}
	for id, f := range fu.files {
		byName[f.name] = f
		ids[f.name] = id
	}
	fu.mu.Unlock()
	if byName["Проект"].parent != "root" || byName["Фото"].parent != ids["Проект"] || byName["b.jpg"].parent != ids["Фото"] {
		t.Error("структура папок на Диске не совпала")
	}
	if _, ok := byName["Пустая папка"]; !ok {
		t.Error("пустая папка не создалась")
	}
	if _, ok := byName["desktop.ini"]; ok {
		t.Error("служебный desktop.ini загружать не нужно")
	}

	// Повтор той же папки: папки переиспользуются, файлы — «уже есть на Диске»
	before := len(byName)
	a.QueueUpload(UploadReq{Acc: "me@gmail.com", Paths: []string{root}})
	js := runUploads(t, a)
	dups := 0
	for _, j := range js {
		if j.Status == "duplicate" {
			dups++
		}
	}
	fu.mu.Lock()
	after := len(fu.files)
	fu.mu.Unlock()
	if dups != 3 {
		t.Errorf("повторная загрузка: дубликатов %d", dups)
	}
	if after != before {
		t.Errorf("на Диске появились копии: было %d, стало %d", before, after)
	}
}

func TestUploadSpoolCleanup(t *testing.T) {
	a, fu := uploadApp(t)
	batch := "b1"
	p := filepath.Join(a.spoolDir(), batch, "Папка", "x.bin")
	data := randFile(t, p, 5000)
	res := a.spoolCommit("me@gmail.com", "", "", batch, []string{"Папка"})
	if res.Added != 1 {
		t.Fatalf("%+v", res)
	}
	runUploads(t, a)
	if got := fu.find("x.bin"); got == nil || !bytes.Equal(got.data, data) {
		t.Fatal("перетащенный файл не загрузился")
	}
	if _, err := os.Stat(filepath.Join(a.spoolDir(), batch)); !os.IsNotExist(err) {
		t.Error("временная копия должна удалиться после загрузки")
	}
}

// Фотоархив: тысячи мелких файлов — одним запросом каждый, без запроса «есть ли уже» на каждый файл.
func TestUploadManySmallFiles(t *testing.T) {
	a, fu := uploadApp(t)
	root := filepath.Join(t.TempDir(), "Фото")
	want := map[string][]byte{}
	for i := 0; i < 60; i++ {
		n := fmt.Sprintf("IMG_%04d.jpg", i)
		want[n] = randFile(t, filepath.Join(root, "Camera", n), 20000+i)
	}
	randFile(t, filepath.Join(root, "video.mp4"), 6<<20) // крупный — обычной сессией
	res := a.QueueUpload(UploadReq{Acc: "me@gmail.com", Paths: []string{root}})
	if res.Added != 61 {
		t.Fatalf("%+v", res)
	}
	listsBefore := fu.lists.Load() // ensureFolder при постановке в очередь
	js := runUploads(t, a)
	for _, j := range js {
		if j.Status != "done" {
			t.Fatalf("%s: %s %s", j.Name, j.Status, j.Error)
		}
	}
	for n, d := range want {
		if got := fu.find(n); got == nil || !bytes.Equal(got.data, d) {
			t.Fatalf("%s не загрузился или испорчен", n)
		}
	}
	if s := fu.simple.Load(); s != 60 {
		t.Errorf("мелких одним запросом: %d из 60", s)
	}
	if l := fu.lists.Load() - listsBefore; l > 4 {
		t.Errorf("запросов списка на 61 файл: %d — должно читаться по разу на папку", l)
	}

	// повтор той же папки: всё «уже есть», сверка по кэшу папки и MD5, без новых загрузок
	a.mu.Lock()
	a.jobs = nil
	a.mu.Unlock()
	in := fu.bytesIn.Load()
	a.QueueUpload(UploadReq{Acc: "me@gmail.com", Paths: []string{root}})
	js = runUploads(t, a)
	dups := 0
	for _, j := range js {
		if j.Status == "duplicate" {
			dups++
		}
	}
	if dups != 61 || fu.bytesIn.Load() != in {
		t.Errorf("повтор: дублей %d из 61, отправлено лишних байт %d", dups, fu.bytesIn.Load()-in)
	}
}

func TestRangeEnd(t *testing.T) {
	if rangeEnd("bytes=0-16777215") != 16777216 || rangeEnd("") != 0 {
		t.Error("разбор Range")
	}
}
