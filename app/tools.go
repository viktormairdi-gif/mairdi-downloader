package main

import (
	"archive/zip"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Компоненты скачиваются один раз с официальных страниц выпусков на GitHub.
type toolSpec struct {
	Name  string // файл в папке tools
	Title string
	URL   string
	InZip []string // какие файлы достать из архива (по имени)
}

var toolSpecs = []toolSpec{
	{"aria2c.exe", "Движок закачек aria2", "https://github.com/aria2/aria2/releases/download/release-1.37.0/aria2-1.37.0-win-64bit-build1.zip", []string{"aria2c.exe"}},
	{"yt-dlp.exe", "Видео-модуль yt-dlp", "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe", nil},
	{"deno.exe", "JS-движок для YouTube (deno)", "https://github.com/denoland/deno/releases/latest/download/deno-x86_64-pc-windows-msvc.zip", []string{"deno.exe"}},
	{"ffmpeg.exe", "Склейка видео ffmpeg", "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip", []string{"ffmpeg.exe", "ffprobe.exe"}},
}

type ToolsState struct {
	Ready   map[string]bool `json:"ready"`
	Busy    string          `json:"busy"`
	Percent float64         `json:"percent"`
	Error   string          `json:"error"`
	Engine  bool            `json:"engine"`
}

func (a *App) toolsSnapshot() ToolsState {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.tools
	t.Ready = map[string]bool{}
	for _, s := range toolSpecs {
		_, err := os.Stat(filepath.Join(a.paths.Tools, s.Name))
		t.Ready[s.Name] = err == nil
	}
	t.Engine = a.aria != nil
	return t
}

func (a *App) setTools(busy string, pct float64, errMsg string) {
	a.mu.Lock()
	a.tools.Busy, a.tools.Percent = busy, pct
	if errMsg != "" || busy != "" {
		a.tools.Error = errMsg
	}
	a.mu.Unlock()
}

func (a *App) toolsLoop() {
	a.refreshTrackers(false)
	for attempt := 0; ; attempt++ {
		missing := false
		for _, s := range toolSpecs {
			if _, err := os.Stat(filepath.Join(a.paths.Tools, s.Name)); err == nil {
				if s.Name == "aria2c.exe" && a.aria == nil {
					a.bootEngine()
				}
				continue
			}
			missing = true
			if err := a.installTool(s); err != nil {
				a.setTools("", 0, s.Title+": "+err.Error())
				continue
			}
			if s.Name == "aria2c.exe" {
				a.bootEngine()
			}
		}
		a.setTools("", 0, "")
		if !missing {
			break
		}
		select {
		case <-a.quit:
			return
		case <-time.After(time.Duration(30*(attempt+1)) * time.Second):
		}
	}
	// раз в сутки — обновление yt-dlp (сайты часто меняются) и списка трекеров
	for {
		a.updateYtdlp()
		a.refreshTrackers(true)
		select {
		case <-a.quit:
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

func (a *App) bootEngine() {
	if a.aria != nil {
		return
	}
	if err := a.startAria2(); err != nil {
		a.setTools("", 0, "Движок: "+err.Error())
		return
	}
	a.resumeOrphans()
}

func (a *App) installTool(s toolSpec) error {
	a.setTools("Скачиваю: "+s.Title, 0, "")
	tmp := filepath.Join(a.paths.Tools, s.Name+".download")
	if err := a.fetch(s.URL, tmp, s.Title); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if len(s.InZip) == 0 {
		return os.Rename(tmp, filepath.Join(a.paths.Tools, s.Name))
	}
	zr, err := zip.OpenReader(tmp)
	if err != nil {
		return err
	}
	defer zr.Close()
	found := 0
	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		for _, want := range s.InZip {
			if !strings.EqualFold(base, want) {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			dst := filepath.Join(a.paths.Tools, want)
			out, err := os.Create(dst + ".new")
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(out, rc)
			rc.Close()
			out.Close()
			if err != nil {
				return err
			}
			_ = os.Remove(dst)
			if err := os.Rename(dst+".new", dst); err != nil {
				return err
			}
			found++
		}
	}
	if found == 0 {
		return errors.New("в архиве нет " + strings.Join(s.InZip, ", "))
	}
	return nil
}

func (a *App) fetch(url, dst, title string) error {
	c := &http.Client{Timeout: 30 * time.Minute}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "MairdiDownloader/1.0")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("сервер ответил " + resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 256<<10)
	lastUI := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			done += int64(n)
			if total > 0 && time.Since(lastUI) > 300*time.Millisecond {
				a.setTools("Скачиваю: "+title, float64(done)*100/float64(total), "")
				lastUI = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

func (a *App) updateYtdlp() {
	yt := a.tool("yt-dlp.exe")
	if yt == "" {
		return
	}
	_ = hiddenCmd(yt, "-U").Run()
}

// Лучшие публичные трекеры — больше пиров, выше скорость торрентов.
func (a *App) refreshTrackers(force bool) {
	p := filepath.Join(a.paths.Data, "trackers.txt")
	if st, err := os.Stat(p); err == nil && !force && time.Since(st.ModTime()) < 24*time.Hour {
		return
	}
	resp, err := httpc.Get("https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best.txt")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err == nil && len(b) > 20 {
		_ = os.WriteFile(p, b, 0o644)
		if a.aria != nil && a.getSettings().Trackers {
			a.aria.call("changeGlobalOption", map[string]any{"bt-tracker": a.trackerList()})
		}
	}
}

func (a *App) trackerList() string {
	b, err := os.ReadFile(filepath.Join(a.paths.Data, "trackers.txt"))
	if err != nil {
		return ""
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, ",")
}
