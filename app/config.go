package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const AppName = "MAIRDI Загрузчик"
const AppID = "MairdiDownloader"
const NativeHostName = "kg.mairdi.downloader"

type Paths struct {
	Exe      string
	Data     string
	Tools    string
	Cookies  string
	WebView  string
	Metadata string
}

func initPaths() Paths {
	exe, _ := os.Executable()
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base, _ = os.UserConfigDir()
	}
	data := filepath.Join(base, AppID)
	p := Paths{
		Exe:      exe,
		Data:     data,
		Tools:    filepath.Join(data, "tools"),
		Cookies:  filepath.Join(data, "cookies"),
		WebView:  filepath.Join(data, "webview"),
		Metadata: filepath.Join(data, "metadata"),
	}
	for _, d := range []string{p.Data, p.Tools, p.Cookies, p.WebView, p.Metadata} {
		_ = os.MkdirAll(d, 0o755)
	}
	return p
}

type Settings struct {
	Root          string   `json:"root"`
	MaxActive     int      `json:"maxActive"`
	Connections   int      `json:"connections"`
	SpeedMode     string   `json:"speedMode"` // max | mid | low
	MidKB         int      `json:"midKB"`
	LowKB         int      `json:"lowKB"`
	UploadLimitKB int      `json:"uploadLimitKB"`
	SeedRatio     float64  `json:"seedRatio"`
	Intercept     bool     `json:"intercept"`
	ExcludedSites []string `json:"excludedSites"`
	VideoQuality  string   `json:"videoQuality"` // best | 2160 | 1080 | 720 | 480 | audio
	Autostart     bool     `json:"autostart"`
	Clipboard     bool     `json:"clipboard"`
	AutoSortRoot  bool     `json:"autoSortRoot"`
	AutoSortSince int64    `json:"autoSortSince"`
	Trackers      bool     `json:"trackers"`
	SkipDupes     bool     `json:"skipDupes"`
	// «Скачать в Google Диск»
	DriveAcc       string `json:"driveAcc"`       // аккаунт по умолчанию ("" — первый подключённый)
	DriveFolder    string `json:"driveFolder"`    // папка на Диске, внутри — та же раскладка
	DriveKeepLocal bool   `json:"driveKeepLocal"` // оставлять копию на компьютере
	// не сохраняется между запусками
	ShutdownWhenDone bool `json:"shutdownWhenDone"`
}

func defaultDownloadsDir() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0); err == nil && p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Downloads")
}

func defaultSettings() Settings {
	return Settings{
		Root:          defaultDownloadsDir(),
		MaxActive:     5,
		Connections:   16,
		SpeedMode:     "max",
		MidKB:         2048,
		LowKB:         256,
		SeedRatio:     1.0,
		Intercept:     true,
		VideoQuality:  "1080",
		Autostart:     true,
		Clipboard:     true,
		AutoSortRoot:  true,
		AutoSortSince: time.Now().Unix(),
		Trackers:      true,
		SkipDupes:     true,
		DriveFolder:   "Загрузки",
	}
}

func (s *Settings) normalize() {
	d := defaultSettings()
	if s.Root == "" {
		s.Root = d.Root
	}
	if s.MaxActive < 1 || s.MaxActive > 20 {
		s.MaxActive = d.MaxActive
	}
	if s.Connections < 1 || s.Connections > 16 {
		s.Connections = d.Connections
	}
	if s.SpeedMode != "mid" && s.SpeedMode != "low" {
		s.SpeedMode = "max"
	}
	if s.MidKB <= 0 {
		s.MidKB = d.MidKB
	}
	if s.LowKB <= 0 {
		s.LowKB = d.LowKB
	}
	if s.SeedRatio < 0 {
		s.SeedRatio = 0
	}
	switch s.VideoQuality {
	case "best", "2160", "1080", "720", "480", "audio":
	default:
		s.VideoQuality = d.VideoQuality
	}
	var segs []string
	for _, p := range strings.FieldsFunc(s.DriveFolder, func(r rune) bool { return r == '/' || r == '\\' }) {
		if p = strings.Trim(p, " ."); p != "" {
			segs = append(segs, safeName(p))
		}
	}
	s.DriveFolder = strings.Join(segs, "/")
	if s.DriveFolder == "" {
		s.DriveFolder = "Загрузки"
	}
	if s.AutoSortSince == 0 {
		s.AutoSortSince = time.Now().Unix()
	}
	var ex []string
	for _, e := range s.ExcludedSites {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			ex = append(ex, e)
		}
	}
	s.ExcludedSites = ex
}

func loadJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

func saveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// saveJSONCompact — для больших файлов (jobs.json), где отступы только раздувают запись.
func saveJSONCompact(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Папка для недокачанных файлов. Если корень — облачная папка (Google Диск, OneDrive,
// Яндекс.Диск), недокачанные части держим локально, чтобы облако не синхронизировало мусор.
func incomingDir(root, data string) string {
	l := strings.ToLower(root)
	for _, k := range []string{"google drive", "googledrive", "мой диск", "my drive", "onedrive", "yandexdisk", "яндекс.диск", "dropbox"} {
		if strings.Contains(l, k) {
			return filepath.Join(data, "incoming")
		}
	}
	return filepath.Join(root, ".Загружается")
}
