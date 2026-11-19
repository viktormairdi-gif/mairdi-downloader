package main

import (
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Part struct {
	Gid    string `json:"gid"`
	Path   string `json:"path"`
	Role   string `json:"role"` // file | video | audio
	Total  int64  `json:"total"`
	Done   int64  `json:"done"`
	Speed  int64  `json:"speed"`
	Status string `json:"status"`
}

type Job struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"` // file | torrent | video
	URL         string    `json:"url"`
	OrigURL     string    `json:"origUrl"`
	PageURL     string    `json:"pageUrl"`
	Referer     string    `json:"referer"`
	Domain      string    `json:"domain"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Total       int64     `json:"total"`
	Done        int64     `json:"done"`
	Speed       int64     `json:"speed"`
	UpSpeed     int64     `json:"upSpeed"`
	ETA         int64     `json:"eta"`
	Connections int       `json:"connections"`
	Seeders     int       `json:"seeders"`
	Bitfield    string    `json:"bitfield"`
	NumPieces   int       `json:"numPieces"`
	Path        string    `json:"path"`
	Dir         string    `json:"dir"`
	Category    string    `json:"category"`
	Reason      string    `json:"reason"`
	Note        string    `json:"note"`
	Error       string    `json:"error"`
	Retries     int       `json:"retries"`
	RetryAt     time.Time `json:"retryAt"`
	Added       time.Time `json:"added"`
	Finished    time.Time `json:"finished"`
	Quality     string    `json:"quality"`
	Playlist    bool      `json:"playlist"`
	Headers     []string  `json:"headers,omitempty"`
	UserAgent   string    `json:"userAgent,omitempty"`
	CookieFile  string    `json:"cookieFile,omitempty"`
	FileName    string    `json:"fileName,omitempty"`
	ForcedDir   string    `json:"forcedDir,omitempty"`
	Parts       []Part    `json:"parts"`
	TmpDir      string    `json:"tmpDir,omitempty"`
	VideoMode   string    `json:"videoMode,omitempty"` // aria | ytdlp
	TorrentFile string    `json:"torrentFile,omitempty"`
	AwaitDir    bool      `json:"awaitDir,omitempty"`
	Source      string    `json:"source"`
	Force       bool      `json:"force,omitempty"`
	DupChecked  bool      `json:"dupChecked,omitempty"`
	DupOf       string    `json:"dupOf,omitempty"`

	Engine  string `json:"engine,omitempty"` // "" — aria2, native — свой движок
	TmpPath string `json:"tmpPath,omitempty"`
	GAcc    string `json:"gAcc,omitempty"` // файл с Google Диска этого аккаунта (качается с токеном)

	// загрузка на Google Диск (Kind = upload)
	GParent     string `json:"gParent,omitempty"`
	GFileID     string `json:"gFileId,omitempty"`
	UploadURL   string `json:"uploadUrl,omitempty"`
	DeleteAfter bool   `json:"deleteAfter,omitempty"`
	SpoolRoot   string `json:"spoolRoot,omitempty"`
	GBase       string `json:"gBase,omitempty"`  // папка, от которой строится GPath ("" — корень)
	GPath       string `json:"gPath,omitempty"`  // «Загрузки/Прайсы» — папки найдём или создадим при загрузке
	SrcJob      string `json:"srcJob,omitempty"` // закачка, которую переносим на Диск
	DelLocal    bool   `json:"delLocal,omitempty"`
	DelRoot     string `json:"delRoot,omitempty"`
	LocalGone   bool   `json:"localGone,omitempty"` // копии на компьютере больше нет — файл только на Диске

	// «Скачать в Google Диск» (у обычной закачки)
	ToDrive         string `json:"toDrive,omitempty"` // аккаунт
	DriveKeep       bool   `json:"driveKeep,omitempty"`
	DriveParent     string `json:"driveParent,omitempty"` // папка Диска, выбранная в файловом менеджере
	DriveParentName string `json:"driveParentName,omitempty"`
	DriveStep       string `json:"driveStep,omitempty"` // "" | queued | done | error | skip | cancelled
	DriveErr        string `json:"driveErr,omitempty"`
	DriveDone       int    `json:"driveDone,omitempty"`
	DriveTotal      int    `json:"driveTotal,omitempty"`
	DriveFile       string `json:"driveFile,omitempty"`
	DriveBase       string `json:"driveBase,omitempty"`
	DrivePath       string `json:"drivePath,omitempty"`

	up *upRun

	native   *nativeRun
	proc     *exec.Cmd
	stopReq  string
	lastDone int64
	lastMove time.Time
}

func (j *Job) finished() bool { return j.Status == "done" || j.Status == "seeding" }

type uiPrompt struct {
	Seq int    `json:"seq"`
	URL string `json:"url"`
}

type App struct {
	paths         Paths
	mu            sync.Mutex
	settings      Settings
	jobs          []*Job
	rules         *Rules
	lib           *Library
	aria          *Aria2
	token         string
	port          int
	tools         ToolsState
	dirty         bool
	delta         *jobsDelta // что уже отдано окну (/api/state)
	prompt        uiPrompt
	ariaOK        bool
	polled        bool
	sawWork       bool
	nextID        int64
	purgeGids     []string
	win           *windowCtl
	gd            *GDrive
	pendingUpload []string // «Отправить → Google Диск» из Проводника: ждут выбора папки
	gfMu          sync.Mutex
	gfolders      map[string]string // путь папок на Диске → id
	gkMu          sync.Mutex
	gkids         map[string]*gKids // содержимое папок Диска — для проверки «уже загружено» без запроса на каждый файл
	quitOnce      sync.Once
	quit          chan struct{}
}

func NewApp(p Paths) *App {
	a := &App{paths: p, quit: make(chan struct{}), win: newWindowCtl()}
	a.settings = defaultSettings()
	loadJSON(filepath.Join(p.Data, "settings.json"), &a.settings)
	a.settings.normalize()
	a.settings.ShutdownWhenDone = false
	a.rules = loadRules(filepath.Join(p.Data, "rules.json"))
	a.gd = newGDrive(p.Data)
	loadJSON(filepath.Join(p.Data, "jobs.json"), &a.jobs)
	for _, j := range a.jobs {
		if n, err := strconv.ParseInt(strings.TrimPrefix(j.ID, "j"), 10, 64); err == nil && n > a.nextID {
			a.nextID = n
		}
		switch j.Status {
		case "resolving", "merging", "moving":
			j.Status = "waiting"
		case "active":
			if j.Kind == "upload" {
				j.Status = "waiting" // загрузка продолжится с того места, что принял Google
			}
		}
		if j.Kind == "video" && !j.finished() {
			// после перезапуска видео заново получает свежие ссылки; уже скачанные куски продолжатся
			for _, p := range j.Parts {
				a.purgeGids = append(a.purgeGids, p.Gid)
			}
			j.Parts = nil
			if j.Status != "paused" && j.Status != "error" {
				j.Status = "waiting"
			}
		}
	}
	return a
}

func (a *App) getSettings() Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings
}

func (a *App) root() string { return a.getSettings().Root }

func (a *App) incoming() string {
	d := incomingDir(a.root(), a.paths.Data)
	if err := os.MkdirAll(d, 0o755); err == nil {
		hideFile(d)
	}
	return d
}

func (a *App) speedLimitKB(s Settings) int {
	switch s.SpeedMode {
	case "mid":
		return s.MidKB
	case "low":
		return s.LowKB
	}
	return 0
}

func (a *App) newID() string {
	a.nextID++
	return "j" + strconv.FormatInt(a.nextID, 10)
}

func (a *App) findJob(id string) *Job {
	for _, j := range a.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (a *App) markDirty() { a.dirty = true }

// anyBusy — есть закачки, чьё состояние меняется само (идёт загрузка, ожидание, перенос…).
func (a *App) anyBusy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.jobs {
		switch j.Status {
		case "done", "duplicate", "error", "paused", "seeding":
		default:
			return true
		}
	}
	return false
}

func (a *App) saveState() {
	a.mu.Lock()
	b := append([]*Job(nil), a.jobs...)
	s := a.settings
	a.dirty = false
	a.mu.Unlock()
	_ = saveJSONCompact(filepath.Join(a.paths.Data, "jobs.json"), b) // без отступов: −20% размера
	_ = saveJSON(filepath.Join(a.paths.Data, "settings.json"), s)
}

// Start — запуск фоновых служб.
func (a *App) Start() error {
	if err := os.MkdirAll(a.root(), 0o755); err != nil {
		return err
	}
	a.token = randHex(16)
	if err := a.startServer(); err != nil {
		return err
	}
	a.registerNativeHost()
	a.applyAutostart(a.getSettings().Autostart)
	go a.toolsLoop()
	go a.pollLoop()
	go a.clipboardLoop()
	go a.autoSortLoop()
	go a.cleanSpoolOrphans()
	return nil
}

func (a *App) Quit() {
	a.quitOnce.Do(func() {
		close(a.quit)
		a.mu.Lock()
		for _, j := range a.jobs {
			if j.proc != nil && j.proc.Process != nil {
				j.stopReq = "quit"
				killTree(j.proc.Process.Pid)
			}
		}
		a.mu.Unlock()
		a.saveState()
		a.aria.shutdown()
		removeEndpoint(a.paths)
		a.win.terminate()
	})
}

// ---------- добавление закачек ----------

type ExtCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
	Expires  float64 `json:"expirationDate"`
}

type AddRequest struct {
	URLs       []string    `json:"urls"`
	URL        string      `json:"url"`
	Mode       string      `json:"mode"` // auto | video | file
	Quality    string      `json:"quality"`
	Playlist   bool        `json:"playlist"`
	Referer    string      `json:"referrer"`
	PageURL    string      `json:"pageUrl"`
	Cookies    string      `json:"cookies"`
	CookieList []ExtCookie `json:"cookieList"`
	UserAgent  string      `json:"userAgent"`
	FileName   string      `json:"filename"`
	Dir        string      `json:"dir"`
	Paused     bool        `json:"paused"`
	Source     string      `json:"source"`
	Torrent    string      `json:"torrent"` // base64 .torrent
	Force      bool        `json:"force"`   // качать, даже если уже скачано
	// «Скачать в Google Диск»: аккаунт ("1" — из настроек), оставить ли копию, папка Диска
	ToDrive         string `json:"toDrive"`
	KeepLocal       *bool  `json:"keepLocal"`
	DriveParent     string `json:"driveParent"`
	DriveParentName string `json:"driveParentName"`
}

type AddResult struct {
	OK    bool     `json:"ok"`
	IDs   []string `json:"ids"`
	Error string   `json:"error,omitempty"`
}

func (a *App) Add(req AddRequest) AddResult {
	res := AddResult{}
	if req.ToDrive != "" {
		acc, err := a.driveAccFor(req.ToDrive)
		if err != nil {
			return AddResult{Error: err.Error()}
		}
		req.ToDrive = acc
		if req.KeepLocal == nil {
			keep := a.getSettings().DriveKeepLocal
			req.KeepLocal = &keep
		}
	} else {
		req.DriveParent, req.DriveParentName = "", ""
	}
	if req.Torrent != "" {
		data, err := base64.StdEncoding.DecodeString(req.Torrent)
		if err == nil {
			var j *Job
			j, err = a.addTorrentData(data, req)
			if err == nil {
				res.IDs = append(res.IDs, j.ID)
			}
		}
		if err != nil {
			res.Error = err.Error()
		}
	}
	urls := req.URLs
	if req.URL != "" {
		urls = append(urls, req.URL)
	}
	var errs []string
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		j, err := a.addOne(u, req)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		res.IDs = append(res.IDs, j.ID)
	}
	if len(errs) > 0 {
		res.Error = strings.Join(errs, "; ")
	}
	res.OK = len(res.IDs) > 0
	if res.OK {
		a.sawWork = true
		go a.saveState()
	}
	return res
}

func (a *App) baseJob(u string, req AddRequest) *Job {
	q := req.Quality
	if q == "" {
		q = a.getSettings().VideoQuality
	}
	page := req.PageURL
	if page == "" {
		page = req.Referer
	}
	j := &Job{URL: u, OrigURL: u, PageURL: page, Referer: req.Referer, Added: time.Now(), Status: "waiting",
		Quality: q, Playlist: req.Playlist, UserAgent: req.UserAgent, FileName: req.FileName, Source: req.Source}
	if req.Dir != "" && req.Dir != "auto" {
		j.ForcedDir = cleanRel(req.Dir)
	}
	if req.Cookies != "" {
		j.Headers = append(j.Headers, "Cookie: "+req.Cookies)
	}
	j.Force = req.Force
	if req.ToDrive != "" {
		j.ToDrive, j.DriveParent, j.DriveParentName = req.ToDrive, req.DriveParent, req.DriveParentName
		j.DriveKeep = req.KeepLocal != nil && *req.KeepLocal
	}
	j.Domain = a.sourceDomain(Source{URL: u, PageURL: page})
	j.Name = nameFromURL(u)
	return j
}

func urlUnescape(s string) (string, error) {
	return url.QueryUnescape(strings.ReplaceAll(s, "+", "%2B"))
}

func nameFromURL(u string) string {
	if strings.HasPrefix(u, "magnet:") {
		if i := strings.Index(u, "dn="); i >= 0 {
			v := u[i+3:]
			if k := strings.Index(v, "&"); k >= 0 {
				v = v[:k]
			}
			if s, err := urlUnescape(v); err == nil {
				return s
			}
			return v
		}
		return "Торрент (получаю данные…)"
	}
	p := u
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if s, err := urlUnescape(p); err == nil {
		p = s
	}
	if p == "" || strings.Contains(p, ":") {
		return u
	}
	return p
}

// Отметка «уже скачано» вместо повторной закачки.
func (a *App) addDuplicate(u string, req AddRequest, prev *Job) *Job {
	j := a.baseJob(u, req)
	j.Kind = prev.Kind
	j.Name, j.Path, j.Dir, j.Category = prev.Name, prev.Path, prev.Dir, prev.Category
	j.Total, j.Done = prev.Total, prev.Total
	j.Headers = nil
	j.Finished = time.Now()
	a.mu.Lock()
	if prev.finished() {
		j.Status, j.DupOf = "duplicate", prev.Path
		j.Reason = "Уже скачано " + prev.Finished.Format("02.01.2006 15:04") + " — повторно не качаю"
		if prev.LocalGone { // файл уже только на Диске — новой загрузки не нужно
			j.DupOf, j.Path = "", ""
			j.ToDrive, j.DriveStep, j.DriveFile = prev.ToDrive, "done", prev.DriveFile
			j.DriveBase, j.DrivePath, j.LocalGone = prev.DriveBase, prev.DrivePath, true
			j.Reason = "Уже скачано " + prev.Finished.Format("02.01.2006 15:04") + " и лежит на Google Диске — повторно не качаю"
		}
	} else {
		j.Status, j.DupOf = "duplicate", ""
		j.Reason = "Эта ссылка уже в очереди"
		if j.ToDrive != "" && prev.ToDrive == "" { // докачается — и уйдёт на Диск, копия останется
			prev.ToDrive, prev.DriveKeep = j.ToDrive, true
			prev.DriveParent, prev.DriveParentName = j.DriveParent, j.DriveParentName
			j.Reason += " — после скачивания отправлю на Google Диск"
		}
		j.ToDrive = ""
	}
	j.ID = a.newID()
	a.jobs = append(a.jobs, j)
	a.markDirty()
	a.mu.Unlock()
	return j
}

func (a *App) addOne(u string, req AddRequest) (*Job, error) {
	low := strings.ToLower(u)
	if !req.Force && a.getSettings().SkipDupes && (strings.HasPrefix(low, "http") || strings.HasPrefix(low, "magnet:")) {
		if prev := a.downloadedBefore(u); prev != nil {
			return a.addDuplicate(u, req, prev), nil
		}
	}
	if strings.HasPrefix(low, "magnet:") {
		return a.addMagnet(u, req)
	}
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "ftp://") {
		if strings.HasSuffix(low, ".torrent") {
			data, err := os.ReadFile(u)
			if err != nil {
				return nil, err
			}
			return a.addTorrentData(data, req)
		}
		return nil, errors.New("не похоже на ссылку: " + u)
	}
	mode := req.Mode
	if mode == "" || mode == "auto" {
		if isVideoSite(u) && !looksLikeFileURL(u) {
			mode = "video"
		} else {
			mode = "file"
		}
	}
	if mode == "video" {
		j := a.baseJob(u, req)
		j.Kind = "video"
		j.Name = "Видео: " + u
		j.Headers = nil
		if len(req.CookieList) > 0 {
			j.CookieFile = writeNetscapeCookies(a.paths.Cookies, req.CookieList)
		}
		if req.Paused {
			j.Status = "paused"
		}
		a.mu.Lock()
		j.ID = a.newID()
		a.jobs = append(a.jobs, j)
		a.markDirty()
		a.mu.Unlock()
		return j, nil
	}
	j := a.baseJob(u, req)
	j.Kind = "file"
	r, err := resolveCloud(u)
	if err != nil {
		return nil, err
	}
	j.URL, j.Note = r.URL, r.Note
	if r.FileName != "" && j.FileName == "" {
		j.FileName = r.FileName
	}
	if j.FileName != "" {
		j.Name = j.FileName
	}
	a.mu.Lock()
	j.ID = a.newID()
	a.jobs = append(a.jobs, j)
	a.mu.Unlock()
	if err := a.startFile(j, req.Paused); err != nil {
		a.mu.Lock()
		j.Status, j.Error = "error", err.Error()
		a.mu.Unlock()
	}
	return j, nil
}

// Вызывается под a.mu — папку передаём готовой, чтобы не брать блокировку повторно.
func (a *App) fileOpts(j *Job, paused bool, inc string) map[string]any {
	opts := map[string]any{"dir": inc}
	if j.FileName != "" {
		opts["out"] = safeName(j.FileName)
	}
	if len(j.Headers) > 0 {
		opts["header"] = j.Headers
	}
	if j.Referer != "" {
		opts["referer"] = j.Referer
	}
	if j.UserAgent != "" {
		opts["user-agent"] = j.UserAgent
	}
	if paused {
		opts["pause"] = "true"
	}
	return opts
}

func (a *App) startFile(j *Job, paused bool) error {
	a.mu.Lock()
	native := j.Engine == "native"
	if native {
		j.Status = "waiting"
		if paused {
			j.Status, j.stopReq = "paused", "pause"
		}
	}
	a.mu.Unlock()
	if native {
		if !paused {
			go a.startNative(j)
		}
		return nil
	}
	if a.aria == nil {
		a.mu.Lock()
		j.Status = "waiting"
		j.Error = "Жду установки движка…"
		a.mu.Unlock()
		return nil
	}
	inc := a.incoming()
	a.mu.Lock()
	opts := a.fileOpts(j, paused, inc)
	u := j.URL
	a.mu.Unlock()
	gid, err := a.aria.addURI([]string{u}, opts)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) || strings.Contains(err.Error(), "connectex") {
			// движок ещё поднимается или перезапускается — закачка подождёт, планировщик повторит
			a.mu.Lock()
			j.Status, j.Error = "waiting", "Жду запуска движка…"
			a.mu.Unlock()
			return nil
		}
		return err
	}
	a.mu.Lock()
	j.Parts = []Part{{Gid: gid, Role: "file"}}
	j.Status = "waiting"
	if paused {
		j.Status = "paused"
	}
	j.Error = ""
	a.markDirty()
	a.mu.Unlock()
	return nil
}

// Повтор после обрыва: та же папка и то же имя — aria2 продолжит с места остановки.
func (a *App) restartFile(j *Job) {
	a.mu.Lock()
	if len(j.Parts) > 0 && j.Parts[0].Path != "" && j.FileName == "" {
		j.FileName = filepath.Base(j.Parts[0].Path)
	}
	orig, note := j.OrigURL, j.Note
	a.mu.Unlock()
	if note == "Яндекс.Диск" || note == "Облако Mail.ru" {
		if r, err := resolveCloud(orig); err == nil {
			a.mu.Lock()
			j.URL = r.URL
			a.mu.Unlock()
		}
	}
	if err := a.startFile(j, false); err != nil {
		a.mu.Lock()
		j.Status, j.Error = "error", err.Error()
		a.mu.Unlock()
	}
}

func (a *App) addMagnet(u string, req AddRequest) (*Job, error) {
	j := a.baseJob(u, req)
	j.Kind = "torrent"
	j.Status = "metadata"
	j.AwaitDir = true
	j.Headers = nil
	a.mu.Lock()
	j.ID = a.newID()
	a.jobs = append(a.jobs, j)
	a.mu.Unlock()
	if a.aria == nil {
		a.mu.Lock()
		j.Status = "waiting"
		a.mu.Unlock()
		return j, nil
	}
	gid, err := a.aria.addURI([]string{u}, map[string]any{"dir": a.paths.Metadata})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	j.Parts = []Part{{Gid: gid, Role: "file"}}
	a.markDirty()
	a.mu.Unlock()
	return j, nil
}

func (a *App) addTorrentData(data []byte, req AddRequest) (*Job, error) {
	tname, files, err := parseTorrent(data)
	if err != nil {
		return nil, err
	}
	dir, reason := classifyTorrent(files, tname)
	if r := a.rules.match("", tname, ""); r != nil {
		dir, reason = r.Dir, "Ваше правило: "+r.describe()
	}
	j := a.baseJob("", req)
	j.Kind = "torrent"
	j.Name = tname
	j.URL, j.OrigURL = "", ""
	if j.ForcedDir != "" {
		dir, reason = j.ForcedDir, "Папка выбрана при добавлении"
	}
	j.Dir, j.Category, j.Reason = dir, topCategory(dir), reason
	root := a.root()
	full := filepath.Join(root, dir)
	_ = os.MkdirAll(full, 0o755)
	// копия .torrent — в «Торренты», рядом с остальными
	tdir := filepath.Join(root, "Торренты")
	_ = os.MkdirAll(tdir, 0o755)
	keep := uniquePath(filepath.Join(tdir, safeName(tname)+".torrent"))
	_ = os.WriteFile(keep, data, 0o644)
	j.TorrentFile = keep
	a.mu.Lock()
	j.ID = a.newID()
	a.jobs = append(a.jobs, j)
	a.mu.Unlock()
	if a.aria == nil {
		return j, nil
	}
	opts := map[string]any{"dir": full}
	if req.Paused {
		opts["pause"] = "true"
	}
	gid, err := a.aria.addTorrent(base64.StdEncoding.EncodeToString(data), opts)
	if err != nil {
		a.mu.Lock()
		j.Status, j.Error = "error", err.Error()
		a.mu.Unlock()
		return j, nil
	}
	a.mu.Lock()
	j.Parts = []Part{{Gid: gid, Role: "file"}}
	j.Path = filepath.Join(full, safeName(tname))
	a.markDirty()
	a.mu.Unlock()
	return j, nil
}

// ---------- опрос движка ----------

func (a *App) pollLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-a.quit:
			return
		case <-t.C:
		}
		a.poll()
		a.schedule()
		// список закачек может быть огромным (тысячи загружаемых фото) — пишем не чаще раза в 3 с
		// раз в 15 с — только если что-то качается (прогресс меняется без markDirty);
		// раньше 20-мегабайтный jobs.json переписывался каждые 15 с и в простое
		if a.dirty && time.Since(last) > 3*time.Second || time.Since(last) > 15*time.Second && a.anyBusy() {
			a.saveState()
			last = time.Now()
		}
	}
}

func (a *App) poll() {
	if a.aria == nil {
		return
	}
	list, err := a.aria.list()
	if err != nil {
		if a.aria.dead() { // движок упал — перезапускаем
			a.aria = nil
			go func() {
				if err := a.startAria2(); err == nil {
					a.resumeOrphans()
				}
			}()
		}
		return
	}
	by := map[string]*aStatus{}
	for i := range list {
		by[list[i].Gid] = &list[i]
	}
	var after []func()
	a.mu.Lock()
	for _, j := range a.jobs {
		if len(j.Parts) == 0 || j.Status == "done" || j.Status == "duplicate" || j.Status == "moving" || j.Status == "merging" || j.Status == "resolving" {
			continue
		}
		switch j.Kind {
		case "file":
			a.pollFile(j, by, &after)
		case "torrent":
			a.pollTorrent(j, by, &after)
		case "video":
			a.pollVideoParts(j, by, &after)
		}
	}
	a.polled = true
	a.mu.Unlock()
	for _, f := range after {
		f()
	}
}

// Сторож: сервер держит соединение, но не отдаёт данные. 40 с без единого байта —
// переподключаемся (пауза + продолжение), aria2 продолжит с того же места.
const stallTimeout = 40 * time.Second

func (a *App) watchdog(j *Job, gids []string, after *[]func()) {
	now := time.Now()
	if j.Done != j.lastDone || j.lastMove.IsZero() {
		if j.Done != j.lastDone && strings.HasPrefix(j.Error, "Сервер перестал") {
			j.Error = ""
		}
		j.lastDone, j.lastMove = j.Done, now
		return
	}
	if now.Sub(j.lastMove) < stallTimeout {
		return
	}
	j.lastMove = now
	j.Retries++
	j.Error = "Сервер перестал отдавать данные — переподключаюсь (" + strconv.Itoa(j.Retries) + ")"
	*after = append(*after, func() {
		for _, g := range gids {
			a.aria.call("forcePause", g)
		}
		time.Sleep(1500 * time.Millisecond)
		for _, g := range gids {
			a.aria.call("unpause", g)
		}
	})
}

func fillPart(p *Part, s *aStatus) {
	p.Total, p.Done, p.Speed, p.Status = atoi64(s.TotalLength), atoi64(s.CompletedLength), atoi64(s.DownloadSpeed), s.Status
	if len(s.Files) > 0 && s.Files[0].Path != "" {
		p.Path = cleanAriaPath(s.Files[0].Path)
	}
}

func (a *App) commonFill(j *Job, s *aStatus) {
	j.Total, j.Done = atoi64(s.TotalLength), atoi64(s.CompletedLength)
	j.Speed, j.UpSpeed = atoi64(s.DownloadSpeed), atoi64(s.UploadSpeed)
	j.Connections = int(atoi64(s.Connections))
	j.Seeders = int(atoi64(s.NumSeeders))
	j.Bitfield, j.NumPieces = s.Bitfield, int(atoi64(s.NumPieces))
	j.ETA = 0
	if j.Speed > 0 && j.Total > j.Done {
		j.ETA = (j.Total - j.Done) / j.Speed
	}
}

func (a *App) pollFile(j *Job, by map[string]*aStatus, after *[]func()) {
	p := &j.Parts[0]
	s := by[p.Gid]
	if s == nil {
		return
	}
	fillPart(p, s)
	a.commonFill(j, s)
	if p.Path != "" {
		j.Name = filepath.Base(p.Path)
	}
	// Как только известны имя и размер — проверяем, нет ли такого файла уже.
	if !j.DupChecked && j.Total > 0 && p.Path != "" && len(s.FollowedBy) == 0 {
		j.DupChecked = true
		if !j.Force && a.settings.SkipDupes && a.lib != nil {
			if ex := a.lib.findByNameSize(j.Name, j.Total); ex != "" {
				gid, part := p.Gid, p.Path
				j.Status, j.DupOf, j.Path = "duplicate", ex, ex
				j.Dir = relTo(a.settings.Root, filepath.Dir(ex))
				j.Category = topCategory(j.Dir)
				j.Reason = "Файл с таким именем и размером уже есть — скачивание остановлено"
				j.Speed, j.ETA, j.Done = 0, 0, j.Total
				j.Finished = time.Now()
				a.markDirty()
				*after = append(*after, func() {
					a.aria.remove(gid)
					time.Sleep(300 * time.Millisecond)
					_ = os.Remove(part)
					_ = os.Remove(part + ".aria2")
				})
				return
			}
		}
	}
	switch s.Status {
	case "active":
		j.Status = "active"
		a.sawWork = true
		// Сервер не отдаёт aria2 ни байта 20 с или подвисает трижды — переходим на свой движок.
		if j.Done == 0 && !j.lastMove.IsZero() && time.Since(j.lastMove) > 20*time.Second ||
			j.Retries >= 3 && j.Done == j.lastDone && time.Since(j.lastMove) > stallTimeout {
			j.lastMove = time.Now()
			*after = append(*after, func() {
				a.switchToNative(j, "Сервер плохо работает с основным движком — качаю вторым")
			})
			return
		}
		a.watchdog(j, []string{p.Gid}, after)
	case "waiting":
		j.Status = "waiting"
		j.lastMove = time.Time{}
	case "paused":
		j.Status = "paused"
		j.lastMove = time.Time{}
	case "complete":
		gid := p.Gid
		if len(s.FollowedBy) > 0 { // это был .torrent по ссылке — дальше качаем сам торрент
			tf := p.Path
			j.Kind, j.Status, j.AwaitDir = "torrent", "metadata", true
			j.Parts = []Part{{Gid: s.FollowedBy[0], Role: "file"}}
			a.markDirty()
			*after = append(*after, func() {
				a.aria.call("removeDownloadResult", gid)
				if tf != "" {
					tdir := filepath.Join(a.root(), "Торренты")
					_ = os.MkdirAll(tdir, 0o755)
					dst := uniquePath(filepath.Join(tdir, filepath.Base(tf)))
					if moveFile(tf, dst) == nil {
						a.mu.Lock()
						j.TorrentFile = dst
						a.mu.Unlock()
					}
				}
			})
			return
		}
		j.Status = "moving"
		tmp := p.Path
		*after = append(*after, func() { go a.finishFile(j, tmp, gid) })
	case "error":
		gid := p.Gid
		j.Speed, j.ETA = 0, 0
		if j.Done == 0 && (s.ErrorCode == "1" || s.ErrorCode == "2" || s.ErrorCode == "6" || s.ErrorCode == "22") {
			// сервер обрывает именно aria2 (бывает у раздающих серверов) — пробуем свой движок
			*after = append(*after, func() {
				a.switchToNative(j, "Сервер оборвал соединение — качаю вторым движком")
			})
			return
		}
		if retryableAria(s.ErrorCode) && j.Retries < 50 {
			j.Retries++
			wait := retryWait(j.Retries)
			j.Status, j.RetryAt = "retrying", time.Now().Add(wait)
			j.Error = humanAriaError(s.ErrorCode, s.ErrorMessage) + " — повтор через " + strconv.Itoa(int(wait.Seconds())) + " с"
		} else {
			j.Status, j.Error = "error", humanAriaError(s.ErrorCode, s.ErrorMessage)
		}
		a.markDirty()
		*after = append(*after, func() { a.aria.call("removeDownloadResult", gid) })
	case "removed":
		if j.stopReq == "" {
			j.Status = "error"
			j.Error = "Закачка остановлена движком"
		}
	}
}

func (a *App) pollTorrent(j *Job, by map[string]*aStatus, after *[]func()) {
	p := &j.Parts[0]
	s := by[p.Gid]
	if s == nil {
		return
	}
	fillPart(p, s)
	a.commonFill(j, s)
	if s.Bittorrent != nil && s.Bittorrent.Info != nil && s.Bittorrent.Info.Name != "" {
		j.Name = s.Bittorrent.Info.Name
	}
	if s.Status == "complete" && len(s.FollowedBy) > 0 {
		old := p.Gid
		j.Parts[0] = Part{Gid: s.FollowedBy[0], Role: "file"}
		j.Status, j.AwaitDir = "metadata", true
		a.markDirty()
		*after = append(*after, func() { a.aria.call("removeDownloadResult", old) })
		return
	}
	if j.AwaitDir {
		if s.Status == "paused" && len(s.Files) > 0 {
			gid := p.Gid
			st := *s
			j.AwaitDir = false
			*after = append(*after, func() { a.placeTorrent(j, gid, &st) })
		} else if s.Status == "active" || s.Status == "waiting" {
			j.Status = "metadata"
		}
		return
	}
	switch s.Status {
	case "active":
		if j.Total > 0 && j.Done >= j.Total {
			if j.Status != "seeding" {
				j.Finished = time.Now()
				a.markDirty()
			}
			j.Status = "seeding"
			j.Speed = 0
		} else {
			j.Status = "active"
			a.sawWork = true
		}
	case "waiting":
		j.Status = "waiting"
	case "paused":
		j.Status = "paused"
	case "complete":
		if j.Finished.IsZero() {
			j.Finished = time.Now()
		}
		j.Status, j.Speed, j.UpSpeed = "done", 0, 0
		gid := p.Gid
		a.markDirty()
		*after = append(*after, func() { a.aria.call("removeDownloadResult", gid) })
	case "error":
		j.Status, j.Error = "error", humanAriaError(s.ErrorCode, s.ErrorMessage)
		a.markDirty()
	}
}

// Магнит получил список файлов — решаем папку и запускаем.
func (a *App) placeTorrent(j *Job, gid string, s *aStatus) {
	name := ""
	if s.Bittorrent != nil && s.Bittorrent.Info != nil {
		name = s.Bittorrent.Info.Name
	}
	base := cleanAriaPath(s.Dir)
	var files []tFile
	for _, f := range s.Files {
		rel := strings.TrimPrefix(cleanAriaPath(f.Path), base)
		files = append(files, tFile{Name: strings.TrimLeft(rel, `\`), Size: atoi64(f.Length)})
	}
	dir, reason := classifyTorrent(files, name)
	if r := a.rules.match("", name, ""); r != nil {
		dir, reason = r.Dir, "Ваше правило: "+r.describe()
	}
	a.mu.Lock()
	if j.ForcedDir != "" {
		dir, reason = j.ForcedDir, "Папка выбрана при добавлении"
	}
	a.mu.Unlock()
	full := filepath.Join(a.root(), dir)
	_ = os.MkdirAll(full, 0o755)
	_, err := a.aria.call("changeOption", gid, map[string]any{"dir": full})
	if err == nil {
		_, err = a.aria.call("unpause", gid)
	}
	a.mu.Lock()
	j.Dir, j.Category, j.Reason = dir, topCategory(dir), reason
	if name != "" {
		j.Name = name
		j.Path = filepath.Join(full, safeName(name))
	}
	j.Status = "waiting"
	if err != nil {
		j.Status, j.Error = "error", err.Error()
	}
	a.markDirty()
	a.mu.Unlock()
}

// После перезапуска: закачки, которых движок не знает, запускаем заново.
func (a *App) resumeOrphans() {
	if a.aria == nil {
		return
	}
	list, err := a.aria.list()
	if err != nil {
		return
	}
	for _, g := range a.purgeGids {
		a.aria.remove(g)
	}
	a.purgeGids = nil
	known := map[string]bool{}
	for _, s := range list {
		known[s.Gid] = true
	}
	a.mu.Lock()
	var redo []*Job
	for _, j := range a.jobs {
		if j.Kind == "video" || j.Status == "done" || j.Status == "duplicate" || j.Status == "error" {
			continue
		}
		if len(j.Parts) == 0 || !known[j.Parts[0].Gid] {
			redo = append(redo, j)
		}
	}
	a.mu.Unlock()
	for _, j := range redo {
		a.mu.Lock()
		kind, paused, url, tf := j.Kind, j.Status == "paused", j.OrigURL, j.TorrentFile
		a.mu.Unlock()
		switch {
		case kind == "file":
			if err := a.startFile(j, paused); err != nil {
				a.mu.Lock()
				j.Status, j.Error = "error", err.Error()
				a.mu.Unlock()
			}
		case kind == "torrent" && tf != "":
			data, err := os.ReadFile(tf)
			if err != nil {
				continue
			}
			a.mu.Lock()
			full := filepath.Join(a.settings.Root, j.Dir)
			a.mu.Unlock()
			opts := map[string]any{"dir": full, "check-integrity": "true"}
			if paused {
				opts["pause"] = "true"
			}
			if gid, err := a.aria.addTorrent(base64.StdEncoding.EncodeToString(data), opts); err == nil {
				a.mu.Lock()
				j.Parts = []Part{{Gid: gid, Role: "file"}}
				if j.Status == "seeding" {
					j.Status = "done" // раздачу после перезапуска не продолжаем
					a.aria.remove(gid)
				}
				a.mu.Unlock()
			}
		case kind == "torrent" && strings.HasPrefix(url, "magnet:"):
			if gid, err := a.aria.addURI([]string{url}, map[string]any{"dir": a.paths.Metadata}); err == nil {
				a.mu.Lock()
				j.Parts = []Part{{Gid: gid, Role: "file"}}
				j.AwaitDir, j.Status = true, "metadata"
				a.mu.Unlock()
			}
		}
	}
}

// ---------- планировщик: повторы, видео, выключение ----------

func (a *App) schedule() {
	now := time.Now()
	var retry, startVid []*Job
	a.mu.Lock()
	activeVideo := 0
	busy := false
	for _, j := range a.jobs {
		switch j.Status {
		case "active", "waiting", "metadata", "resolving", "merging", "moving", "retrying":
			busy = true
		}
		if j.Kind == "video" && (j.Status == "resolving" || j.Status == "active" || j.Status == "merging" || j.proc != nil) {
			activeVideo++
		}
	}
	for _, j := range a.jobs {
		if j.Status == "retrying" && now.After(j.RetryAt) {
			j.Status = "waiting"
			if j.Kind == "video" {
				continue
			}
			retry = append(retry, j)
		}
	}
	for _, j := range a.jobs {
		if j.Kind == "video" && j.Status == "waiting" && len(j.Parts) == 0 && j.proc == nil && activeVideo < a.settings.MaxActive {
			j.Status = "resolving"
			activeVideo++
			startVid = append(startVid, j)
		}
	}
	shutdown := a.settings.ShutdownWhenDone && a.sawWork && !busy
	if shutdown {
		a.settings.ShutdownWhenDone = false
	}
	a.mu.Unlock()
	for _, j := range retry {
		if j.Kind == "file" {
			go a.restartFile(j)
		}
	}
	for _, j := range startVid {
		go a.startVideo(j)
	}
	a.forwardToDrive()
	a.scheduleUploads()
	if a.aria == nil {
		// движок ещё ставится — файлы ждут
	} else {
		a.mu.Lock()
		var pending []*Job
		for _, j := range a.jobs {
			if j.Kind == "file" && j.Status == "waiting" && len(j.Parts) == 0 {
				pending = append(pending, j)
			}
		}
		a.mu.Unlock()
		for _, j := range pending {
			_ = a.startFile(j, false)
		}
	}
	if shutdown {
		_ = hiddenCmd("shutdown", "/s", "/t", "60", "/c", "Все закачки завершены — компьютер выключится через минуту. Отмена: shutdown /a").Start()
	}
}

// ---------- завершение файла: раскладка по папкам ----------

func (a *App) finishFile(j *Job, tmp, gid string) {
	a.mu.Lock()
	src := Source{URL: j.OrigURL, PageURL: j.PageURL, Domain: j.Domain}
	forced := j.ForcedDir
	a.mu.Unlock()
	name := filepath.Base(tmp)
	a.mu.Lock()
	checkDup := !j.Force && a.settings.SkipDupes
	a.mu.Unlock()
	if checkDup {
		if ex := a.library().findSameContent(tmp); ex != "" {
			_ = os.Remove(tmp)
			_ = os.Remove(tmp + ".aria2")
			if a.aria != nil {
				a.aria.call("removeDownloadResult", gid)
			}
			a.mu.Lock()
			j.Status, j.DupOf, j.Path = "duplicate", ex, ex
			j.Dir = relTo(a.settings.Root, filepath.Dir(ex))
			j.Category = topCategory(j.Dir)
			j.Name = filepath.Base(ex)
			j.Reason = "Точно такой же файл уже есть (совпадает содержимое) — копию не оставил"
			j.Speed, j.ETA, j.Finished = 0, 0, time.Now()
			a.markDirty()
			a.mu.Unlock()
			return
		}
	}
	v := a.Classify(tmp, name, src)
	if forced != "" {
		v.Dir, v.Reason = forced, "Папка выбрана при добавлении"
	}
	if v.NewExt != "" {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + v.NewExt
	}
	dst := uniquePath(filepath.Join(a.root(), v.Dir, name))
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	err := moveFile(tmp, dst)
	_ = os.Remove(tmp + ".aria2")
	if a.aria != nil {
		a.aria.call("removeDownloadResult", gid)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	j.Speed, j.ETA, j.Connections = 0, 0, 0
	if err != nil {
		j.Status, j.Error, j.Path = "done", "Не удалось переложить: "+err.Error(), tmp
		a.markDirty()
		return
	}
	j.Path, j.Dir, j.Category, j.Reason = dst, v.Dir, topCategory(v.Dir), v.Reason
	j.Name = filepath.Base(dst)
	j.Status, j.Finished, j.Error = "done", time.Now(), ""
	if j.Total <= 0 { // размер заранее не был известен (выгрузка документа, архив «на лету»)
		j.Total = fileSize(dst)
	}
	j.Done = j.Total
	j.Headers = nil
	a.markDirty()
	if a.lib != nil {
		a.lib.add(dst, fileSize(dst))
	}
}

// Пауза перед повтором: 10, 20, 40, 60, 60… секунд — не дольше минуты.
func retryWait(n int) time.Duration {
	return time.Duration(min(5*(1<<min(n, 4)), 60)) * time.Second
}

func relTo(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil || strings.HasPrefix(r, "..") {
		return p
	}
	return r
}

func uniquePath(p string) string {
	if _, err := os.Lstat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; i < 10000; i++ {
		c := base + " (" + strconv.Itoa(i) + ")" + ext
		if _, err := os.Lstat(c); err != nil {
			return c
		}
	}
	return base + " " + randHex(3) + ext
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		in.Close()
		return err
	}
	_, err = io.Copy(out, in)
	in.Close()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	if st, e := os.Stat(src); e == nil {
		_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	}
	return os.Remove(src)
}

// ---------- действия пользователя ----------

func (a *App) Action(id, action string) error {
	a.mu.Lock()
	j := a.findJob(id)
	if j == nil {
		a.mu.Unlock()
		return errors.New("закачка не найдена")
	}
	kind, status := j.Kind, j.Status
	gids := []string{}
	for _, p := range j.Parts {
		gids = append(gids, p.Gid)
	}
	path := j.Path
	gone := j.LocalGone
	a.mu.Unlock()
	if kind == "upload" {
		return a.uploadAction(j, action)
	}

	switch action {
	case "drive":
		return a.openOnDrive(j)
	case "to-drive":
		return a.sendToDrive(j)
	case "drive-retry":
		a.driveRetry(j)
		return nil
	case "open", "folder":
		if gone { // файл перенесён на Google Диск — открываем там
			return a.openOnDrive(j)
		}
	}

	switch action {
	case "pause":
		if kind == "video" && status != "active" && status != "resolving" {
			a.mu.Lock()
			if j.proc == nil && len(j.Parts) == 0 {
				j.Status = "paused"
			}
			a.mu.Unlock()
		}
		a.mu.Lock()
		if j.proc != nil && j.proc.Process != nil {
			j.stopReq = "pause"
			killTree(j.proc.Process.Pid)
		}
		if j.Engine == "native" {
			j.stopReq = "pause"
			if j.native != nil {
				j.native.cancel()
			}
		}
		if j.Status == "retrying" || j.Status == "waiting" {
			j.Status = "paused"
		}
		a.mu.Unlock()
		for _, g := range gids {
			a.aria.call("forcePause", g)
		}
	case "resume", "retry":
		switch status {
		case "done", "seeding", "duplicate", "moving", "merging", "active", "resolving", "metadata":
			return nil // уже скачано или и так качается
		}
		a.mu.Lock()
		j.Error = ""
		if action == "retry" {
			j.Retries = 0
		}
		native := j.Engine == "native" && kind == "file"
		if native {
			j.stopReq = ""
			if j.native == nil {
				j.Status = "waiting"
			}
		}
		a.mu.Unlock()
		switch {
		case native:
			go a.startNative(j)
		case kind == "video" && len(gids) == 0:
			a.mu.Lock()
			if j.proc == nil {
				j.Status = "waiting"
			}
			a.mu.Unlock()
		case status == "error" || status == "retrying":
			if kind == "file" {
				go a.restartFile(j)
			} else if kind == "video" {
				for _, g := range gids {
					a.aria.remove(g)
				}
				a.mu.Lock()
				j.Parts, j.Status = nil, "waiting"
				a.mu.Unlock()
			} else if kind == "torrent" {
				for _, g := range gids {
					a.aria.call("unpause", g)
				}
			}
		default:
			for _, g := range gids {
				a.aria.call("unpause", g)
			}
		}
	case "remove", "removeWithFile":
		if status == "duplicate" {
			action = "remove" // файл-оригинал принадлежит другой записи — не трогаем
			path = ""
		}
		a.mu.Lock()
		if j.proc != nil && j.proc.Process != nil {
			j.stopReq = "remove"
			killTree(j.proc.Process.Pid)
		}
		if j.native != nil {
			j.stopReq = "remove"
			j.native.cancel()
		}
		finished := j.finished()
		tmpDir := j.TmpDir
		var partPaths []string
		for _, p := range j.Parts {
			partPaths = append(partPaths, p.Path)
		}
		if j.TmpPath != "" && !finished {
			tp := j.TmpPath
			go func() { // свой движок закрывает файл не мгновенно
				time.Sleep(time.Second)
				_ = os.Remove(tp)
				_ = os.Remove(tp + ".mdstate")
			}()
		}
		out := a.jobs[:0]
		for _, x := range a.jobs {
			if x != j {
				out = append(out, x)
			}
		}
		a.jobs = out
		a.markDirty()
		a.mu.Unlock()
		for _, g := range gids {
			a.aria.remove(g)
		}
		if !finished {
			for _, p := range partPaths {
				if p != "" {
					_ = os.Remove(p)
					_ = os.Remove(p + ".aria2")
				}
			}
			if kind == "torrent" && path != "" && action == "removeWithFile" {
				a.safeRemoveAll(path)
			}
		}
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
		if action == "removeWithFile" && finished && path != "" {
			a.safeRemoveAll(path)
		}
	case "force":
		a.mu.Lock()
		req := AddRequest{URL: j.OrigURL, PageURL: j.PageURL, Referer: j.Referer, Quality: j.Quality, Force: true, Source: j.Source, UserAgent: j.UserAgent}
		if j.ToDrive != "" && !j.LocalGone {
			keep := j.DriveKeep
			req.ToDrive, req.KeepLocal, req.DriveParent, req.DriveParentName = j.ToDrive, &keep, j.DriveParent, j.DriveParentName
		}
		if kind == "video" {
			req.Mode = "video"
		} else {
			req.Mode = "file"
		}
		a.mu.Unlock()
		if req.URL == "" {
			return errors.New("у записи нет ссылки")
		}
		res := a.Add(req)
		if !res.OK {
			return errors.New(res.Error)
		}
		return a.Action(id, "remove")
	case "open":
		if path == "" {
			return errors.New("у закачки нет файла")
		}
		if _, err := os.Stat(path); err != nil {
			return errors.New("файл не найден — удалён или перемещён: " + path)
		}
		if err := shellOpen(path); err != nil {
			return err
		}
	case "folder":
		if path != "" {
			revealInExplorer(path)
		} else {
			shellOpen(a.root())
		}
	default:
		return errors.New("неизвестное действие")
	}
	return nil
}

// Удаляет только внутри папки загрузок и никогда — саму папку категории.
func (a *App) safeRemoveAll(p string) {
	root := filepath.Clean(a.root())
	p = filepath.Clean(p)
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || !strings.Contains(rel, `\`) && isCategory(rel) {
		return
	}
	_ = os.RemoveAll(p)
}

func isCategory(name string) bool {
	for _, c := range Categories {
		if strings.EqualFold(c, name) {
			return true
		}
	}
	return false
}

func (a *App) BulkAction(action string) {
	a.mu.Lock()
	var ids []string
	for _, j := range a.jobs {
		switch action {
		case "pause":
			if j.Status == "active" || j.Status == "waiting" || j.Status == "metadata" || j.Status == "retrying" {
				ids = append(ids, j.ID)
			}
		case "resume":
			if j.Status == "paused" || j.Status == "error" {
				ids = append(ids, j.ID)
			}
		case "clear":
			if j.Status == "done" || j.Status == "duplicate" {
				ids = append(ids, j.ID)
			}
		}
	}
	a.mu.Unlock()
	for _, id := range ids {
		switch action {
		case "clear":
			a.mu.Lock()
			out := a.jobs[:0]
			for _, x := range a.jobs {
				if x.ID != id {
					out = append(out, x)
				}
			}
			a.jobs = out
			a.markDirty()
			a.mu.Unlock()
		default:
			_ = a.Action(id, action)
		}
	}
}

// Переместить готовый файл в другую папку и, если попросили, запомнить правило.
func (a *App) Move(id, dir, remember string) error {
	dir = cleanRel(dir)
	a.mu.Lock()
	j := a.findJob(id)
	if j == nil {
		a.mu.Unlock()
		return errors.New("закачка не найдена")
	}
	if !j.finished() {
		a.mu.Unlock()
		if remember != "" {
			return a.rememberFor(j, dir, remember)
		}
		a.mu.Lock()
		j.ForcedDir = dir
		a.mu.Unlock()
		return nil
	}
	src := j.Path
	seeding := j.Status == "seeding"
	var gids []string
	for _, p := range j.Parts {
		gids = append(gids, p.Gid)
	}
	a.mu.Unlock()
	if seeding {
		for _, g := range gids {
			a.aria.remove(g)
		}
	}
	dst := uniquePath(filepath.Join(a.root(), dir, filepath.Base(src)))
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := moveFile(src, dst); err != nil {
		if err2 := os.Rename(src, dst); err2 != nil {
			return err
		}
	}
	a.mu.Lock()
	j.Path, j.Dir, j.Category = dst, dir, topCategory(dir)
	j.Reason = "Вы переложили вручную"
	if seeding {
		j.Status = "done"
	}
	a.markDirty()
	a.mu.Unlock()
	if remember != "" {
		return a.rememberFor(j, dir, remember)
	}
	return nil
}

func (a *App) rememberFor(j *Job, dir, remember string) error {
	a.mu.Lock()
	domain, name := j.Domain, j.Name
	a.mu.Unlock()
	switch remember {
	case "domain":
		if domain == "" {
			return errors.New("у файла неизвестен сайт")
		}
		return a.rules.add("domain", domain, dir)
	case "ext":
		e := strings.ToLower(filepath.Ext(name))
		if e == "" {
			return errors.New("у файла нет расширения")
		}
		return a.rules.add("ext", e, dir)
	}
	return nil
}

// Список папок для диалога «Переместить»: стандартные + реально существующие.
func (a *App) knownDirs() []string {
	set := map[string]bool{}
	for _, d := range StandardDirs {
		set[d] = true
	}
	root := a.root()
	for _, c := range Categories {
		ents, err := os.ReadDir(filepath.Join(root, c))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && c != "Торренты" {
				set[filepath.Join(c, e.Name())] = true
			}
		}
	}
	for _, r := range a.rules.list() {
		set[r.Dir] = true
	}
	var out []string
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
