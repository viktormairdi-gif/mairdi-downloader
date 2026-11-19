package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDPAPIRoundtrip(t *testing.T) {
	enc, err := dpapiProtect([]byte("1//refresh-token"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "refresh") {
		t.Fatal("токен лежит открытым текстом")
	}
	dec, err := dpapiUnprotect(enc)
	if err != nil || string(dec) != "1//refresh-token" {
		t.Fatalf("расшифровка: %q %v", dec, err)
	}
}

func TestSetClientJSON(t *testing.T) {
	g := newGDrive(t.TempDir())
	if err := g.setClient(`{"web":{"client_id":"x.apps.googleusercontent.com","client_secret":"s"}}`, ""); err == nil {
		t.Error("ключ веб-приложения должен отклоняться")
	}
	if err := g.setClient(`{"installed":{"client_id":"123-abc.apps.googleusercontent.com","client_secret":"GOCSPX-1"}}`, ""); err != nil {
		t.Fatal(err)
	}
	if !g.configured() || g.cfg.ClientSecret != "GOCSPX-1" {
		t.Error("ключ из JSON не сохранился")
	}
}

// Поддельный Google: токены, метаданные, список папки и выдача файлов с докачкой.
type fakeGoogle struct {
	payload  []byte
	refresh  atomic.Int32
	mediaReq atomic.Int32
	badAuth  atomic.Int32
}

func (f *fakeGoogle) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "RT" || r.Form.Get("client_secret") != "SEC" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		n := f.refresh.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "AT" + string(rune('0'+n)), "expires_in": 3600})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer AT") {
		f.badAuth.Add(1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"no"}}`))
		return
	}
	files := map[string]map[string]any{
		"F1":    {"id": "F1", "name": "Прайс июль.xlsx", "mimeType": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "size": "6291456"},
		"DIR":   {"id": "DIR", "name": "Проект", "mimeType": gFolderMime},
		"SUB":   {"id": "SUB", "name": "Фото", "mimeType": gFolderMime},
		"F2":    {"id": "F2", "name": "a.bin", "mimeType": "application/octet-stream", "size": "6291456"},
		"F3":    {"id": "F3", "name": "b.bin", "mimeType": "application/octet-stream", "size": "6291456"},
		"DOC":   {"id": "DOC", "name": "Договор", "mimeType": "application/vnd.google-apps.document"},
		"FORM":  {"id": "FORM", "name": "Анкета", "mimeType": "application/vnd.google-apps.form"},
		"EMPTY": {"id": "EMPTY", "name": "Пустая", "mimeType": gFolderMime},
	}
	kids := map[string][]string{"DIR": {"F2", "SUB", "DOC", "FORM", "EMPTY"}, "SUB": {"F3"}}
	switch {
	case r.URL.Path == "/drive/v3/files":
		q := r.URL.Query().Get("q")
		var list []map[string]any
		for id, k := range kids {
			if strings.HasPrefix(q, "'"+id+"' in parents") {
				for _, c := range k {
					list = append(list, files[c])
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": list})
	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && strings.HasSuffix(r.URL.Path, "/export"):
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("DOCX-DATA"))
	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/"):
		id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		if r.URL.Query().Get("alt") == "media" {
			f.mediaReq.Add(1)
			http.ServeContent(w, r, "x", time.Now(), bytes.NewReader(f.payload))
			return
		}
		m, ok := files[id]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"not found"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(m)
	default:
		http.NotFound(w, r)
	}
}

func driveApp(t *testing.T) (*App, *fakeGoogle) {
	fg := &fakeGoogle{payload: make([]byte, 6<<20)}
	_, _ = rand.Read(fg.payload)
	srv := httptest.NewServer(http.HandlerFunc(fg.handler))
	t.Cleanup(srv.Close)
	oldAPI, oldTok := gAPI, gTokenURL
	gAPI, gTokenURL = srv.URL, srv.URL+"/token"
	t.Cleanup(func() { gAPI, gTokenURL = oldAPI, oldTok })

	a := nativeApp(t)
	a.gd = newGDrive(t.TempDir())
	enc, err := dpapiProtect([]byte("RT"))
	if err != nil {
		t.Fatal(err)
	}
	a.gd.cfg = GConfig{ClientID: "x.apps.googleusercontent.com", ClientSecret: "SEC",
		Accounts: []*GAccount{{Email: "me@gmail.com", RefreshEnc: enc}}}
	return a, fg
}

func waitJobs(t *testing.T, a *App, n int) []*Job {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		done := 0
		for _, j := range a.jobs {
			if j.Status == "done" || j.Status == "duplicate" || j.Status == "error" {
				done++
			}
		}
		js := append([]*Job(nil), a.jobs...)
		a.mu.Unlock()
		if done >= n {
			return js
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("закачки с Диска не завершились")
	return nil
}

func TestDriveFileDownload(t *testing.T) {
	a, fg := driveApp(t)
	res := a.DriveDownload("me@gmail.com", []string{"F1"})
	if res.Added != 1 {
		t.Fatalf("не добавилось: %+v", res)
	}
	js := waitJobs(t, a, 1)
	j := js[0]
	if j.Status != "done" {
		t.Fatalf("статус %s: %s", j.Status, j.Error)
	}
	got, _ := os.ReadFile(j.Path)
	if !bytes.Equal(got, fg.payload) {
		t.Fatal("файл с Диска собрался с ошибкой")
	}
	if !strings.Contains(j.Path, "Прайсы") {
		t.Errorf("прайс должен лечь в «Прайсы», а лёг в %s", j.Path)
	}
	if fg.mediaReq.Load() < 2 {
		t.Errorf("ожидалась многопоточность, запросов: %d", fg.mediaReq.Load())
	}
	if fg.refresh.Load() != 1 {
		t.Errorf("токен должен обновиться один раз, а обновился %d", fg.refresh.Load())
	}
	if fg.badAuth.Load() != 0 {
		t.Error("были запросы без токена")
	}
	// Второй раз тот же файл — «уже скачано»
	a.DriveDownload("me@gmail.com", []string{"F1"})
	a.mu.Lock()
	last := a.jobs[len(a.jobs)-1].Status
	a.mu.Unlock()
	if last != "duplicate" {
		t.Errorf("повтор должен стать duplicate, а он %s", last)
	}
	time.Sleep(300 * time.Millisecond) // фоновое сохранение списка закачек
}

func TestDriveFolderDownload(t *testing.T) {
	a, _ := driveApp(t)
	res := a.DriveDownload("me@gmail.com", []string{"DIR"})
	if res.Added != 3 || len(res.Errors) != 1 {
		t.Fatalf("ожидалось 3 файла и 1 ошибка (форма): %+v", res)
	}
	waitJobs(t, a, 3)
	root := a.root()
	for _, p := range []string{`Google Диск\Проект\a.bin`, `Google Диск\Проект\Фото\b.bin`, `Google Диск\Проект\Договор.docx`} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("нет %s", p)
		}
	}
	if st, err := os.Stat(filepath.Join(root, `Google Диск\Проект\Пустая`)); err != nil || !st.IsDir() {
		t.Error("пустая папка не перенеслась")
	}
	// Повтор папки: всё уже лежит — ничего не качаем
	res2 := a.DriveDownload("me@gmail.com", []string{"DIR"})
	if res2.Added != 0 || res2.Skipped != 3 {
		t.Errorf("повтор папки: %+v", res2)
	}
}

func TestDriveTokenRefresh(t *testing.T) {
	a, fg := driveApp(t)
	t1, err := a.gd.accessToken("me@gmail.com")
	if err != nil {
		t.Fatal(err)
	}
	if t2, _ := a.gd.accessToken("me@gmail.com"); t2 != t1 || fg.refresh.Load() != 1 {
		t.Error("действующий токен не должен обновляться")
	}
	a.gd.tokens["me@gmail.com"].exp = time.Now().Add(30 * time.Second) // вот-вот истечёт
	t3, _ := a.gd.accessToken("me@gmail.com")
	if t3 == t1 || fg.refresh.Load() != 2 {
		t.Error("истекающий токен должен обновиться заранее")
	}
	if _, err := a.gd.accessToken("other@gmail.com"); err == nil {
		t.Error("неподключённый аккаунт должен давать ошибку")
	}
}

func TestDriveFileMapping(t *testing.T) {
	r := gRawFile{ID: "S", Name: "Ярлык", MimeType: gShortcutMime}
	r.ShortcutDetails = &struct {
		TargetID       string `json:"targetId"`
		TargetMimeType string `json:"targetMimeType"`
	}{"T", "application/vnd.google-apps.spreadsheet"}
	f := r.toFile()
	if f.ID != "T" || !f.Shortcut || f.Kind != "xlsx" || f.Folder {
		t.Errorf("ярлык на таблицу: %+v", f)
	}
	if g := (&gRawFile{MimeType: "application/vnd.google-apps.form"}).toFile(); !g.NoDown {
		t.Error("форму Google скачать нельзя — должна быть помечена")
	}
	if q := gQuote(`O'Brien \ x`); q != `'O\'Brien \\ x'` {
		t.Errorf("экранирование: %s", q)
	}
}
