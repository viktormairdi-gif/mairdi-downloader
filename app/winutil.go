package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procMessageBoxW           = user32.NewProc("MessageBoxW")
	procOpenClipboard         = user32.NewProc("OpenClipboard")
	procCloseClipboard        = user32.NewProc("CloseClipboard")
	procGetClipboardData      = user32.NewProc("GetClipboardData")
	procGetClipboardSeqNumber = user32.NewProc("GetClipboardSequenceNumber")
	procIsClipboardFmtAvail   = user32.NewProc("IsClipboardFormatAvailable")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procShowWindow            = user32.NewProc("ShowWindow")
	procSendMessageW          = user32.NewProc("SendMessageW")
	procLoadImageW            = user32.NewProc("LoadImageW")
	procShellExecuteW         = shell32.NewProc("ShellExecuteW")
	procSHChangeNotify        = shell32.NewProc("SHChangeNotify")
	procGlobalLock            = kernel32.NewProc("GlobalLock")
	procGlobalUnlock          = kernel32.NewProc("GlobalUnlock")
)

func messageBox(title, text string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x40)
}

// Все дочерние процессы (aria2, yt-dlp, ffmpeg) завершаются вместе с программой.
var (
	killJob     windows.Handle
	killJobOnce sync.Once
)

func attachToKillJob(p *os.Process) {
	killJobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		_, err = windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
		if err != nil {
			windows.CloseHandle(h)
			return
		}
		killJob = h
	})
	if killJob == 0 || p == nil {
		return
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return
	}
	_ = windows.AssignProcessToJobObject(killJob, ph)
	windows.CloseHandle(ph)
}

func killTree(pid int) {
	c := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	_ = c.Run()
}

func hiddenCmd(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return c
}

// shellOpen открывает файл/папку/ссылку программой по умолчанию.
// ShellExecute возвращает код ≤ 32 при ошибке (31 — нет связанной программы).
func shellOpen(target string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	t, _ := windows.UTF16PtrFromString(target)
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(t)), 0, 0, 1)
	switch {
	case r > 32:
		return nil
	case r == 31:
		return errors.New("для этого типа файла в Windows не выбрана программа")
	default:
		return fmt.Errorf("Windows не открыла файл (код %d)", r)
	}
}

func revealInExplorer(path string) {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		shellOpen(path)
		return
	}
	c := exec.Command("explorer.exe", "/select,", path)
	_ = c.Start()
}

func clipboardSeq() uint32 {
	r, _, _ := procGetClipboardSeqNumber.Call()
	return uint32(r)
}

func clipboardText() string {
	const cfUnicodeText = 13
	if r, _, _ := procIsClipboardFmtAvail.Call(cfUnicodeText); r == 0 {
		return ""
	}
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return ""
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return ""
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return ""
	}
	defer procGlobalUnlock.Call(h)
	var out []uint16
	for i := 0; i < 1<<16; i++ {
		c := *(*uint16)(unsafe.Pointer(p + uintptr(i)*2))
		if c == 0 {
			break
		}
		out = append(out, c)
	}
	return windows.UTF16ToString(out)
}

func setWindowIconFromFile(hwnd uintptr, icoPath string) {
	p, _ := windows.UTF16PtrFromString(icoPath)
	const imageIcon, lrLoadFromFile, wmSetIcon = 1, 0x10, 0x80
	big, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)), imageIcon, 48, 48, lrLoadFromFile)
	small, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)), imageIcon, 16, 16, lrLoadFromFile)
	if big != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, 1, big)
	}
	if small != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, 0, small)
	}
}

func bringToFront(hwnd uintptr) {
	procShowWindow.Call(hwnd, 9) // SW_RESTORE
	procSetForegroundWindow.Call(hwnd)
}

// ---------- реестр (только HKCU — собственные ключи программы) ----------

