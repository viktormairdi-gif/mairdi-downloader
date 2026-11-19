package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ytFormat struct {
	URL       string            `json:"url"`
	Protocol  string            `json:"protocol"`
	Ext       string            `json:"ext"`
	FormatID  string            `json:"format_id"`
	Headers   map[string]string `json:"http_headers"`
	Vcodec    string            `json:"vcodec"`
	Acodec    string            `json:"acodec"`
	Fragments []json.RawMessage `json:"fragments"`
}

type ytInfo struct {
	Type             string            `json:"_type"`
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Extractor        string            `json:"extractor_key"`
	WebpageURL       string            `json:"webpage_url"`
	URL              string            `json:"url"`
	Protocol         string            `json:"protocol"`
	Ext              string            `json:"ext"`
	FormatID         string            `json:"format_id"`
	Headers          map[string]string `json:"http_headers"`
	Vcodec           string            `json:"vcodec"`
	Acodec           string            `json:"acodec"`
	Fragments        []json.RawMessage `json:"fragments"`
	RequestedFormats []ytFormat        `json:"requested_formats"`
	Entries          []struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		ID    string `json:"id"`
	} `json:"entries"`
}

func (a *App) tool(name string) string {
	p := filepath.Join(a.paths.Tools, name)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func formatFor(q string) string {
	if q == "audio" {
		return "ba[ext=m4a]/ba/b"
	}
	if q == "best" || q == "" {
		return "bv*+ba/b"
	}
	h := "[height<=" + q + "]"
	if q == "2160" {
		return "bv*" + h + "+ba/b" + h + "/bv*+ba/b"
	}
	// до 1080p — H.264 + AAC: открывается любым плеером и телефоном без перекодирования
	return "bv*" + h + "[vcodec^=avc1]+ba[ext=m4a]/bv*" + h + "[ext=mp4]+ba[ext=m4a]/bv*" + h + "+ba/b" + h + "/bv*+ba/b"
}

func (a *App) ytBaseArgs(j *Job) []string {
	args := []string{"--no-warnings", "--no-colors", "--encoding", "utf-8", "--socket-timeout", "30"}
	if d := a.tool("deno.exe"); d != "" {
		args = append(args, "--js-runtimes", "deno:"+d)
	}
	if j.CookieFile != "" {
		args = append(args, "--cookies", j.CookieFile)
	}
	if j.UserAgent != "" {
		args = append(args, "--user-agent", j.UserAgent)
	}
	return args
}

func ytCmd(exe string, args ...string) *exec.Cmd {
	c := hiddenCmd(exe, args...)
	c.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	return c
}

func (a *App) videoFail(j *Job, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j.proc = nil
	j.Speed, j.ETA = 0, 0
	if j.Retries < 5 {
		j.Retries++
		wait := time.Duration(15*j.Retries) * time.Second
		j.Status, j.RetryAt = "retrying", time.Now().Add(wait)
		j.Error = msg + " — повтор через " + strconv.Itoa(int(wait.Seconds())) + " с"
	} else {
		j.Status, j.Error = "error", msg
	}
	a.markDirty()
}

// startVideo: получить прямые ссылки через yt-dlp и качать дорожки многопоточно через aria2.
func (a *App) startVideo(j *Job) {
	yt := a.tool("yt-dlp.exe")
	if yt == "" {
		a.mu.Lock()
		j.Status, j.Error = "waiting", "Жду установки yt-dlp…"
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	u, q, playlist, forced, mode := j.OrigURL, j.Quality, j.Playlist, j.ForcedDir, j.VideoMode
	args := a.ytBaseArgs(j)
	a.mu.Unlock()
	if mode == "ytdlp" {
		a.runYtdlp(j)
		return
	}
	args = append(args, "-J", "-f", formatFor(q))
	if playlist {
		args = append(args, "--flat-playlist", "--yes-playlist")
	} else {
		args = append(args, "--no-playlist")
	}
	args = append(args, u)
	var stderr bytes.Buffer
	c := ytCmd(yt, args...)
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		a.videoFail(j, ytError(stderr.String(), err))
		return
	}
	var info ytInfo
	if err := json.Unmarshal(out, &info); err != nil {
		a.videoFail(j, "yt-dlp вернул непонятный ответ")
		return
	}
	site := siteFolder(hostOf(firstNonEmpty(info.WebpageURL, u)))
	if site == "" {
		site = info.Extractor
	}
	// Плейлист → отдельная закачка на каждое видео
	if info.Type == "playlist" {
		dir := forced
		if dir == "" {
			dir = joinNonEmpty("Видео", site, safeName(info.Title))
			if q == "audio" {
				dir = joinNonEmpty("Музыка", safeName(info.Title))
			}
		}
		n := 0
		for _, e := range info.Entries {
			eu := e.URL
			if eu == "" {
				continue
			}
			if !strings.HasPrefix(eu, "http") && e.ID != "" && strings.Contains(u, "youtu") {
				eu = "https://www.youtube.com/watch?v=" + e.ID
			}
			req := AddRequest{Mode: "video", Quality: q, Dir: dir, Source: "playlist", PageURL: u}
			a.mu.Lock()
			if j.ToDrive != "" { // плейлист «в Диск» — каждый ролик туда же
				keep := j.DriveKeep
				req.ToDrive, req.KeepLocal, req.DriveParent, req.DriveParentName = j.ToDrive, &keep, j.DriveParent, j.DriveParentName
			}
			a.mu.Unlock()
			if _, err := a.addOne(eu, req); err == nil {
				n++
			}
		}
		a.mu.Lock()
		j.Status, j.Finished = "done", time.Now()
		j.Name = "Плейлист «" + info.Title + "» — " + strconv.Itoa(n) + " видео добавлено в очередь"
		j.Dir, j.Category, j.Reason = dir, topCategory(dir), "Плейлист разложен на отдельные видео"
		a.markDirty()
		a.mu.Unlock()
		return
	}

	formats := info.RequestedFormats
	if len(formats) == 0 && info.URL != "" {
		formats = []ytFormat{{URL: info.URL, Protocol: info.Protocol, Ext: info.Ext, FormatID: info.FormatID,
			Headers: info.Headers, Vcodec: info.Vcodec, Acodec: info.Acodec, Fragments: info.Fragments}}
	}
	dir := forced
	if dir == "" {
		dir = joinNonEmpty("Видео", site)
		if q == "audio" {
			dir = "Музыка"
		}
	}
	title := safeName(firstNonEmpty(info.Title, info.ID, "video"))
	a.mu.Lock()
	j.Dir, j.Category = dir, topCategory(dir)
	j.Reason = "Видео с сайта " + site
	if q == "audio" {
		j.Reason = "Звук из видео с сайта " + site
	}
	j.Name = title
	j.Error = ""
	a.mu.Unlock()

	// Уже скачано (то же название в той же папке)?
	if !j.Force && a.getSettings().SkipDupes {
		for _, ext := range []string{".mp4", ".webm", ".mkv", ".mp3", ".m4a"} {
			p := filepath.Join(a.root(), dir, title+ext)
			if st, err := os.Stat(p); err == nil && st.Size() > 0 {
				a.mu.Lock()
				j.Status, j.Path, j.DupOf, j.Finished = "duplicate", p, p, time.Now()
				j.Name = filepath.Base(p)
				j.Total, j.Done = st.Size(), st.Size()
				j.Reason = "Это видео уже скачано — повторно не качаю"
				a.markDirty()
				a.mu.Unlock()
				return
			}
		}
	}

	direct := a.aria != nil && len(formats) > 0
	for _, f := range formats {
		if (f.Protocol != "https" && f.Protocol != "http") || len(f.Fragments) > 0 || f.URL == "" {
			direct = false
		}
	}
	if !direct {
		a.mu.Lock()
		j.VideoMode = "ytdlp"
		a.mu.Unlock()
		a.runYtdlp(j)
		return
	}
	tmp := filepath.Join(a.incoming(), "video-"+j.ID)
	_ = os.MkdirAll(tmp, 0o755)
	var parts []Part
	for _, f := range formats {
		opts := map[string]any{"dir": tmp, "out": safeName(info.ID + "." + f.FormatID + "." + f.Ext)}
		var hdr []string
		for k, v := range f.Headers {
			if strings.EqualFold(k, "User-Agent") {
				opts["user-agent"] = v
				continue
			}
			hdr = append(hdr, k+": "+v)
		}
		if len(hdr) > 0 {
			opts["header"] = hdr
		}
		gid, err := a.aria.addURI([]string{f.URL}, opts)
		if err != nil {
			for _, p := range parts {
				a.aria.remove(p.Gid)
			}
			a.mu.Lock()
			j.VideoMode = "ytdlp"
			a.mu.Unlock()
			a.runYtdlp(j)
			return
		}
		role := "video"
		if f.Vcodec == "none" || (f.Vcodec == "" && f.Acodec != "" && f.Acodec != "none" && len(formats) > 1) {
			role = "audio"
		}
		parts = append(parts, Part{Gid: gid, Role: role, Path: filepath.Join(tmp, opts["out"].(string))})
	}
	a.mu.Lock()
	j.Parts, j.TmpDir, j.VideoMode, j.Status = parts, tmp, "aria", "active"
	a.markDirty()
	a.mu.Unlock()
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func ytError(stderr string, err error) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "ERROR:") {
			l = strings.TrimSpace(strings.TrimPrefix(l, "ERROR:"))
			switch {
			case strings.Contains(l, "Sign in to confirm") || strings.Contains(l, "login required") || strings.Contains(l, "Private video"):
				return "Сайт требует вход — скачайте через расширение Chrome (оно передаст вход): " + l
			case strings.Contains(l, "Unsupported URL"):
				return "На этой странице не найдено видео"
			}
			return l
		}
	}
	if err != nil {
		return "yt-dlp: " + err.Error()
	}
	return "Не удалось получить видео"
}

