package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed ui
var uiFS embed.FS

//go:embed icon.ico
var iconICO []byte

type endpoint struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func endpointPath(p Paths) string { return filepath.Join(p.Data, "endpoint.json") }

func readEndpoint(p Paths) (endpoint, bool) {
	var e endpoint
	ok := loadJSON(endpointPath(p), &e)
	return e, ok && e.Port > 0
}

func removeEndpoint(p Paths) { _ = os.Remove(endpointPath(p)) }

func epURL(e endpoint, path string) string { return "http://127.0.0.1:" + strconv.Itoa(e.Port) + path }

func callEndpoint(e endpoint, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", epURL(e, path), strings.NewReader(string(b)))
	req.Header.Set("X-Token", e.Token)
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("HTTP " + resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func pingEndpoint(e endpoint) bool {
	var r map[string]any
	return callEndpoint(e, "/api/ping", map[string]any{}, &r) == nil
}

func (a *App) startServer() error {
	var ln net.Listener
	var err error
	for _, port := range []int{17880, 17881, 17882, 0} {
		ln, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	a.port = ln.Addr().(*net.TCPAddr).Port
	if err := saveJSON(endpointPath(a.paths), endpoint{Port: a.port, Token: a.token, PID: os.Getpid()}); err != nil {
		return err
	}
	mux := http.NewServeMux()
	sub, _ := fs.Sub(uiFS, "ui")
	static := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !a.hostOK(r) {
			http.Error(w, "forbidden", 403)
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			b, _ := fs.ReadFile(sub, "index.html")
			html := strings.Replace(string(b), "__TOKEN__", a.token, 1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(html))
			return
		}
		if r.URL.Path == "/icon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write(iconICO)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		static.ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/", a.handleAPI)
	mux.HandleFunc("/gthumb", a.handleGThumb)
	mux.HandleFunc("/spool", a.handleSpool)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// Защита: только 127.0.0.1 и без подмены Host (DNS rebinding).
func (a *App) hostOK(r *http.Request) bool {
	h := r.Host
	return h == "127.0.0.1:"+strconv.Itoa(a.port) || h == "localhost:"+strconv.Itoa(a.port)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) handleAPI(w http.ResponseWriter, r *http.Request) {
	if !a.hostOK(r) || r.Header.Get("X-Token") != a.token {
		http.Error(w, "forbidden", 403)
		return
	}
	var body map[string]json.RawMessage
	if r.Method == "POST" {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&body)
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(body[k], &s)
		return s
	}
	fail := func(err error) { writeJSON(w, map[string]any{"ok": false, "error": err.Error()}) }
	okResp := map[string]any{"ok": true}

	switch strings.TrimPrefix(r.URL.Path, "/api/") {
	case "ping":
		writeJSON(w, map[string]any{"ok": true, "pid": os.Getpid()})
	case "state":
		var since int64
		_ = json.Unmarshal(body["since"], &since)
		writeJSON(w, a.stateFor(str("epoch"), since))
	case "add":
		var req AddRequest
		raw, _ := json.Marshal(body)
		_ = json.Unmarshal(raw, &req)
		if req.Source == "" {
			req.Source = "ui"
		}
		writeJSON(w, a.Add(req))
	case "ext":
		a.handleExt(w, body)
	case "action":
		ids := []string{}
		_ = json.Unmarshal(body["ids"], &ids)
		if id := str("id"); id != "" {
			ids = append(ids, id)
		}
		act := str("action")
		var errs []string
		for _, id := range ids {
			if err := a.Action(id, act); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			fail(errors.New(strings.Join(errs, "; ")))
			return
		}
		writeJSON(w, okResp)
	case "bulk":
		a.BulkAction(str("action"))
		writeJSON(w, okResp)
	case "move":
		ids := []string{}
		_ = json.Unmarshal(body["ids"], &ids)
		var errs []string
		for i, id := range ids {
			rem := str("remember")
			if i > 0 {
				rem = ""
			}
			if err := a.Move(id, str("dir"), rem); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			fail(errors.New(strings.Join(errs, "; ")))
			return
		}
		writeJSON(w, okResp)
	case "settings":
		var s Settings
		raw, _ := json.Marshal(body)
		if err := json.Unmarshal(raw, &s); err != nil {
			fail(err)
			return
		}
		if err := a.applySettings(s); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "speed":
		a.mu.Lock()
		a.settings.SpeedMode = str("mode")
		a.settings.normalize()
		s := a.settings
		a.mu.Unlock()
		a.pushEngineOptions(s)
		go a.saveState()
		writeJSON(w, okResp)
	case "shutdown-when-done":
		var on bool
		_ = json.Unmarshal(body["on"], &on)
		a.mu.Lock()
		a.settings.ShutdownWhenDone = on
		a.mu.Unlock()
		writeJSON(w, okResp)
	case "rules":
		writeJSON(w, map[string]any{"ok": true, "rules": a.rules.list()})
	case "rules/delete":
		var i int
		_ = json.Unmarshal(body["index"], &i)
		if err := a.rules.remove(i); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "rules/add":
		if err := a.rules.add(str("kind"), str("value"), str("dir")); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "sort-root":
		moved, dupes := a.SortRoot(true)
		writeJSON(w, map[string]any{"ok": true, "moved": moved, "dupes": dupes})
	case "open-root":
		shellOpen(a.root())
		writeJSON(w, okResp)
	case "open-dir":
		p := filepath.Join(a.root(), cleanRel(str("dir")))
		_ = os.MkdirAll(p, 0o755)
		shellOpen(p)
		writeJSON(w, okResp)
	case "pick-folder":
		writeJSON(w, map[string]any{"ok": true, "path": pickFolder(a.root())})
	case "ext-path":
		writeJSON(w, map[string]any{"ok": true, "path": a.extensionDir()})
	case "open-extension":
		shellOpen(a.extensionDir())
		writeJSON(w, okResp)
	case "ext-status":
		writeJSON(w, a.extStatus())
	case "ext-browser":
		if err := openExtensionsPage(str("browser")); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "register-default":
		a.registerAsDefault()
		writeJSON(w, okResp)
	case "prompt-ack":
		a.mu.Lock()
		a.prompt.URL = ""
		a.mu.Unlock()
		writeJSON(w, okResp)
	case "show":
		a.win.show()
		writeJSON(w, okResp)
	case "cli":
		var args []string
		_ = json.Unmarshal(body["args"], &args)
		a.HandleCLI(args)
		writeJSON(w, okResp)
	case "gdrive/client":
		if err := a.gd.setClient(str("clientId"), str("clientSecret")); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "gdrive/connect":
		err := a.gd.startLogin(func(email string, err error) {
			if err == nil {
				a.win.show()
			}
		})
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "gdrive/login-reopen":
		if !a.gd.reopenLogin() {
			fail(errors.New("вход не идёт — нажмите «Подключить» ещё раз"))
			return
		}
		writeJSON(w, okResp)
	case "gdrive/login-cancel":
		a.gd.cancelLogin()
		writeJSON(w, okResp)
	case "gdrive/disconnect":
		a.gd.disconnect(str("acc"))
		writeJSON(w, okResp)
	case "gdrive/about":
		ab, err := a.gd.about(str("acc"))
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "about": ab})
	case "gdrive/list":
		var req GListReq
		raw, _ := json.Marshal(body)
		_ = json.Unmarshal(raw, &req)
		res, err := a.gd.list(req)
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "files": res.Files, "next": res.Next})
	case "gdrive/download":
		ids := []string{}
		_ = json.Unmarshal(body["ids"], &ids)
		writeJSON(w, a.DriveDownload(str("acc"), ids))
	case "gdrive/upload", "gdrive/pick-upload", "gdrive/spool-commit", "gdrive/pending-commit":
		var req UploadReq
		raw, _ := json.Marshal(body)
		_ = json.Unmarshal(raw, &req)
		switch strings.TrimPrefix(r.URL.Path, "/api/gdrive/") {
		case "pick-upload":
			req.Paths = pickUploadPaths(str("mode"))
			if len(req.Paths) == 0 {
				writeJSON(w, map[string]any{"ok": false, "cancel": true})
				return
			}
		case "spool-commit":
			var tops []string
			_ = json.Unmarshal(body["tops"], &tops)
			writeJSON(w, a.spoolCommit(req.Acc, req.Parent, req.ParentName, str("batch"), tops))
			return
		case "pending-commit":
			a.mu.Lock()
			req.Paths, a.pendingUpload = a.pendingUpload, nil
			a.mu.Unlock()
		}
		writeJSON(w, a.QueueUpload(req))
	case "gdrive/pending-cancel":
		a.mu.Lock()
		a.pendingUpload = nil
		a.mu.Unlock()
		writeJSON(w, okResp)
	case "sendto":
		var on bool
		_ = json.Unmarshal(body["on"], &on)
		if err := setSendTo(a.paths.Exe, on); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "gdrive/op":
		var req GOpReq
		raw, _ := json.Marshal(body)
		_ = json.Unmarshal(raw, &req)
		writeJSON(w, a.gd.op(req))
	case "gdrive/perms":
		ps, link, err := a.gd.perms(str("acc"), str("id"))
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "perms": ps, "link": link})
	case "gdrive/perm-link":
		if err := a.gd.setLinkAccess(str("acc"), str("id"), str("role")); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "gdrive/perm-add":
		var notify bool
		_ = json.Unmarshal(body["notify"], &notify)
		if err := a.gd.addPerson(str("acc"), str("id"), str("email"), str("role"), notify); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "gdrive/perm-change":
		if err := a.gd.changePerm(str("acc"), str("id"), str("perm"), str("role")); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "gdrive/open":
		if err := a.driveOpen(str("acc"), str("id")); err != nil {
			fail(err)
			return
		}
		writeJSON(w, okResp)
	case "open-url":
		// только известные адреса Google — окно не может открыть что угодно
		u := str("url")
		switch hostOf(u) {
		case "console.cloud.google.com", "myaccount.google.com", "drive.google.com":
			if strings.HasPrefix(u, "https://") {
				shellOpen(u)
			}
		}
		writeJSON(w, okResp)
	case "quit":
		writeJSON(w, okResp)
		go a.Quit()
	default:
		http.NotFound(w, r)
	}
}

