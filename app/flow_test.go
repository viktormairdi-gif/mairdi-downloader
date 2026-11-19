package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Поддельный aria2: принимает addUri и сразу «докачивает» файл.
type fakeAria struct {
	mu   sync.Mutex
	dl   map[string]map[string]any
	n    int
	body []byte
}

func (f *fakeAria) handler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	var result any = "OK"
	switch req.Method {
	case "aria2.addUri":
		var opts map[string]any
		_ = json.Unmarshal(req.Params[2], &opts)
		f.n++
		gid := "g" + strconv.Itoa(f.n)
		out, _ := opts["out"].(string)
		if out == "" {
			out = "Прайс-лист.csv"
		}
		p := filepath.Join(opts["dir"].(string), out)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, f.body, 0o644)
		size := strconv.Itoa(len(f.body))
		f.dl[gid] = map[string]any{"gid": gid, "status": "complete", "totalLength": size, "completedLength": size,
			"files": []map[string]any{{"path": p, "length": size}}}
		result = gid
	case "aria2.tellActive", "aria2.tellWaiting":
		result = []any{}
	case "aria2.tellStopped":
		list := []any{}
		for _, d := range f.dl {
			list = append(list, d)
		}
		result = list
	case "aria2.removeDownloadResult", "aria2.forceRemove":
		var gid string
		_ = json.Unmarshal(req.Params[1], &gid)
		delete(f.dl, gid)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": "m", "result": result})
}

func TestWatchdog(t *testing.T) {
	a := &App{}
	j := &Job{Done: 100}
	var after []func()
	a.watchdog(j, []string{"g1"}, &after) // первый замер
	if len(after) != 0 {
		t.Fatal("рано переподключаться")
	}
	j.lastMove = time.Now().Add(-stallTimeout - time.Second)
	a.watchdog(j, []string{"g1"}, &after)
	if len(after) != 1 || j.Retries != 1 || j.Error == "" {
		t.Fatalf("сторож не сработал: after=%d retries=%d err=%q", len(after), j.Retries, j.Error)
	}
	j.Done = 200
	a.watchdog(j, []string{"g1"}, &after)
	if j.Error != "" {
		t.Error("после возобновления ошибка должна очищаться")
	}
}

func TestFullFileFlow(t *testing.T) {
	root := t.TempDir()
	data := t.TempDir()
	fa := &fakeAria{dl: map[string]map[string]any{}, body: []byte("Артикул;Наименование;Цена\n1;Телефон;100\n")}
	srv := httptest.NewServer(http.HandlerFunc(fa.handler))
	defer srv.Close()
	_, portS, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portS)

	a := &App{paths: Paths{Data: data, Tools: data, Cookies: data, Metadata: data}, quit: make(chan struct{}), win: newWindowCtl()}
	a.settings = defaultSettings()
	a.settings.Root = root
	a.rules = loadRules(filepath.Join(data, "rules.json"))
	a.lib = newLibrary()
	a.aria = &Aria2{port: port, secret: "s", client: &http.Client{Timeout: 3 * time.Second}, exited: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		defer close(done)
		res := a.Add(AddRequest{URLs: []string{"https://supplier.kg/files/export"}, Source: "ui"})
		if !res.OK {
			t.Errorf("Add: %+v", res)
			return
		}
		for i := 0; i < 50; i++ {
			a.poll()
			a.schedule()
			a.mu.Lock()
			st := a.jobs[0].Status
			a.mu.Unlock()
			if st == "done" {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		a.mu.Lock()
		j := *a.jobs[0]
		a.mu.Unlock()
		want := filepath.Join(root, "Прайсы", "supplier.kg", "Прайс-лист.csv")
		if j.Status != "done" || j.Path != want {
			t.Errorf("ожидалось done в %s, получили %s / %s (%s)", want, j.Status, j.Path, j.Error)
		}
		if _, err := os.Stat(want); err != nil {
			t.Errorf("файл не на месте: %v", err)
		}
		// Повтор той же ссылки → «уже скачано», без новой закачки
		res2 := a.Add(AddRequest{URLs: []string{"https://supplier.kg/files/export?utm_source=tg"}})
		a.mu.Lock()
		j2 := a.findJob(res2.IDs[0])
		st2 := j2.Status
		a.mu.Unlock()
		if st2 != "duplicate" {
			t.Errorf("повтор должен стать duplicate, а он %s", st2)
		}
		// Другая ссылка, но тот же файл → содержимое совпадёт, копии не будет
		res3 := a.Add(AddRequest{URLs: []string{"https://mirror.kg/other"}, FileName: "copy.csv"})
		for i := 0; i < 50; i++ {
			a.poll()
			a.mu.Lock()
			st := a.findJob(res3.IDs[0]).Status
			a.mu.Unlock()
			if st == "duplicate" || st == "done" {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		a.mu.Lock()
		j3 := *a.findJob(res3.IDs[0])
		a.mu.Unlock()
		if j3.Status != "duplicate" || j3.DupOf != want {
			t.Errorf("копия по содержимому: %s → %s", j3.Status, j3.DupOf)
		}
		_ = a.stateJSON()
		_ = a.Action(j.ID, "pause")
		a.BulkAction("clear")
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("зависание: путь закачки не завершился за 15 с (взаимная блокировка?)")
	}
}