// Сводка по дорожкам, которые качает aria2.
func (a *App) pollVideoParts(j *Job, by map[string]*aStatus, after *[]func()) {
	var total, done, speed int64
	conns := 0
	all := true
	var failed *aStatus
	for i := range j.Parts {
		p := &j.Parts[i]
		s := by[p.Gid]
		if s == nil {
			all = false
			continue
		}
		fillPart(p, s)
		total += p.Total
		done += p.Done
		speed += p.Speed
		conns += int(atoi64(s.Connections))
		if s.Status != "complete" {
			all = false
		}
		if s.Status == "error" {
			failed = s
		}
		if i == 0 {
			j.Bitfield, j.NumPieces = s.Bitfield, int(atoi64(s.NumPieces))
		}
	}
	j.Total, j.Done, j.Speed, j.Connections = total, done, speed, conns
	j.ETA = 0
	if speed > 0 && total > done {
		j.ETA = (total - done) / speed
	}
	paused := true
	for _, p := range j.Parts {
		if p.Status != "paused" {
			paused = false
		}
	}
	switch {
	case failed != nil:
		gids := []string{}
		for _, p := range j.Parts {
			gids = append(gids, p.Gid)
		}
		j.Parts, j.VideoMode, j.Status = nil, "ytdlp", "waiting" // запасной путь — сам yt-dlp
		j.Error = "Прямая ссылка не сработала (" + humanAriaError(failed.ErrorCode, failed.ErrorMessage) + "), качаю через yt-dlp"
		a.markDirty()
		*after = append(*after, func() {
			for _, g := range gids {
				a.aria.remove(g)
			}
		})
	case all && len(j.Parts) > 0:
		j.Status = "merging"
		a.markDirty()
		*after = append(*after, func() { go a.finishVideo(j) })
	case paused:
		j.Status = "paused"
	default:
		j.Status = "active"
		a.sawWork = true
		var gids []string
		for _, p := range j.Parts {
			gids = append(gids, p.Gid)
		}
		a.watchdog(j, gids, after)
	}
}