func (a *App) stateJSON() map[string]any { return a.stateFor("", 0) }

// stateFor: epoch/since — что уже есть у окна; в ответе jobs — только изменённое (см. jobsDelta).
func (a *App) stateFor(epoch string, since int64) map[string]any {
	a.mu.Lock()
	jobs := make([]*Job, len(a.jobs))
	copy(jobs, a.jobs)
	var down, up int64
	active := 0
	counts := map[string]int{}
	for _, j := range jobs {
		down += j.Speed
		up += j.UpSpeed
		if j.Status == "active" || j.Status == "resolving" || j.Status == "metadata" {
			active++
		}
		if j.Category != "" {
			counts[j.Category]++
		}
	}
	s := a.settings
	prompt := a.prompt
	pending := append([]string{}, a.pendingUpload...)
	if a.delta == nil {
		a.delta = newJobsDelta()
	}
	b, removed, rev, full := a.delta.build(jobs, epoch, since)
	a.mu.Unlock()
	return map[string]any{
		"jobs":       b,
		"removed":    removed,
		"full":       full,
		"rev":        rev,
		"epoch":      a.delta.epoch,
		"down":       down,
		"up":         up,
		"active":     active,
		"settings":   s,
		"tools":      a.toolsSnapshot(),
		"categories": Categories,
		"counts":     counts,
		"dirs":       a.knownDirs(),
		"prompt":     prompt,
		"gdrive":     findGoogleDrive(),
		"version":    Version,
		"g": map[string]any{
			"configured": a.gd.configured(),
			"accounts":   a.gd.accounts(),
			"login":      a.gd.loginState(),
			"loginUrl":   a.gd.loginLink(),
			"pending":    pending,
			"sendTo":     sendToInstalled(),
		},
	}
}

