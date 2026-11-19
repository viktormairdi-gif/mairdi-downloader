package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Второй движок (на Go): многопоточная закачка с докачкой и делением кусков на лету.
// Включается сам, если aria2 не может работать с сервером (обрывы, молчание).

type nSeg struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // включительно
	Done  int64 `json:"done"`
}

type nState struct {
	URL  string  `json:"url"`
	Size int64   `json:"size"`
	Segs []*nSeg `json:"segs"`
}

type nativeRun struct {
	cancel context.CancelFunc
	done   atomic.Int64
	conns  atomic.Int32
}

var nativeClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       0,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

const nativeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

func (a *App) nativeRequest(ctx context.Context, j *Job, u string, from, to int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	ua := j.UserAgent
	if ua == "" {
		ua = nativeUA
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	if j.Referer != "" {
		req.Header.Set("Referer", j.Referer)
	}
	for _, h := range j.Headers {
		if k, v, ok := strings.Cut(h, ":"); ok {
			req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	if j.GAcc != "" && a.gd != nil {
		// токен берётся на каждый запрос — истёкший обновится сам, закачка не прервётся
		tok, err := a.gd.accessToken(j.GAcc)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if from >= 0 {
		if to >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
		}
	}
	return req, nil
}

// Некоторые серверы пускают только «свои» программы (wget/curl) и рвут соединение
// остальным до первого байта. Перебираем представления и запоминаем рабочее.
var altAgents = []string{"Wget/1.21.4", "curl/8.9.1"}

func (a *App) nativeProbe(ctx context.Context, j *Job, u string) (*http.Response, error) {
	req, err := a.nativeRequest(ctx, j, u, 0, 0)
	if err != nil {
		return nil, err
	}
	resp, err := nativeClient.Do(req)
	if err == nil || ctx.Err() != nil || !isEarlyDrop(err) {
		return resp, err
	}
	first := err
	a.mu.Lock()
	orig := j.UserAgent
	a.mu.Unlock()
	for _, ua := range altAgents {
		a.mu.Lock()
		j.UserAgent = ua
		a.mu.Unlock()
		req, _ = a.nativeRequest(ctx, j, u, 0, 0)
		if resp, err = nativeClient.Do(req); err == nil {
			return resp, nil // ua остаётся в задаче — им же качают все потоки и повторы
		}
	}
	a.mu.Lock()
	j.UserAgent = orig
	a.mu.Unlock()
	return nil, first
}

func isEarlyDrop(err error) bool {
	s := err.Error()
	return errors.Is(err, io.EOF) || strings.HasSuffix(s, ": EOF") || strings.Contains(s, "connection reset") || strings.Contains(s, "forcibly closed")
}

func fileNameFromResponse(resp *http.Response, fallback string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, p, err := mime.ParseMediaType(cd); err == nil {
			if n := p["filename"]; n != "" {
				return safeName(n)
			}
		}
	}
	if b := path.Base(resp.Request.URL.Path); b != "" && b != "/" && b != "." {
		if s, err := url.PathUnescape(b); err == nil {
			b = s
		}
		return safeName(b)
	}
	return safeName(fallback)
}

// switchToNative — перевести закачку с aria2 на свой движок (вызывается вне a.mu).
func (a *App) switchToNative(j *Job, reason string) {
	a.mu.Lock()
	gids := []string{}
	var old []string
	for _, p := range j.Parts {
		gids = append(gids, p.Gid)
		old = append(old, p.Path)
	}
	j.Parts = nil
	j.Retries = 0
	j.Engine = "native"
	j.Error = reason
	j.Status = "waiting"
	j.lastMove = time.Time{}
	a.markDirty()
	a.mu.Unlock()
	for _, g := range gids {
		a.aria.remove(g)
	}
	time.Sleep(300 * time.Millisecond)
	for _, p := range old {
		if p != "" {
			_ = os.Remove(p)
			_ = os.Remove(p + ".aria2")
		}
	}
	go a.startNative(j)
}

func (a *App) startNative(j *Job) {
	inc := a.incoming()
	s := a.getSettings()
	a.mu.Lock()
	if j.native != nil || j.Status == "paused" && j.stopReq == "pause" {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &nativeRun{cancel: cancel}
	j.native = run
	j.stopReq = ""
	j.Status = "active"
	u := j.URL
	tmp := j.TmpPath
	conns := s.Connections
	if j.GAcc != "" {
		conns = min(conns, 8) // Google режет слишком много параллельных запросов к одному файлу
	}
	a.mu.Unlock()

	err := a.nativeDownload(ctx, j, run, u, inc, tmp, conns)

	a.mu.Lock()
	j.native = nil
	stop := j.stopReq
	j.Speed, j.ETA, j.Connections = 0, 0, 0
	tmp = j.TmpPath
	a.mu.Unlock()
	switch {
	case stop == "pause":
		a.mu.Lock()
		j.Status = "paused"
		a.markDirty()
		a.mu.Unlock()
	case stop != "":
		return
	case errors.Is(err, context.Canceled):
		// пауза и сразу «Старт» — запускаемся заново с сохранённого места
		go a.startNative(j)
	case err != nil:
		a.mu.Lock()
		if j.Retries < 50 {
			j.Retries++
			wait := retryWait(j.Retries)
			j.Status, j.RetryAt = "retrying", time.Now().Add(wait)
			j.Error = err.Error() + " — повтор через " + strconv.Itoa(int(wait.Seconds())) + " с"
		} else {
			j.Status, j.Error = "error", err.Error()
		}
		a.markDirty()
		a.mu.Unlock()
	default:
		a.mu.Lock()
		j.Status = "moving"
		a.mu.Unlock()
		_ = os.Remove(tmp + ".mdstate")
		a.finishFile(j, tmp, "")
	}
}

func (a *App) nativeDownload(ctx context.Context, j *Job, run *nativeRun, u, inc, tmp string, conns int) error {
	// 1. Узнаём размер, имя и поддержку докачки (Range).
	var st nState
	if tmp != "" {
		loadJSON(tmp+".mdstate", &st)
	}
	if tmp == "" || st.Size == 0 {
		resp, err := a.nativeProbe(ctx, j, u)
		if err != nil {
			return friendlyNetErr(err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("сервер ответил %s", resp.Status)
		}
		size := int64(-1)
		ranges := resp.StatusCode == http.StatusPartialContent
		if ranges {
			if cr := resp.Header.Get("Content-Range"); cr != "" {
				if i := strings.LastIndex(cr, "/"); i >= 0 {
					size, _ = strconv.ParseInt(cr[i+1:], 10, 64)
				}
			}
		} else if resp.ContentLength > 0 {
			size = resp.ContentLength
		}
		a.mu.Lock()
		name := j.FileName
		a.mu.Unlock()
		if name == "" {
			name = fileNameFromResponse(resp, "file")
		}
		if tmp == "" {
			tmp = uniquePath(filepath.Join(inc, name))
		}
		st = nState{URL: u, Size: size}
		if ranges && size > 0 {
			n := int64(conns)
			if size < 4<<20 {
				n = 1
			}
			chunk := size / n
			for i := int64(0); i < n; i++ {
				end := (i+1)*chunk - 1
				if i == n-1 {
					end = size - 1
				}
				st.Segs = append(st.Segs, &nSeg{Start: i * chunk, End: end})
			}
		}
		a.mu.Lock()
		j.TmpPath, j.Total, j.Name = tmp, max(size, 0), filepath.Base(tmp)
		a.mu.Unlock()
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Нет докачки у сервера — один поток с начала.
	if len(st.Segs) == 0 {
		_ = f.Truncate(0)
		return a.nativeStream(ctx, j, run, f, u, st.Size)
	}
	if fi, _ := f.Stat(); fi != nil && fi.Size() != st.Size {
		_ = f.Truncate(st.Size)
	}

	// 2. Многопоточная закачка: освободившийся поток делит самый большой оставшийся кусок.
	var mu sync.Mutex
	var total int64
	for _, s := range st.Segs {
		total += s.Done
	}
	run.done.Store(total)
	save := func() {
		mu.Lock()
		defer mu.Unlock()
		_ = saveJSON(tmp+".mdstate", st)
	}
	next := func() *nSeg {
		mu.Lock()
		defer mu.Unlock()
		var best *nSeg
		for _, s := range st.Segs {
			if rem := s.End - s.Start + 1 - s.Done; rem > 0 && (best == nil || rem > best.End-best.Start+1-best.Done) {
				best = s
			}
		}
		if best == nil {
			return nil
		}
		rem := best.End - best.Start + 1 - best.Done
		if rem < 2<<20 {
			return nil // маленький хвост докачает его же поток
		}
		mid := best.Start + best.Done + rem/2
		ns := &nSeg{Start: mid, End: best.End}
		best.End = mid - 1
		st.Segs = append(st.Segs, ns)
		return ns
	}
	work := func(s *nSeg) error {
		var lastErr error
		for attempt := 0; ; attempt++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			mu.Lock()
			from, to := s.Start+s.Done, s.End
			mu.Unlock()
			if from > to {
				return nil
			}
			err := a.nativeRange(ctx, j, run, f, u, s, &mu)
			if err == nil {
				mu.Lock()
				left := s.End - s.Start + 1 - s.Done
				mu.Unlock()
				if left <= 0 {
					return nil
				}
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			if attempt >= 8 {
				return lastErr
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			}
		}
	}
	errCh := make(chan error, 64)
	var wg sync.WaitGroup
	worker := func(s *nSeg) {
		defer wg.Done()
		run.conns.Add(1)
		defer run.conns.Add(-1)
		for s != nil {
			if err := work(s); err != nil {
				errCh <- err
				return
			}
			s = next()
		}
	}
	mu.Lock()
	initial := append([]*nSeg(nil), st.Segs...)
	mu.Unlock()
	started := 0
	for _, s := range initial {
		if s.End-s.Start+1-s.Done > 0 && started < max(conns, 1) {
			wg.Add(1)
			started++
			go worker(s)
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	var firstErr error
	for {
		select {
		case <-finished:
			save()
			if firstErr != nil {
				return firstErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// всё ли докачано
			mu.Lock()
			var left int64
			for _, s := range st.Segs {
				left += s.End - s.Start + 1 - s.Done
			}
			mu.Unlock()
			if left > 0 {
				return errors.New("закачка прервалась")
			}
			return nil
		case err := <-errCh:
			if firstErr == nil && !errors.Is(err, context.Canceled) {
				firstErr = err
			}
		case <-tick.C:
			save()
			a.nativeProgress(j, run)
		}
	}
}

func (a *App) nativeProgress(j *Job, run *nativeRun) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := run.done.Load()
	if d > j.Done && strings.Contains(j.Error, "повтор через") {
		j.Error = "" // данные пошли — старая надпись о повторе больше не нужна
	}
	sp := d - j.Done
	if sp < 0 {
		sp = 0
	}
	j.Speed = (j.Speed + sp) / 2
	if j.Speed == 0 {
		j.Speed = sp
	}
	j.Done = d
	j.Connections = int(run.conns.Load())
	j.ETA = 0
	if j.Speed > 0 && j.Total > j.Done {
		j.ETA = (j.Total - j.Done) / j.Speed
	}
	if j.Done > j.lastDone {
		a.sawWork = true
	}
}

// Один кусок. Молчание сервера дольше 30 с — обрыв и повтор.
func (a *App) nativeRange(ctx context.Context, j *Job, run *nativeRun, f *os.File, u string, s *nSeg, mu *sync.Mutex) error {
	mu.Lock()
	from, to := s.Start+s.Done, s.End
	mu.Unlock()
	if from > to {
		return nil
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := a.nativeRequest(rctx, j, u, from, to)
	if err != nil {
		return err
	}
	resp, err := nativeClient.Do(req)
	if err != nil {
		return friendlyNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("сервер ответил %s", resp.Status)
	}
	idle := time.AfterFunc(30*time.Second, cancel)
	defer idle.Stop()
	buf := make([]byte, 256<<10)
	off := from
	for {
		mu.Lock()
		limit := s.End // кусок могли поделить на лету
		mu.Unlock()
		if off > limit {
			return nil
		}
		want := int64(len(buf))
		if rem := limit - off + 1; rem < want {
			want = rem
		}
		n, rerr := resp.Body.Read(buf[:want])
		if n > 0 {
			idle.Reset(30 * time.Second)
			if _, werr := f.WriteAt(buf[:n], off); werr != nil {
				return werr
			}
			off += int64(n)
			mu.Lock()
			s.Done += int64(n)
			mu.Unlock()
			run.done.Add(int64(n))
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			if rctx.Err() != nil && ctx.Err() == nil {
				return errors.New("сервер замолчал — переподключаюсь")
			}
			return friendlyNetErr(rerr)
		}
	}
}

// Сервер без докачки — один поток.
func (a *App) nativeStream(ctx context.Context, j *Job, run *nativeRun, f *os.File, u string, size int64) error {
	req, err := a.nativeRequest(ctx, j, u, -1, -1)
	if err != nil {
		return err
	}
	resp, err := nativeClient.Do(req)
	if err != nil {
		return friendlyNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("сервер ответил %s", resp.Status)
	}
	run.conns.Store(1)
	run.done.Store(0)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				a.nativeProgress(j, run)
			}
		}
	}()
	buf := make([]byte, 256<<10)
	var off int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], off); werr != nil {
				return werr
			}
			off += int64(n)
			run.done.Store(off)
		}
		if rerr == io.EOF {
			if size > 0 && off < size {
				return errors.New("сервер оборвал передачу")
			}
			return nil
		}
		if rerr != nil {
			return friendlyNetErr(rerr)
		}
	}
}

func friendlyNetErr(err error) error {
	s := err.Error()
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case strings.Contains(s, "no such host"):
		return errors.New("Не удалось найти сервер (DNS)")
	case strings.Contains(s, "failed to respond") || strings.Contains(s, "connectex") || strings.Contains(s, "connection refused"):
		return errors.New("Сервер не отвечает")
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline"):
		return errors.New("Время ожидания истекло")
	case strings.Contains(s, "connection reset") || strings.Contains(s, "forcibly closed") || strings.Contains(s, "EOF"):
		return errors.New("Сервер оборвал соединение")
	case strings.Contains(s, "certificate"):
		return errors.New("Ошибка сертификата сайта")
	}
	return errors.New("Ошибка сети: " + s)
}