func setRegString(path, name, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

func getRegString(root registry.Key, path, name string) string {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue(name)
	return v
}

func deleteRegValue(path, name string) {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.DeleteValue(name)
}

// Регистрирует хост native messaging для Chrome / Edge / Brave / Chromium.
func (a *App) registerNativeHost() {
	manifest := filepath.Join(a.paths.Data, "native-host.json")
	_ = saveJSON(manifest, map[string]any{
		"name":            NativeHostName,
		"description":     AppName,
		"path":            a.paths.Exe,
		"type":            "stdio",
		"allowed_origins": []string{"chrome-extension://" + ExtensionID + "/"},
	})
	for _, b := range []string{`Software\Google\Chrome`, `Software\Microsoft\Edge`, `Software\BraveSoftware\Brave-Browser`,
		`Software\Chromium`, `Software\Yandex\YandexBrowser`} {
		_ = setRegString(b+`\NativeMessagingHosts\`+NativeHostName, "", manifest)
	}
}

func (a *App) applyAutostart(on bool) {
	const run = `Software\Microsoft\Windows\CurrentVersion\Run`
	if on {
		_ = setRegString(run, AppID, `"`+a.paths.Exe+`" --background`)
	} else {
		deleteRegValue(run, AppID)
	}
}

// Делает программу обработчиком magnet-ссылок и .torrent-файлов и открывает
// окно Windows «Приложения по умолчанию» для подтверждения.
func (a *App) registerAsDefault() {
	exe := a.paths.Exe
	open := `"` + exe + `" "%1"`
	cls := `Software\Classes\`
	_ = setRegString(cls+AppID+".torrent", "", "Торрент-файл")
	_ = setRegString(cls+AppID+`.torrent\DefaultIcon`, "", exe+",0")
	_ = setRegString(cls+AppID+`.torrent\shell\open\command`, "", open)
	_ = setRegString(cls+`.torrent\OpenWithProgids`, AppID+".torrent", "")
	_ = setRegString(cls+`.torrent`, "", AppID+".torrent")

	_ = setRegString(cls+AppID+".magnet", "", "URL:magnet")
	_ = setRegString(cls+AppID+".magnet", "URL Protocol", "")
	_ = setRegString(cls+AppID+`.magnet\shell\open\command`, "", open)
	_ = setRegString(cls+"magnet", "", "URL:magnet")
	_ = setRegString(cls+"magnet", "URL Protocol", "")
	_ = setRegString(cls+`magnet\DefaultIcon`, "", exe+",0")
	_ = setRegString(cls+`magnet\shell\open\command`, "", open)

	capPath := `Software\` + AppID + `\Capabilities`
	_ = setRegString(capPath, "ApplicationName", AppName)
	_ = setRegString(capPath, "ApplicationDescription", "Менеджер загрузок с раскладкой по папкам")
	_ = setRegString(capPath+`\FileAssociations`, ".torrent", AppID+".torrent")
	_ = setRegString(capPath+`\URLAssociations`, "magnet", AppID+".magnet")
	_ = setRegString(`Software\RegisteredApplications`, AppID, capPath)
	procSHChangeNotify.Call(0x08000000, 0, 0, 0)
	shellOpen("ms-settings:defaultapps?registeredAppUser=" + AppID)
}

// Откуда скачан файл браузером: Windows хранит адрес в потоке Zone.Identifier.
func zoneIdentifier(path string) (host, referrer string) {
	b, err := os.ReadFile(path + ":Zone.Identifier")
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "HostUrl="); ok {
			host = v
		} else if v, ok := strings.CutPrefix(line, "ReferrerUrl="); ok {
			referrer = v
		}
	}
	return
}

func pickFolder(start string) string {
	ps := `Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
		`$d.Description = 'Папка для загрузок'; $d.UseDescriptionForTitle = $true; $d.SelectedPath = $env:MD_START; ` +
		`$f = New-Object System.Windows.Forms.Form; $f.TopMost = $true; ` +
		`if ($d.ShowDialog($f) -eq 'OK') { [Console]::OutputEncoding = [Text.Encoding]::UTF8; Write-Output $d.SelectedPath }`
	c := hiddenCmd("powershell.exe", "-NoProfile", "-STA", "-Command", ps)
	c.Env = append(os.Environ(), "MD_START="+start)
	out, err := c.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Окно выбора файлов (несколько сразу) или папки — для загрузки на Google Диск.
func pickUploadPaths(mode string) []string {
	var ps string
	if mode == "folder" {
		ps = `Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
			`$d.Description = 'Папка для загрузки на Google Диск'; $d.UseDescriptionForTitle = $true; ` +
			`$f = New-Object System.Windows.Forms.Form; $f.TopMost = $true; ` +
			`if ($d.ShowDialog($f) -eq 'OK') { [Console]::OutputEncoding = [Text.Encoding]::UTF8; Write-Output $d.SelectedPath }`
	} else {
		ps = `Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.OpenFileDialog; ` +
			`$d.Title = 'Файлы для загрузки на Google Диск'; $d.Multiselect = $true; $d.Filter = 'Все файлы (*.*)|*.*'; ` +
			`$f = New-Object System.Windows.Forms.Form; $f.TopMost = $true; ` +
			`if ($d.ShowDialog($f) -eq 'OK') { [Console]::OutputEncoding = [Text.Encoding]::UTF8; $d.FileNames | ForEach-Object { Write-Output $_ } }`
	}
	out, err := hiddenCmd("powershell.exe", "-NoProfile", "-STA", "-Command", ps).Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths
}

// Пункт «Отправить → Google Диск (MAIRDI)» в контекстном меню Проводника.
const sendToName = "Google Диск (MAIRDI).lnk"

func sendToPath() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_SendTo, 0); err == nil && p != "" {
		return filepath.Join(p, sendToName)
	}
	return filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\SendTo`, sendToName)
}

func sendToInstalled() bool {
	_, err := os.Stat(sendToPath())
	return err == nil
}

func setSendTo(exe string, on bool) error {
	lnk := sendToPath()
	if !on {
		if err := os.Remove(lnk); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	ps := `$s = (New-Object -ComObject WScript.Shell).CreateShortcut($env:MD_LNK); $s.TargetPath = $env:MD_EXE; ` +
		`$s.Arguments = '--gdrive-send'; $s.IconLocation = $env:MD_EXE + ',0'; $s.Description = 'Загрузить на Google Диск'; $s.Save()`
	c := hiddenCmd("powershell.exe", "-NoProfile", "-Command", ps)
	c.Env = append(os.Environ(), "MD_LNK="+lnk, "MD_EXE="+exe)
	if out, err := c.CombinedOutput(); err != nil {
		return errors.New("не удалось создать ярлык: " + strings.TrimSpace(string(out)))
	}
	return nil
}

func findGoogleDrive() string {
	for _, d := range "GHIJKLMNOPQRSTUVWXYZ" {
		for _, n := range []string{"Мой диск", "My Drive"} {
			p := string(d) + `:\` + n
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				return p
			}
		}
	}
	return ""
}