func (a *App) applySettings(s Settings) error {
	s.normalize()
	if st, err := os.Stat(s.Root); err != nil || !st.IsDir() {
		if err := os.MkdirAll(s.Root, 0o755); err != nil {
			return errors.New("папка загрузок недоступна: " + s.Root)
		}
	}
	a.mu.Lock()
	old := a.settings
	s.AutoSortSince = old.AutoSortSince
	s.ShutdownWhenDone = old.ShutdownWhenDone
	if s.AutoSortRoot && !old.AutoSortRoot {
		s.AutoSortSince = time.Now().Unix()
	}
	a.settings = s
	a.mu.Unlock()
	if old.Autostart != s.Autostart {
		a.applyAutostart(s.Autostart)
	}
	if old.Root != s.Root {
		go a.scanLibrary()
		if a.aria != nil {
			a.aria.call("changeGlobalOption", map[string]any{"dir": a.incoming()})
		}
	}
	a.pushEngineOptions(s)
	a.saveState()
	return nil
}

func (a *App) pushEngineOptions(s Settings) {
	if a.aria == nil {
		return
	}
	c := strconv.Itoa(s.Connections)
	a.aria.call("changeGlobalOption", map[string]any{
		"max-overall-download-limit": strconv.Itoa(a.speedLimitKB(s)) + "K",
		"max-overall-upload-limit":   strconv.Itoa(s.UploadLimitKB) + "K",
		"max-concurrent-downloads":   strconv.Itoa(s.MaxActive),
		"max-connection-per-server":  c,
		"split":                      c,
		"seed-ratio":                 strconv.FormatFloat(s.SeedRatio, 'f', 2, 64),
	})
}

