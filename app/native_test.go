package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func nativeApp(t *testing.T) *App {
	d := t.TempDir()
	a := &App{paths: Paths{Data: d}, quit: make(chan struct{}), win: newWindowCtl(), lib: newLibrary()}
	a.settings = defaultSettings()
	a.settings.Root = t.TempDir()
	a.rules = loadRules(filepath.Join(d, "rules.json"))
	return a
}

func runNative(t *testing.T, a *App, url string) (*Job, []byte) {
	j := &Job{ID: "n1", Kind: "file", URL: url, OrigURL: url, Engine: "native", Status: "waiting"}
	a.jobs = []*Job{j}
	done := make(chan struct{})
	go func() { a.startNative(j); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("свой движок завис")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if j.Status != "done" {
		t.Fatalf("статус %s: %s", j.Status, j.Error)
	}
	b, err := os.ReadFile(j.Path)
	if err != nil {
		t.Fatal(err)
	}
	return j, b
}

func TestNativeSegmented(t *testing.T) {
	payload := make([]byte, 20<<20)
	_, _ = rand.Read(payload)
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		w.Header().Set("Content-Disposition", `attachment; filename="Прайс опт.xlsx"`)
		http.ServeContent(w, r, "x.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()
	a := nativeApp(t)
	j, got := runNative(t, a, srv.URL+"/get")
	if !bytes.Equal(got, payload) {
		t.Fatal("файл собрался с ошибкой")
	}
	if reqs.Load() < 10 {
		t.Errorf("ожидалась многопоточность, запросов: %d", reqs.Load())
	}
	if !strings.Contains(j.Path, "Прайс опт.xlsx") {
		t.Errorf("имя из Content-Disposition потерялось: %s", j.Path)
	}
	if _, err := os.Stat(j.Path + ".mdstate"); err == nil {
		t.Error("служебный файл остался")
	}
}

// Сервер пускает только curl, остальным рвёт соединение (так ведут себя некоторые раздачи).
func TestNativePickyServer(t *testing.T) {
	payload := make([]byte, 6<<20)
	_, _ = rand.Read(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "curl/") {
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				c.Close()
				return
			}
		}
		http.ServeContent(w, r, "x.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()
	a := nativeApp(t)
	j, got := runNative(t, a, srv.URL+"/picky.bin")
	if !bytes.Equal(got, payload) {
		t.Fatal("файл собрался с ошибкой")
	}
	if !strings.HasPrefix(j.UserAgent, "curl/") {
		t.Errorf("должен был запомнить рабочее представление, а он %q", j.UserAgent)
	}
}

func TestNativeNoRanges(t *testing.T) {
	payload := []byte(strings.Repeat("данные ", 50000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload) // Range игнорируется
	}))
	defer srv.Close()
	a := nativeApp(t)
	_, got := runNative(t, a, srv.URL+"/file.txt")
	if !bytes.Equal(got, payload) {
		t.Fatal("одним потоком файл собрался с ошибкой")
	}
}

// Сервер рвёт каждое соединение на середине — движок должен докачать с места обрыва.
func TestNativeFlaky(t *testing.T) {
	payload := make([]byte, 8<<20)
	_, _ = rand.Read(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var from, to int64 = 0, int64(len(payload)) - 1
		if rg := r.Header.Get("Range"); rg != "" {
			fmt.Sscanf(rg, "bytes=%d-%d", &from, &to)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(payload)))
		w.Header().Set("Content-Length", strconv.FormatInt(to-from+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		n := (to - from + 1)
		if n > 64<<10 && to-from > 0 && from != 0 || n > 1<<20 {
			n = n / 2 // отдаём половину и рвём
		}
		_, _ = w.Write(payload[from : from+n])
		if hj, ok := w.(http.Hijacker); ok && n < to-from+1 {
			c, _, _ := hj.Hijack()
			c.Close()
		}
	}))
	defer srv.Close()
	a := nativeApp(t)
	_, got := runNative(t, a, srv.URL+"/flaky.bin")
	if !bytes.Equal(got, payload) {
		t.Fatal("после обрывов файл собрался с ошибкой")
	}
}

func TestNativePauseResume(t *testing.T) {
	payload := make([]byte, 64<<20)
	_, _ = rand.Read(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		http.ServeContent(w, r, "x.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()
	a := nativeApp(t)
	j := &Job{ID: "p1", Kind: "file", URL: srv.URL + "/big.bin", OrigURL: srv.URL + "/big.bin", Engine: "native", Status: "waiting"}
	a.jobs = []*Job{j}
	go a.startNative(j)
	time.Sleep(150 * time.Millisecond)
	if err := a.Action("p1", "pause"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	a.mu.Lock()
	st := j.Status
	a.mu.Unlock()
	if st != "paused" && st != "done" {
		t.Fatalf("после паузы статус %s", st)
	}
	_ = a.Action("p1", "resume")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		st = j.Status
		a.mu.Unlock()
		if st == "done" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st != "done" {
		t.Fatalf("после продолжения не докачалось: %s %s", st, j.Error)
	}
	got, _ := os.ReadFile(j.Path)
	if !bytes.Equal(got, payload) {
		t.Fatal("после паузы файл собрался с ошибкой")
	}
	_ = context.Background
}
