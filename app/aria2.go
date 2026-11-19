package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Aria2 — движок закачек: HTTP/HTTPS/FTP в 16 потоков, BitTorrent, magnet, докачка.
type Aria2 struct {
	port    int
	secret  string
	cmd     *exec.Cmd
	client  *http.Client
	session string
	exited  chan struct{}
}

type aFile struct {
	Path            string `json:"path"`
	Length          string `json:"length"`
	CompletedLength string `json:"completedLength"`
	Selected        string `json:"selected"`
	URIs            []struct {
		URI string `json:"uri"`
	} `json:"uris"`
}

type aStatus struct {
	Gid             string   `json:"gid"`
	Status          string   `json:"status"`
	TotalLength     string   `json:"totalLength"`
	CompletedLength string   `json:"completedLength"`
	UploadLength    string   `json:"uploadLength"`
	DownloadSpeed   string   `json:"downloadSpeed"`
	UploadSpeed     string   `json:"uploadSpeed"`
	Connections     string   `json:"connections"`
	NumSeeders      string   `json:"numSeeders"`
	ErrorCode       string   `json:"errorCode"`
	ErrorMessage    string   `json:"errorMessage"`
	FollowedBy      []string `json:"followedBy"`
	Dir             string   `json:"dir"`
	Bitfield        string   `json:"bitfield"`
	NumPieces       string   `json:"numPieces"`
	InfoHash        string   `json:"infoHash"`
	Files           []aFile  `json:"files"`
	Bittorrent      *struct {
		Info *struct {
			Name string `json:"name"`
		} `json:"info"`
	} `json:"bittorrent"`
}

var statusKeys = []string{"gid", "status", "totalLength", "completedLength", "uploadLength", "downloadSpeed",
	"uploadSpeed", "connections", "numSeeders", "errorCode", "errorMessage", "followedBy", "dir",
	"bitfield", "numPieces", "infoHash", "files", "bittorrent"}

