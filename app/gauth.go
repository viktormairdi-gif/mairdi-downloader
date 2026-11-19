package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Вход в Google: OAuth 2.0 для настольных программ (loopback + PKCE).
// Пароль программа не видит; refresh-токен хранится зашифрованным DPAPI (только этот пользователь Windows).

var (
	gAuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	gTokenURL  = "https://oauth2.googleapis.com/token"
	gRevokeURL = "https://oauth2.googleapis.com/revoke"
	gAPI       = "https://www.googleapis.com"
)

const gScope = "https://www.googleapis.com/auth/drive"

type GAccount struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	Photo      string `json:"photo"`
	RefreshEnc string `json:"refreshEnc"` // DPAPI + base64
}

type GConfig struct {
	ClientID     string      `json:"clientId"`
	ClientSecret string      `json:"clientSecret"`
	Accounts     []*GAccount `json:"accounts"`
}

type gToken struct {
	access string
	exp    time.Time
}

type GDrive struct {
	path    string
	mu      sync.Mutex
	cfg     GConfig
	tokens  map[string]*gToken
	refresh sync.Mutex // одно обновление токена за раз
	login   string     // идёт вход: "" | "wait" | текст ошибки
	authURL string     // ссылка входа, пока он идёт (открыть ещё раз / скопировать)
	cancel  chan struct{}
	client  *http.Client
}

func newGDrive(dataDir string) *GDrive {
	g := &GDrive{path: filepath.Join(dataDir, "gdrive.json"), tokens: map[string]*gToken{},
		client: &http.Client{Timeout: 60 * time.Second}}
	loadJSON(g.path, &g.cfg)
	return g
}

func (g *GDrive) save() { _ = saveJSON(g.path, g.cfg) }

func (g *GDrive) configured() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cfg.ClientID != "" && g.cfg.ClientSecret != ""
}

type GAccountInfo struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Photo string `json:"photo"`
}

func (g *GDrive) accounts() []GAccountInfo {
	out := []GAccountInfo{}
	if g == nil {
		return out
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.cfg.Accounts {
		out = append(out, GAccountInfo{a.Email, a.Name, a.Photo})
	}
	return out
}

func (g *GDrive) loginState() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.login
}

// setClient принимает Client ID + Secret или целиком JSON-файл, скачанный из Google Cloud.
func (g *GDrive) setClient(id, secret string) error {
	id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
	if strings.HasPrefix(id, "{") {
		var f map[string]struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		if err := json.Unmarshal([]byte(id), &f); err != nil {
			return errors.New("не похоже на файл ключа Google (JSON)")
		}
		if _, ok := f["web"]; ok {
			return errors.New("это ключ типа «Веб-приложение» — нужен тип «Приложение для компьютера» (Desktop app)")
		}
		c, ok := f["installed"]
		if !ok {
			return errors.New("в файле нет раздела installed — создайте клиент типа «Приложение для компьютера»")
		}
		id, secret = c.ClientID, c.ClientSecret
	}
	if !strings.HasSuffix(id, ".apps.googleusercontent.com") {
		return errors.New("Client ID должен заканчиваться на .apps.googleusercontent.com")
	}
	if secret == "" {
		return errors.New("укажите Client Secret")
	}
	g.mu.Lock()
	g.cfg.ClientID, g.cfg.ClientSecret = id, secret
	g.mu.Unlock()
	g.save()
	return nil
}

// ---------- шифрование токенов (DPAPI) ----------

func dpapiProtect(b []byte) (string, error) {
	if len(b) == 0 {
		return "", errors.New("пусто")
	}
	in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return "", err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return base64.StdEncoding.EncodeToString(unsafe.Slice(out.Data, out.Size)), nil
}

func dpapiUnprotect(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("повреждён ключ доступа")
	}
	in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// ---------- вход ----------

type gTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (g *GDrive) tokenRequest(v url.Values) (*gTokenResp, error) {
	g.mu.Lock()
	v.Set("client_id", g.cfg.ClientID)
	v.Set("client_secret", g.cfg.ClientSecret)
	g.mu.Unlock()
	resp, err := g.client.PostForm(gTokenURL, v)
	if err != nil {
		return nil, friendlyNetErr(err)
	}
	defer resp.Body.Close()
	var t gTokenResp
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t)
	if t.Error != "" || resp.StatusCode != 200 {
		switch t.Error {
		case "invalid_grant":
			return nil, errors.New("Google отозвал доступ — подключите аккаунт заново")
		case "invalid_client", "unauthorized_client":
			return nil, errors.New("Google не принял ключ приложения (Client ID / Secret)")
		}
		return nil, fmt.Errorf("Google: %s %s", firstNonEmpty(t.Error, resp.Status), t.ErrorDesc)
	}
	return &t, nil
}