// Склейка видео и звука без перекодирования и раскладка в папку.
func (a *App) finishVideo(j *Job) {
	a.mu.Lock()
	parts := append([]Part(nil), j.Parts...)
	title, dir, q, tmp := j.Name, j.Dir, j.Quality, j.TmpDir
	a.mu.Unlock()
	for _, p := range parts {
		a.aria.call("removeDownloadResult", p.Gid)
	}
	var vpath, apath string
	for _, p := range parts {
		if p.Role == "audio" {
			apath = p.Path
		} else {
			vpath = p.Path
		}
	}
	if vpath == "" {
		vpath, apath = apath, ""
	}
	ff := a.tool("ffmpeg.exe")
	out := ""
	var err error
	switch {
	case q == "audio":
		if ff == "" {
			out = vpath // ffmpeg ещё ставится — оставляем m4a
			break
		}
		out = filepath.Join(tmp, "out.mp3")
		err = runFF(ff, "-y", "-i", vpath, "-vn", "-c:a", "libmp3lame", "-q:a", "2", out)
	case apath == "":
		out = vpath
	default:
		if ff == "" {
			a.mu.Lock()
			j.Status, j.Error, j.RetryAt = "merging", "Жду установки ffmpeg для склейки…", time.Now()
			a.mu.Unlock()
			time.Sleep(20 * time.Second)
			go a.finishVideo(j)
			return
		}
		ve, ae := strings.ToLower(filepath.Ext(vpath)), strings.ToLower(filepath.Ext(apath))
		ext := ".mkv"
		if ve == ".mp4" && (ae == ".m4a" || ae == ".mp4") {
			ext = ".mp4"
		} else if ve == ".webm" && ae == ".webm" {
			ext = ".webm"
		}
		out = filepath.Join(tmp, "out"+ext)
		args := []string{"-y", "-i", vpath, "-i", apath, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy"}
		if ext == ".mp4" {
			args = append(args, "-movflags", "+faststart")
		}
		err = runFF(ff, append(args, out)...)
	}
	if err != nil {
		a.videoFail(j, "Не удалось склеить видео и звук: "+err.Error())
		return
	}
	dst := uniquePath(filepath.Join(a.root(), dir, title+filepath.Ext(out)))
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := moveFile(out, dst); err != nil {
		a.videoFail(j, "Не удалось переложить файл: "+err.Error())
		return
	}
	_ = os.RemoveAll(tmp)
	a.mu.Lock()
	j.Path, j.Name = dst, filepath.Base(dst)
	j.Status, j.Finished, j.Error = "done", time.Now(), ""
	j.Speed, j.ETA, j.Connections = 0, 0, 0
	j.Done = j.Total
	j.TmpDir = ""
	a.markDirty()
	a.mu.Unlock()
	a.cleanupCookies(j)
	a.library().add(dst, fileSize(dst))
}

func runFF(ff string, args ...string) error {
	var stderr bytes.Buffer
	c := hiddenCmd(ff, append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

func (a *App) cleanupCookies(j *Job) {
	a.mu.Lock()
	cf := j.CookieFile
	j.CookieFile = ""
	a.mu.Unlock()
	if cf != "" {
		_ = os.Remove(cf)
	}
}

// Запасной путь: yt-dlp качает сам (HLS/DASH-фрагменты — в 16 потоков).
func (a *App) runYtdlp(j *Job) {
	yt := a.tool("yt-dlp.exe")
	inc := a.incoming()
	root := a.root()
	a.mu.Lock()
	if j.proc != nil {
		a.mu.Unlock()
		return
	}
	u, q, dir := j.OrigURL, j.Quality, j.Dir
	if dir == "" {
		dir = joinNonEmpty("Видео", siteFolder(hostOf(u)))
		if q == "audio" {
			dir = "Музыка"
		}
		j.Dir, j.Category = dir, topCategory(dir)
		j.Reason = "Видео с сайта " + siteFolder(hostOf(u))
	}
	tmp := filepath.Join(inc, "video-"+j.ID)
	j.TmpDir = tmp
	args := a.ytBaseArgs(j)
	a.mu.Unlock()
	_ = os.MkdirAll(tmp, 0o755)
	args = append(args, "--newline", "--progress", "--no-simulate", "--no-playlist", "-N", "16",
		"--retries", "infinite", "--fragment-retries", "infinite", "--continue",
		"-f", formatFor(q), "-P", "home:"+filepath.Join(root, dir), "-P", "temp:"+tmp,
		"-o", "%(title).150B.%(ext)s",
		"--progress-template", "download:PROGRESS %(progress.downloaded_bytes)s|%(progress.total_bytes)s|%(progress.total_bytes_estimate)s|%(progress.speed)s|%(progress.eta)s",
		"--print", "before_dl:TITLE %(title)s", "--print", "after_move:FILE %(filepath)s")
	if ff := a.tool("ffmpeg.exe"); ff != "" {
		args = append(args, "--ffmpeg-location", ff)
		if q == "audio" {
			args = append(args, "-x", "--audio-format", "mp3", "--audio-quality", "2")
		} else {
			args = append(args, "--merge-output-format", "mp4/mkv")
		}
	}
	args = append(args, u)
	c := ytCmd(yt, args...)
	stdout, _ := c.StdoutPipe()
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Start(); err != nil {
		a.videoFail(j, err.Error())
		return
	}
	attachToKillJob(c.Process)
	a.mu.Lock()
	j.proc, j.stopReq, j.Status, j.VideoMode = c, "", "active", "ytdlp"
	a.mu.Unlock()
	final := ""
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "PROGRESS "):
			f := strings.Split(strings.TrimPrefix(line, "PROGRESS "), "|")
			if len(f) == 5 {
				num := func(s string) int64 { v, _ := strconv.ParseFloat(s, 64); return int64(v) }
				tot := num(f[1])
				if tot == 0 {
					tot = num(f[2])
				}
				a.mu.Lock()
				j.Done, j.Total, j.Speed, j.ETA = num(f[0]), tot, num(f[3]), num(f[4])
				a.sawWork = true
				a.mu.Unlock()
			}
		case strings.HasPrefix(line, "TITLE "):
			a.mu.Lock()
			j.Name = strings.TrimPrefix(line, "TITLE ")
			a.mu.Unlock()
		case strings.HasPrefix(line, "FILE "):
			final = strings.TrimPrefix(line, "FILE ")
		}
	}
	err := c.Wait()
	a.mu.Lock()
	stop := j.stopReq
	j.proc = nil
	a.mu.Unlock()
	switch {
	case stop == "pause":
		a.mu.Lock()
		j.Status, j.Speed, j.ETA = "paused", 0, 0
		a.mu.Unlock()
		return
	case stop != "":
		return
	case err != nil:
		a.videoFail(j, ytError(stderr.String(), err))
		return
	}
	_ = os.RemoveAll(tmp)
	a.mu.Lock()
	if final != "" {
		j.Path, j.Name = final, filepath.Base(final)
		if st, e := os.Stat(final); e == nil {
			j.Total, j.Done = st.Size(), st.Size()
		}
	}
	j.Status, j.Finished, j.Error = "done", time.Now(), ""
	j.Speed, j.ETA, j.TmpDir = 0, 0, ""
	a.markDirty()
	a.mu.Unlock()
	a.cleanupCookies(j)
	if final != "" {
		a.library().add(final, fileSize(final))
	}
}

// Netscape-файл cookies для yt-dlp (вход на сайты передаёт расширение).
func writeNetscapeCookies(dir string, list []ExtCookie) string {
	var sb strings.Builder
	sb.WriteString("# Netscape HTTP Cookie File\n")
	for _, c := range list {
		dom := c.Domain
		sub := "FALSE"
		if strings.HasPrefix(dom, ".") {
			sub = "TRUE"
		}
		if c.HTTPOnly {
			dom = "#HttpOnly_" + dom
		}
		sec := "FALSE"
		if c.Secure {
			sec = "TRUE"
		}
		p := c.Path
		if p == "" {
			p = "/"
		}
		sb.WriteString(strings.Join([]string{dom, sub, p, sec, strconv.FormatInt(int64(c.Expires), 10), c.Name, c.Value}, "\t"))
		sb.WriteByte('\n')
	}
	f := filepath.Join(dir, "c"+randHex(8)+".txt")
	if os.WriteFile(f, []byte(sb.String()), 0o600) != nil {
		return ""
	}
	return f
}
