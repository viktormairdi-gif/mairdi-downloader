package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const Version = "1.3.3"

func init() { runtime.LockOSThread() } // главный поток — для окна WebView2

func main() {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "chrome-extension://") {
		runNativeHost()
		return
	}
	background := false
	var pass []string
	for _, a := range os.Args[1:] {
		switch {
		case a == "--background":
			background = true
		case strings.HasPrefix(a, "--parent-window"):
		default:
			pass = append(pass, a)
		}
	}
	enableDPIAwareness()
	paths := initPaths()

	// Одна копия программы: вторая передаёт ссылки первой и выходит.
	name, _ := windows.UTF16PtrFromString("Local\\" + AppID + "-single")
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		for i := 0; i < 50; i++ {
			if ep, ok := readEndpoint(paths); ok && pingEndpoint(ep) {
				if len(pass) > 0 {
					_ = callEndpoint(ep, "/api/cli", map[string]any{"args": pass}, nil)
				} else if !background {
					_ = callEndpoint(ep, "/api/show", map[string]any{}, nil)
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		return
	}
	defer windows.CloseHandle(h)

	app := NewApp(paths)
	if err := app.Start(); err != nil {
		messageBox(AppName, "Не удалось запустить: "+err.Error())
		return
	}
	go runTray(app)
	if len(pass) > 0 {
		app.HandleCLI(pass)
	} else if !background {
		app.win.show()
	}
	app.win.loop(app)
	app.Quit()
}

// ---------- хост native messaging: мост между расширением Chrome и программой ----------

func runNativeHost() {
	paths := initPaths()
	in := bufio.NewReader(os.Stdin)
	for {
		var n uint32
		if err := binary.Read(in, binary.LittleEndian, &n); err != nil {
			return
		}
		if n == 0 || n > 64<<20 {
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(in, buf); err != nil {
			return
		}
		var msg map[string]any
		_ = json.Unmarshal(buf, &msg)
		reply := nativeHandle(paths, msg)
		out, _ := json.Marshal(reply)
		_ = binary.Write(os.Stdout, binary.LittleEndian, uint32(len(out)))
		_, _ = os.Stdout.Write(out)
	}
}

func nativeHandle(paths Paths, msg map[string]any) map[string]any {
	typ, _ := msg["type"].(string)
	ep, ok := readEndpoint(paths)
	alive := ok && pingEndpoint(ep)
	if !alive {
		if typ == "ping" {
			if start, _ := msg["start"].(bool); !start {
				return map[string]any{"ok": false, "running": false}
			}
		}
		if err := launchMain(paths); err != nil {
			return map[string]any{"ok": false, "error": "Не удалось запустить программу: " + err.Error()}
		}
		for i := 0; i < 100; i++ {
			time.Sleep(100 * time.Millisecond)
			if ep, ok = readEndpoint(paths); ok && pingEndpoint(ep) {
				alive = true
				break
			}
		}
		if !alive {
			return map[string]any{"ok": false, "error": "Программа не отвечает"}
		}
	}
	var res map[string]any
	if err := callEndpoint(ep, "/api/ext", msg, &res); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	res["running"] = true
	return res
}

// Запуск основной программы вне job-объекта браузера, чтобы Chrome не закрыл её вместе с хостом.
func launchMain(paths Paths) error {
	c := exec.Command(paths.Exe, "--background")
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x01000000 | 0x00000008} // BREAKAWAY_FROM_JOB | DETACHED_PROCESS
	if err := c.Start(); err == nil {
		return c.Process.Release()
	}
	c = exec.Command("explorer.exe", paths.Exe)
	return c.Start()
}