func pkcePair() (verifier, challenge string) {
	verifier = randHex(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

const gDonePage = `<!doctype html><meta charset="utf-8"><title>MAIRDI Загрузчик</title>
<body style="font:16px Segoe UI,sans-serif;display:flex;align-items:center;justify-content:center;height:90vh;background:#F4F5F7">
<div style="background:#fff;padding:32px 40px;border-radius:14px;box-shadow:0 10px 30px rgba(0,0,0,.08);text-align:center">
<div style="font-size:42px;color:#F58735">%s</div><h2>%s</h2><p style="color:#6B7480">%s</p></div>`

// startLogin открывает браузер со входом Google и ждёт ответа (до 5 минут) в фоне.
func (g *GDrive) startLogin(onDone func(email string, err error)) error {
	if !g.configured() {
		return errors.New("сначала укажите ключ приложения Google")
	}
	g.mu.Lock()
	if g.login == "wait" {
		g.mu.Unlock()
		return errors.New("вход уже идёт — завершите его в браузере")
	}
	g.login = "wait"
	clientID := g.cfg.ClientID
	cancel := make(chan struct{})
	g.cancel = cancel
	g.mu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		g.setLogin(err.Error())
		return err
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	verifier, challenge := pkcePair()
	state := randHex(12)
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {gScope},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state},
		"access_type": {"offline"}, "prompt": {"consent select_account"},
	}
	type result struct {
		code string
		err  error
	}
	ch := make(chan result, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		qq := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if qq.Get("state") != state {
			http.Error(w, "bad state", 400)
			return
		}
		if e := qq.Get("error"); e != "" {
			fmt.Fprintf(w, gDonePage, "✕", "Доступ не выдан", "Можно закрыть вкладку и попробовать ещё раз.")
			select {
			case ch <- result{err: errors.New("вход отменён в браузере")}:
			default:
			}
			return
		}
		fmt.Fprintf(w, gDonePage, "✓", "Google Диск подключён", "Можно закрыть эту вкладку и вернуться в Загрузчик.")
		select {
		case ch <- result{code: qq.Get("code")}:
		default:
		}
	})
	go func() { _ = srv.Serve(ln) }()
	link := gAuthURL + "?" + q.Encode()
	g.mu.Lock()
	g.authURL = link
	g.mu.Unlock()
	openInBrowser(link)

	go func() {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = srv.Shutdown(ctx)
			cancel()
		}()
		var res result
		select {
		case res = <-ch:
		case <-time.After(10 * time.Minute):
			res.err = errors.New("время на вход истекло")
		case <-cancel:
			res.err = errors.New("вход отменён")
		}
		g.mu.Lock()
		g.authURL, g.cancel = "", nil
		g.mu.Unlock()
		if res.err != nil {
			g.setLogin(res.err.Error())
			onDone("", res.err)
			return
		}
		email, err := g.finishLogin(res.code, verifier, redirect)
		if err != nil {
			g.setLogin(err.Error())
		} else {
			g.setLogin("")
		}
		onDone(email, err)
	}()
	return nil
}

// Вход открываем в Chrome — там обычно уже выполнен вход в Google; иначе в браузере по умолчанию.
func openInBrowser(link string) {
	for _, p := range []string{
		getRegString(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`, ""),
		getRegString(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`, ""),
		os.Getenv("ProgramFiles") + `\Google\Chrome\Application\chrome.exe`,
		os.Getenv("LOCALAPPDATA") + `\Google\Chrome\Application\chrome.exe`,
	} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			if exec.Command(p, link).Start() == nil {
				return
			}
		}
	}
	shellOpen(link)
}

func (g *GDrive) loginLink() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authURL
}

func (g *GDrive) reopenLogin() bool {
	if l := g.loginLink(); l != "" {
		openInBrowser(l)
		return true
	}
	return false
}

func (g *GDrive) cancelLogin() {
	g.mu.Lock()
	c := g.cancel
	g.cancel = nil
	g.mu.Unlock()
	if c != nil {
		close(c)
	}
}

func (g *GDrive) setLogin(s string) {
	g.mu.Lock()
	g.login = s
	g.mu.Unlock()
}

func (g *GDrive) finishLogin(code, verifier, redirect string) (string, error) {
	t, err := g.tokenRequest(url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {verifier}, "redirect_uri": {redirect}})
	if err != nil {
		return "", err
	}
	if t.RefreshToken == "" {
		return "", errors.New("Google не выдал постоянный доступ — удалите доступ программы в настройках аккаунта Google и подключите заново")
	}
	enc, err := dpapiProtect([]byte(t.RefreshToken))
	if err != nil {
		return "", err
	}
	tmp := &gToken{access: t.AccessToken, exp: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)}
	var about struct {
		User struct {
			DisplayName  string `json:"displayName"`
			EmailAddress string `json:"emailAddress"`
			PhotoLink    string `json:"photoLink"`
		} `json:"user"`
	}
	if err := g.callWith(tmp.access, "GET", "/drive/v3/about", url.Values{"fields": {"user"}}, nil, &about); err != nil {
		return "", err
	}
	email := strings.ToLower(about.User.EmailAddress)
	if email == "" {
		return "", errors.New("Google не сообщил адрес аккаунта")
	}
	g.mu.Lock()
	var acc *GAccount
	for _, a := range g.cfg.Accounts {
		if a.Email == email {
			acc = a
		}
	}
	if acc == nil {
		acc = &GAccount{Email: email}
		g.cfg.Accounts = append(g.cfg.Accounts, acc)
	}
	acc.Name, acc.Photo, acc.RefreshEnc = about.User.DisplayName, about.User.PhotoLink, enc
	g.tokens[email] = tmp
	g.mu.Unlock()
	g.save()
	return email, nil
}