func atoi64(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 6800
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) startAria2() error {
	exe := filepath.Join(a.paths.Tools, "aria2c.exe")
	if _, err := os.Stat(exe); err != nil {
		return errors.New("aria2c.exe не установлен")
	}
	s := a.getSettings()
	ar := &Aria2{port: freePort(), secret: randHex(16), client: &http.Client{Timeout: 15 * time.Second},
		session: filepath.Join(a.paths.Data, "aria2.session")}
	if _, err := os.Stat(ar.session); err != nil {
		_ = os.WriteFile(ar.session, nil, 0o600)
	}
	conns := strconv.Itoa(s.Connections)
	args := []string{
		"--enable-rpc=true", "--rpc-listen-all=false", "--rpc-listen-port=" + strconv.Itoa(ar.port),
		"--rpc-secret=" + ar.secret, "--rpc-max-request-size=64M",
		"--input-file=" + ar.session, "--save-session=" + ar.session, "--save-session-interval=15",
		"--dir=" + a.incoming(),
		// скорость
		"--max-connection-per-server=" + conns, "--split=" + conns, "--min-split-size=1M",
		"--max-concurrent-downloads=" + strconv.Itoa(s.MaxActive),
		"--uri-selector=adaptive", "--stream-piece-selector=geom",
		"--disk-cache=64M", "--file-allocation=trunc", "--piece-length=1M",
		// надёжность и докачка
		"--continue=true", "--always-resume=true", "--max-tries=0", "--retry-wait=5",
		"--timeout=30", "--connect-timeout=15", "--max-file-not-found=3",
		"--auto-file-renaming=true", "--allow-overwrite=false", "--remote-time=true",
		"--content-disposition-default-utf8=true", "--check-certificate=true",
		"--auto-save-interval=5",
		"--user-agent=Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
		// торренты
		"--enable-dht=true", "--enable-dht6=true", "--bt-enable-lpd=true", "--enable-peer-exchange=true",
		"--bt-max-peers=128", "--bt-request-peer-speed-limit=10M", "--bt-detach-seed-only=true",
		"--bt-save-metadata=true", "--bt-load-saved-metadata=true", "--pause-metadata=true",
		"--follow-torrent=true", "--bt-remove-unselected-file=true",
		"--seed-ratio=" + strconv.FormatFloat(s.SeedRatio, 'f', 2, 64),
		"--bt-require-crypto=false", "--bt-min-crypto-level=plain",
		"--listen-port=51413-51423", "--dht-listen-port=51413-51423",
		"--dht-file-path=" + filepath.Join(a.paths.Data, "dht.dat"),
		"--dht-file-path6=" + filepath.Join(a.paths.Data, "dht6.dat"),
		"--max-overall-download-limit=" + strconv.Itoa(a.speedLimitKB(s)) + "K",
		"--max-overall-upload-limit=" + strconv.Itoa(s.UploadLimitKB) + "K",
		"--console-log-level=warn", "--summary-interval=0",
	}
	if tr := a.trackerList(); tr != "" && s.Trackers {
		args = append(args, "--bt-tracker="+tr)
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.Dir = a.paths.Data
	if err := cmd.Start(); err != nil {
		return err
	}
	ar.cmd = cmd
	ar.exited = make(chan struct{})
	go func() { _ = cmd.Wait(); close(ar.exited) }()
	attachToKillJob(cmd.Process)
	// Движок отдаём остальным только когда он уже отвечает: закачка, перехваченная
	// в первую секунду после автозапуска, иначе падала с «connection refused».
	for i := 0; i < 100; i++ {
		if _, err := ar.call("getVersion"); err == nil {
			a.aria = ar
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	a.aria = ar
	return errors.New("aria2 не отвечает")
}

func (ar *Aria2) call(method string, params ...any) (json.RawMessage, error) {
	if ar == nil {
		return nil, errors.New("движок не запущен")
	}
	p := append([]any{"token:" + ar.secret}, params...)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "m", "method": "aria2." + method, "params": p})
	resp, err := ar.client.Post("http://127.0.0.1:"+strconv.Itoa(ar.port)+"/jsonrpc", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, fmt.Errorf("aria2: %s", r.Error.Message)
	}
	return r.Result, nil
}

func (ar *Aria2) addURI(uris []string, opts map[string]any) (string, error) {
	r, err := ar.call("addUri", uris, opts)
	if err != nil {
		return "", err
	}
	var gid string
	err = json.Unmarshal(r, &gid)
	return gid, err
}

func (ar *Aria2) addTorrent(b64 string, opts map[string]any) (string, error) {
	r, err := ar.call("addTorrent", b64, []string{}, opts)
	if err != nil {
		return "", err
	}
	var gid string
	err = json.Unmarshal(r, &gid)
	return gid, err
}

func (ar *Aria2) list() ([]aStatus, error) {
	var all []aStatus
	for _, m := range []struct {
		name string
		args []any
	}{{"tellActive", []any{statusKeys}}, {"tellWaiting", []any{0, 10000, statusKeys}}, {"tellStopped", []any{0, 10000, statusKeys}}} {
		r, err := ar.call(m.name, m.args...)
		if err != nil {
			return nil, err
		}
		var part []aStatus
		if err := json.Unmarshal(r, &part); err != nil {
			return nil, err
		}
		all = append(all, part...)
	}
	return all, nil
}

func (ar *Aria2) status(gid string) (*aStatus, error) {
	r, err := ar.call("tellStatus", gid, statusKeys)
	if err != nil {
		return nil, err
	}
	var s aStatus
	err = json.Unmarshal(r, &s)
	return &s, err
}

func (ar *Aria2) remove(gid string) {
	_, _ = ar.call("forceRemove", gid)
	time.Sleep(50 * time.Millisecond)
	_, _ = ar.call("removeDownloadResult", gid)
}

func (ar *Aria2) shutdown() {
	if ar == nil {
		return
	}
	_, _ = ar.call("saveSession")
	_, _ = ar.call("shutdown")
	select {
	case <-ar.exited:
	case <-time.After(4 * time.Second):
		_ = ar.cmd.Process.Kill()
	}
}

// Коды ошибок aria2, после которых имеет смысл повторить (сеть, таймаут, сервер занят).
func retryableAria(code string) bool {
	switch code {
	case "1", "2", "6", "8", "19", "22", "23", "29", "30":
		return true
	}
	return false
}

func humanAriaError(code, msg string) string {
	m := map[string]string{
		"2":  "Время ожидания истекло",
		"3":  "Файл не найден на сервере (404)",
		"6":  "Ошибка сети",
		"9":  "Недостаточно места на диске",
		"13": "Файл уже существует",
		"16": "Не удалось создать файл",
		"19": "Не удалось найти сервер (DNS)",
		"22": "Сервер вернул ошибку",
		"24": "Нужна авторизация на сайте",
		"29": "Сервер перегружен",
	}
	if t, ok := m[code]; ok {
		return t
	}
	if msg != "" {
		return msg
	}
	return "Ошибка загрузки (код " + code + ")"
}

func (ar *Aria2) dead() bool {
	select {
	case <-ar.exited:
		return true
	default:
		return false
	}
}

func cleanAriaPath(p string) string { return strings.ReplaceAll(p, "/", `\`) }
