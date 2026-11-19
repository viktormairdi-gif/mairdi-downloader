package main

// Чёткий текст при масштабе экрана 125–200 %. Без этого Windows считает программу
// «не знающей про DPI», рисует окно в 96 dpi и растягивает картинкой — шрифты мылятся.

var (
	procSetDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware     = user32.NewProc("SetProcessDPIAware")
	procGetDpiForSystem        = user32.NewProc("GetDpiForSystem")
)

// Вызывать в самом начале, до создания любых окон (трей, WebView2, диалоги).
func enableDPIAwareness() {
	if procSetDpiAwarenessContext.Find() == nil {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4 (Windows 10 1703+)
		if r, _, _ := procSetDpiAwarenessContext.Call(^uintptr(3)); r != 0 {
			return
		}
	}
	if procSetProcessDPIAware.Find() == nil {
		_, _, _ = procSetProcessDPIAware.Call()
	}
}

// Масштаб основного экрана: 96 dpi = 100 %.
func systemDPI() int {
	if procGetDpiForSystem.Find() == nil {
		if r, _, _ := procGetDpiForSystem.Call(); r >= 96 {
			return int(r)
		}
	}
	return 96
}

// Логические пиксели (как в CSS) → физические.
func dpiScale(v int) int { return v * systemDPI() / 96 }