func (g *GDrive) disconnect(email string) {
	g.mu.Lock()
	var enc string
	var keep []*GAccount
	for _, a := range g.cfg.Accounts {
		if a.Email == email {
			enc = a.RefreshEnc
			continue
		}
		keep = append(keep, a)
	}
	g.cfg.Accounts = keep
	delete(g.tokens, email)
	g.mu.Unlock()
	g.save()
	if enc != "" {
		if rt, err := dpapiUnprotect(enc); err == nil {
			go func() { _, _ = g.client.PostForm(gRevokeURL, url.Values{"token": {string(rt)}}) }()
		}
	}
}

// accessToken — действующий токен аккаунта; обновляется сам за минуту до истечения.
func (g *GDrive) accessToken(email string) (string, error) {
	g.mu.Lock()
	if t := g.tokens[email]; t != nil && time.Until(t.exp) > time.Minute {
		g.mu.Unlock()
		return t.access, nil
	}
	g.mu.Unlock()
	g.refresh.Lock()
	defer g.refresh.Unlock()
	g.mu.Lock()
	if t := g.tokens[email]; t != nil && time.Until(t.exp) > time.Minute {
		g.mu.Unlock()
		return t.access, nil
	}
	var enc string
	for _, a := range g.cfg.Accounts {
		if a.Email == email {
			enc = a.RefreshEnc
		}
	}
	g.mu.Unlock()
	if enc == "" {
		return "", errors.New("аккаунт Google " + email + " не подключён")
	}
	rt, err := dpapiUnprotect(enc)
	if err != nil {
		return "", errors.New("не удалось расшифровать доступ к Google — подключите аккаунт заново")
	}
	t, err := g.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {string(rt)}})
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.tokens[email] = &gToken{access: t.AccessToken, exp: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)}
	g.mu.Unlock()
	return t.AccessToken, nil
}

// ---------- вызовы Drive API ----------

type gAPIError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

func (g *GDrive) callWith(token, method, path string, q url.Values, body any, out any) error {
	u := gAPI + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(method, u, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err = g.client.Do(req)
		if err != nil {
			return friendlyNetErr(err)
		}
		// лимит запросов Google — короткая пауза и повтор
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < 3 && body == nil {
			resp.Body.Close()
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		break
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e gAPIError
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		return humanGError(resp.StatusCode, e)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func humanGError(code int, e gAPIError) error {
	reason := ""
	if len(e.Error.Errors) > 0 {
		reason = e.Error.Errors[0].Reason
	}
	switch {
	case code == 401:
		return errors.New("Google не принял доступ — подключите аккаунт заново")
	case reason == "accessNotConfigured" || strings.Contains(e.Error.Message, "has not been used") || strings.Contains(e.Error.Message, "is disabled"):
		return errors.New("в Google Cloud не включён Google Drive API — включите его в проекте")
	case code == 404:
		return errors.New("файл не найден или нет доступа")
	case reason == "cannotDownloadAbusiveFile":
		return errors.New("Google пометил файл как вредоносный и не отдаёт его")
	case reason == "exportSizeLimitExceeded":
		return errors.New("документ слишком большой для выгрузки")
	case reason == "downloadQuotaExceeded":
		return errors.New("превышен лимит скачиваний этого файла у Google — попробуйте позже")
	case code == 403 && strings.Contains(reason, "ateLimit"):
		return errors.New("Google временно ограничил частоту запросов — повторю")
	case code == 403 && reason == "storageQuotaExceeded":
		return errors.New("на Диске закончилось место")
	case code == 403 && (reason == "insufficientFilePermissions" || reason == "forbidden" || e.Error.Message == ""):
		return errors.New("нет прав на это действие (файл не ваш или доступ только на чтение)")
	case code == 403:
		return errors.New("нет прав на это действие: " + e.Error.Message)
	}
	if e.Error.Message != "" {
		return errors.New("Google: " + e.Error.Message)
	}
	return fmt.Errorf("Google ответил %d", code)
}

func (g *GDrive) call(email, method, path string, q url.Values, body any, out any) error {
	tok, err := g.accessToken(email)
	if err != nil {
		return err
	}
	return g.callWith(tok, method, path, q, body, out)
}
