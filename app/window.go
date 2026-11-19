package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"fyne.io/systray"
	webview2 "github.com/jchv/go-webview2"
)

// windowCtl — окно программы. Закрытие окна не выключает программу: она живёт в трее.
type windowCtl struct {
	mu       sync.Mutex
	wv       webview2.WebView
	req      chan struct{}
	quit     chan struct{}
	quitOnce sync.Once
}

func newWindowCtl() *windowCtl {
	return &windowCtl{req: make(chan struct{}, 1), quit: make(chan struct{})}
}

func (w *windowCtl) show() {
	w.mu.Lock()
	wv := w.wv
	w.mu.Unlock()
	if wv != nil {
		wv.Dispatch(func() { bringToFront(uintptr(wv.Window())) })
		return
	}
	select {
	case w.req <- struct{}{}:
	default:
	}
}

func (w *windowCtl) terminate() {
	w.quitOnce.Do(func() {
		close(w.quit)
		w.mu.Lock()
		wv := w.wv
		w.mu.Unlock()
		if wv != nil {
			wv.Dispatch(func() { wv.Terminate() })
		}
	})
}

// loop крутится в главном потоке: WebView2 требует один и тот же поток.
func (w *windowCtl) loop(a *App) {
	ico := filepath.Join(a.paths.Data, "icon.ico")
	_ = os.WriteFile(ico, iconICO, 0o644)
	for {
		select {
		case <-w.quit:
			return
		case <-w.req:
		}
		url := "http://127.0.0.1:" + strconv.Itoa(a.port) + "/"
		ww, wh := fitScreen(dpiScale(1240), dpiScale(780))
		wv := webview2.NewWithOptions(webview2.WebViewOptions{
			Debug:     os.Getenv("MD_DEBUG") == "1",
			AutoFocus: true,
			DataPath:  a.paths.WebView,
			WindowOptions: webview2.WindowOptions{
				Title: AppName, Width: uint(ww), Height: uint(wh), Center: true,
			},
		})
		if wv == nil {
			openAppWindowFallback(url)
			continue
		}
		setWindowIconFromFile(uintptr(wv.Window()), ico)
		wv.SetSize(dpiScale(980), dpiScale(560), webview2.HintMin)
		wv.SetSize(ww, wh, webview2.HintNone)
		wv.Navigate(url)
		bringToFront(uintptr(wv.Window()))
		w.mu.Lock()
		w.wv = wv
		w.mu.Unlock()
		wv.Run()
		w.mu.Lock()
		w.wv = nil
		w.mu.Unlock()
	}
}

// Если WebView2 недоступен — окно-приложение Edge/Chrome.
func openAppWindowFallback(url string) {
	for _, p := range []string{
		os.Getenv("ProgramFiles(x86)") + `\Microsoft\Edge\Application\msedge.exe`,
		os.Getenv("ProgramFiles") + `\Microsoft\Edge\Application\msedge.exe`,
		os.Getenv("ProgramFiles") + `\Google\Chrome\Application\chrome.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			_ = exec.Command(p, "--app="+url, "--window-size=1240,780").Start()
			return
		}
	}
	shellOpen(url)
}

func humanSpeed(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f МБ/с", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%d КБ/с", b>>10)
	}
	return fmt.Sprintf("%d Б/с", b)
}

func runTray(a *App) {
	runtime.LockOSThread()
	systray.Run(func() {
		systray.SetIcon(iconICO)
		systray.SetTooltip(AppName)
		systray.SetOnTapped(func() { a.win.show() })
		mOpen := systray.AddMenuItem("Открыть "+AppName, "")
		mFolder := systray.AddMenuItem("Открыть папку загрузок", "")
		systray.AddSeparator()
		mStart := systray.AddMenuItem("▶ Старт всех", "")
		mPause := systray.AddMenuItem("⏸ Пауза всех", "")
		systray.AddSeparator()
		mMax := systray.AddMenuItemCheckbox("Скорость: максимальная", "", true)
		mMid := systray.AddMenuItemCheckbox("Скорость: средняя", "", false)
		mLow := systray.AddMenuItemCheckbox("Скорость: минимальная", "", false)
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Выход", "")
		setSpeed := func(mode string) {
			a.mu.Lock()
			a.settings.SpeedMode = mode
			a.settings.normalize()
			s := a.settings
			a.mu.Unlock()
			a.pushEngineOptions(s)
			go a.saveState()
		}
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-a.quit:
					systray.Quit()
					return
				case <-mOpen.ClickedCh:
					a.win.show()
				case <-mFolder.ClickedCh:
					shellOpen(a.root())
				case <-mStart.ClickedCh:
					a.BulkAction("resume")
				case <-mPause.ClickedCh:
					a.BulkAction("pause")
				case <-mMax.ClickedCh:
					setSpeed("max")
				case <-mMid.ClickedCh:
					setSpeed("mid")
				case <-mLow.ClickedCh:
					setSpeed("low")
				case <-mQuit.ClickedCh:
					go a.Quit()
				case <-t.C:
					a.mu.Lock()
					var down int64
					act := 0
					for _, j := range a.jobs {
						down += j.Speed
						if j.Status == "active" {
							act++
						}
					}
					mode := a.settings.SpeedMode
					a.mu.Unlock()
					tip := AppName
					if act > 0 {
						tip = fmt.Sprintf("%s\nАктивных: %d · ↓ %s", AppName, act, humanSpeed(down))
					}
					systray.SetTooltip(tip)
					for m, item := range map[string]*systray.MenuItem{"max": mMax, "mid": mMid, "low": mLow} {
						if m == mode {
							item.Check()
						} else {
							item.Uncheck()
						}
					}
				}
			}
		}()
	}, func() {})
}

var procGetSystemMetrics = user32.NewProc("GetSystemMetrics")

// Окно не больше 92 % экрана (на ноутбуке 1080p при 150 % иначе вылезет за край).
func fitScreen(w, h int) (int, int) {
	sw, _, _ := procGetSystemMetrics.Call(0) // SM_CXSCREEN
	sh, _, _ := procGetSystemMetrics.Call(1) // SM_CYSCREEN
	if m := int(sw) * 92 / 100; m > 0 && w > m {
		w = m
	}
	if m := int(sh) * 88 / 100; m > 0 && h > m {
		h = m
	}
	return w, h
}