// ---------- сообщения от расширения браузера (через native messaging) ----------

func (a *App) handleExt(w http.ResponseWriter, body map[string]json.RawMessage) {
	var typ, extVer string
	_ = json.Unmarshal(body["type"], &typ)
	_ = json.Unmarshal(body["extVer"], &extVer)
	a.extTouch(extVer)
	switch typ {
	case "ping":
		s := a.getSettings()
		writeJSON(w, map[string]any{"ok": true, "version": Version, "root": s.Root, "drive": len(a.gd.accounts()) > 0})
	case "open":
		a.win.show()
		writeJSON(w, map[string]any{"ok": true})
	case "add":
		var req AddRequest
		raw, _ := json.Marshal(body)
		_ = json.Unmarshal(raw, &req)
		if req.Source == "" {
			req.Source = "browser"
		}
		if req.Source == "intercept" {
			s := a.getSettings()
			h := hostOf(firstNonEmpty(req.PageURL, req.Referer, req.URL))
			skip := !s.Intercept
			for _, ex := range s.ExcludedSites {
				if h == ex || strings.HasSuffix(h, "."+ex) {
					skip = true
				}
			}
			if skip {
				writeJSON(w, map[string]any{"ok": false, "skip": true})
				return
			}
		}
		res := a.Add(req)
		out := map[string]any{"ok": res.OK, "ids": res.IDs, "error": res.Error}
		if res.OK && len(res.IDs) == 1 {
			a.mu.Lock()
			if j := a.findJob(res.IDs[0]); j != nil && j.Status == "duplicate" {
				out["duplicate"] = true
				out["path"] = j.DupOf
				out["name"] = j.Name
			}
			a.mu.Unlock()
		}
		writeJSON(w, out)
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "неизвестная команда"})
	}
}

// Папка расширения: рядом с программой или на уровень выше (bin\..\extension).
func (a *App) extensionDir() string {
	d := filepath.Dir(a.paths.Exe)
	for _, p := range []string{filepath.Join(d, "extension"), filepath.Join(filepath.Dir(d), "extension")} {
		if _, err := os.Stat(filepath.Join(p, "manifest.json")); err == nil {
			return p
		}
	}
	return filepath.Join(filepath.Dir(d), "extension")
}

// Аргументы командной строки: magnet-ссылки, .torrent-файлы, ссылки.
func (a *App) HandleCLI(args []string) {
	for _, x := range args {
		if x == "--gdrive-send" { // Проводник: «Отправить → Google Диск»
			var paths []string
			for _, p := range args {
				if !strings.HasPrefix(p, "--") && strings.TrimSpace(p) != "" {
					paths = append(paths, p)
				}
			}
			a.mu.Lock()
			a.pendingUpload = append(a.pendingUpload, paths...)
			a.mu.Unlock()
			a.win.show()
			return
		}
	}
	var urls []string
	for _, x := range args {
		x = strings.TrimSpace(x)
		if x == "" || strings.HasPrefix(x, "--") {
			continue
		}
		if strings.HasSuffix(strings.ToLower(x), ".torrent") {
			if data, err := os.ReadFile(x); err == nil {
				_, _ = a.addTorrentData(data, AddRequest{Source: "cli"})
				continue
			}
		}
		urls = append(urls, x)
	}
	if len(urls) > 0 {
		a.Add(AddRequest{URLs: urls, Source: "cli"})
	}
	a.win.show()
}
