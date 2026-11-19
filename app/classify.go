package main

import (
	"archive/zip"
	"bytes"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Категории верхнего уровня — то, что пользователь видит в папке загрузок.
var Categories = []string{"Прайсы", "Документы", "Дистрибутивы", "Программирование", "Видео", "Музыка",
	"Изображения", "Книги", "Архивы", "Торренты", "Google Диск", "Прочее"}

// Стандартные подпапки — показываются в диалоге «Переместить в…».
var StandardDirs = []string{
	"Прайсы",
	`Документы\PDF`, `Документы\Word`, `Документы\Таблицы`, `Документы\Презентации`, `Документы\Тексты`, `Документы\Договоры и счета`,
	`Дистрибутивы\Программы`, `Дистрибутивы\Драйверы`, `Дистрибутивы\Образы дисков`, `Дистрибутивы\Android`, `Дистрибутивы\Обновления Windows`, `Дистрибутивы\macOS и Linux`,
	`Программирование\Исходники`, `Программирование\Библиотеки и SDK`, `Программирование\Инструменты`, `Программирование\Базы данных`, `Программирование\Данные`,
	`Видео`, `Видео\Фильмы и сериалы`, `Музыка`, `Изображения`, `Изображения\Дизайн-макеты`, `Книги`, `Архивы`, `Торренты`, `Прочее`, `Прочее\Шрифты`,
}

type Source struct {
	URL     string
	PageURL string
	Domain  string
}

type Verdict struct {
	Dir    string // относительно корня: «Прайсы\asiamobile.kg»
	Reason string
	NewExt string // если расширение было потеряно и восстановлено по содержимому
}

func extSet(list string) map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(list) {
		m["."+e] = true
	}
	return m
}

var (
	extVideo   = extSet("mp4 mkv avi mov webm wmv flv m4v ts m2ts mts 3gp mpg mpeg vob ogv")
	extAudio   = extSet("mp3 flac wav m4a aac ogg opus wma alac aiff ape mka")
	extImage   = extSet("jpg jpeg png gif webp bmp svg heic heif tif tiff ico avif jfif raw cr2 cr3 nef arw dng")
	extDesign  = extSet("psd ai fig sketch cdr xd indd eps afdesign afphoto")
	extBook    = extSet("epub fb2 mobi azw azw3 djvu djv cbr cbz")
	extWord    = extSet("doc docx odt rtf dotx")
	extTable   = extSet("xls xlsx xlsm xlsb ods csv")
	extSlides  = extSet("ppt pptx odp pps ppsx key")
	extText    = extSet("txt md log nfo")
	extSetup   = extSet("exe msi msix msixbundle appx appxbundle")
	extDisk    = extSet("iso img vhd vhdx wim esd vmdk ova bin cue mdf nrg")
	extAndroid = extSet("apk xapk apks apkm")
	extOther   = extSet("dmg pkg deb rpm appimage snap flatpak ipa")
	extWinUpd  = extSet("msu cab")
	extArchive = extSet("zip rar 7z tar gz tgz bz2 xz zst lz lzma tbz2 txz")
	extSource  = extSet("pas dpr dpk dfm dproj inc go py pyw ipynb js mjs cjs ts tsx jsx vue svelte java kt kts scala cs vb fs cpp cc cxx c h hpp hh rs php rb swift m mm dart lua pl r jl sh bash zsh ps1 psm1 bat cmd html htm css scss sass less sln csproj vcxproj gradle cmake mk asm s ex exs erl hs clj groovy")
	extLib     = extSet("whl jar war aar nupkg gem crate dll so dylib lib a bpl dcu dcp tlb")
	extDevTool = extSet("vsix")
	extDB      = extSet("sql bak mdf ldf db sqlite sqlite3 dump backup fdb gdb accdb mdb")
	extData    = extSet("json yaml yml toml xml ini cfg conf env proto graphql")
	extFont    = extSet("ttf otf woff woff2 eot fon")
)

var (
	rePriceName = regexp.MustCompile(`(?i)(прайс|прейскурант|price|pricelist|price[-_ ]?list|остатк|наличи|stock|ассортимент|прайслист|расценк|цены|tarif|тариф)`)
	reDocsName  = regexp.MustCompile(`(?i)(сч[её]т|invoice|счет-фактур|накладн|акт[ _-]|акт\b|договор|contract|соглашени|agreement|эсф|esf|waybill|ттн|квитанц|receipt|чек[ _-])`)
	reDriver    = regexp.MustCompile(`(?i)(driver|драйвер|chipset|nvidia|geforce|radeon|amd[-_ ]?(software|adrenalin|chipset)|realtek|intel[-_ ].*(graphics|wifi|wireless|bluetooth|lan|me)|bios|firmware|прошивк|wacom|logitech|razer|synaptics|elan|mediatek|qualcomm|broadcom|hp[-_ ]?(smart|support)|epson|canon|brother|kyocera|xerox|samsung[-_ ]?(printer|usb))`)
	reDevTool   = regexp.MustCompile(`(?i)(vscode|visual ?studio|code[-_ ]?setup|git-|git_|github ?desktop|nodejs|node-v|python-|go\d|golang|delphi|radstudio|rad ?studio|embarcadero|jetbrains|intellij|pycharm|webstorm|goland|rider|clion|datagrip|phpstorm|android ?studio|docker|postman|insomnia|ssms|sql ?server|dbeaver|heidisql|navicat|xampp|wamp|openjdk|jdk|msys|mingw|cmake|putty|winscp|filezilla|notepad\+\+|sublime|cursor|windsurf|wsl|terraform|kubectl|helm|ngrok|composer|ruby|rustup|deno|bun-|yarn|pnpm)`)
	reCodeHost  = regexp.MustCompile(`(?i)(^|\.)(github\.com|githubusercontent\.com|gitlab\.com|bitbucket\.org|codeload\.github\.com|sourceforge\.net|npmjs\.(com|org)|pypi\.org|pythonhosted\.org|nuget\.org|maven\.org|packagist\.org|crates\.io|golang\.org|go\.dev|getit\.embarcadero\.com|marketplace\.visualstudio\.com)$`)
	reBookName  = regexp.MustCompile(`(?i)(книга|book|учебник|самоучитель|руководство|manual|handbook|ebook|глава|роман|cookbook)`)
)

var priceHeadWords = []string{"цена", "price", "стоимость", "опт", "розн", "сом", "usd", "kgs", "руб", "тенге", "прайс", "закуп"}
var priceItemWords = []string{"артикул", "наименование", "товар", "модель", "sku", "код", "кол-во", "количество", "остаток", "наличие", "model", "qty", "item", "бренд", "brand", "ед.", "штрих"}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	return strings.TrimPrefix(h, "www.")
}

// Человекочитаемое имя сайта для подпапки: youtube.com → YouTube, asiamobile.kg → asiamobile.kg.
func siteFolder(domain string) string {
	known := map[string]string{
		"youtube.com": "YouTube", "youtu.be": "YouTube", "m.youtube.com": "YouTube", "music.youtube.com": "YouTube Music",
		"instagram.com": "Instagram", "tiktok.com": "TikTok", "vk.com": "ВКонтакте", "vkvideo.ru": "ВКонтакте", "vk.ru": "ВКонтакте",
		"rutube.ru": "Rutube", "facebook.com": "Facebook", "fb.watch": "Facebook", "x.com": "X (Twitter)", "twitter.com": "X (Twitter)",
		"vimeo.com": "Vimeo", "dailymotion.com": "Dailymotion", "ok.ru": "Одноклассники", "twitch.tv": "Twitch",
		"soundcloud.com": "SoundCloud", "reddit.com": "Reddit", "t.me": "Telegram", "pinterest.com": "Pinterest",
		"bilibili.com": "Bilibili", "dzen.ru": "Дзен", "kick.com": "Kick", "threads.net": "Threads", "coub.com": "Coub",
	}
	d := strings.TrimPrefix(strings.ToLower(domain), "www.")
	if v, ok := known[d]; ok {
		return v
	}
	for k, v := range known {
		if strings.HasSuffix(d, "."+k) {
			return v
		}
	}
	if d == "" || isCloudHost(d) {
		return "" // облако — не «сайт поставщика», подпапка по его имени ничего не говорит
	}
	return safeName(d)
}

func isCloudHost(d string) bool {
	for _, c := range []string{"drive.google.com", "docs.google.com", "drive.usercontent.google.com", "googleusercontent.com",
		"disk.yandex.ru", "disk.yandex.com", "disk.yandex.kz", "yadi.sk", "downloader.disk.yandex.ru", "cloud.mail.ru",
		"dropbox.com", "dropboxusercontent.com", "onedrive.live.com", "1drv.ms", "sharepoint.com"} {
		if d == c || strings.HasSuffix(d, "."+c) {
			return true
		}
	}
	return false
}

var reBadChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func safeName(s string) string {
	s = reBadChars.ReplaceAllString(s, "_")
	s = strings.Trim(s, " .")
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120])
	}
	if s == "" {
		s = "file"
	}
	return s
}

func (a *App) sourceDomain(src Source) string {
	if src.Domain != "" {
		return src.Domain
	}
	for _, u := range []string{src.PageURL, src.URL} {
		if h := hostOf(u); h != "" && !isCDN(h) {
			return h
		}
	}
	return hostOf(src.URL)
}

// CDN и файловые хостинги не говорят ничего о поставщике.
func isCDN(h string) bool {
	for _, s := range []string{"googleusercontent.com", "drive.usercontent.google.com", "cloudfront.net", "akamaized.net",
		"amazonaws.com", "dropboxusercontent.com", "yandex.net", "cdn.", "storage.googleapis.com", "1drv.ms", "sharepoint.com"} {
		if strings.Contains(h, s) {
			return true
		}
	}
	return false
}

// Classify решает, в какую папку положить скачанный файл.
func (a *App) Classify(filePath, name string, src Source) Verdict {
	ext := strings.ToLower(filepath.Ext(name))
	domain := a.sourceDomain(src)
	v := Verdict{}

	// Расширение потеряно или подозрительное — смотрим сигнатуру.
	if ext == "" || ext == ".bin" || ext == ".tmp" || ext == ".download" || ext == ".php" || ext == ".aspx" {
		if real := sniffExt(filePath); real != "" && real != ext {
			v.NewExt = real
			ext = real
		}
	}

	// 1. Правила, которым программу научил пользователь.
	if r := a.rules.match(domain, name, ext); r != nil {
		v.Dir = r.Dir
		v.Reason = "Ваше правило: " + r.describe()
		return v
	}

	base := strings.ToLower(name)
	site := siteFolder(domain)

	// 2. Прайсы — по имени или по содержимому таблицы.
	isTable := extTable[ext]
	if (isTable || ext == ".pdf" || ext == ".zip") && rePriceName.MatchString(base) {
		v.Dir = joinNonEmpty("Прайсы", site)
		v.Reason = "В названии есть слово «прайс/цены/остатки»"
		return v
	}
	if isTable && looksLikePriceList(filePath, ext) {
		v.Dir = joinNonEmpty("Прайсы", site)
		v.Reason = "Внутри таблицы колонки цены и товара"
		return v
	}

	switch {
	case ext == ".torrent":
		v.Dir, v.Reason = "Торренты", "Торрент-файл"
	case extSetup[ext]:
		switch {
		case reDevTool.MatchString(base):
			v.Dir, v.Reason = `Программирование\Инструменты`, "Установщик инструмента разработчика"
		case reDriver.MatchString(base):
			v.Dir, v.Reason = `Дистрибутивы\Драйверы`, "Похоже на драйвер или прошивку"
		default:
			v.Dir, v.Reason = `Дистрибутивы\Программы`, "Установщик программы"
		}
	case extDisk[ext]:
		v.Dir, v.Reason = `Дистрибутивы\Образы дисков`, "Образ диска"
	case extAndroid[ext]:
		v.Dir, v.Reason = `Дистрибутивы\Android`, "Приложение Android"
	case extOther[ext]:
		v.Dir, v.Reason = `Дистрибутивы\macOS и Linux`, "Пакет macOS/Linux"
	case extWinUpd[ext]:
		v.Dir, v.Reason = `Дистрибутивы\Обновления Windows`, "Пакет обновления Windows"
	case extDevTool[ext]:
		v.Dir, v.Reason = `Программирование\Инструменты`, "Расширение редактора кода"
	case extSource[ext]:
		v.Dir, v.Reason = `Программирование\Исходники`, "Исходный код"
	case extLib[ext]:
		v.Dir, v.Reason = `Программирование\Библиотеки и SDK`, "Библиотека или пакет"
	case extDB[ext]:
		v.Dir, v.Reason = `Программирование\Базы данных`, "Файл базы данных / дамп"
	case extData[ext]:
		v.Dir, v.Reason = `Программирование\Данные`, "Файл данных / конфигурации"
	case extVideo[ext]:
		v.Dir, v.Reason = joinNonEmpty("Видео", site), "Видеофайл"
	case extAudio[ext]:
		v.Dir, v.Reason = "Музыка", "Аудиофайл"
	case extDesign[ext]:
		v.Dir, v.Reason = `Изображения\Дизайн-макеты`, "Макет дизайна"
	case extImage[ext]:
		v.Dir, v.Reason = "Изображения", "Изображение"
	case extBook[ext]:
		v.Dir, v.Reason = "Книги", "Электронная книга"
	case extFont[ext]:
		v.Dir, v.Reason = `Прочее\Шрифты`, "Шрифт"
	case ext == ".pdf":
		switch {
		case reDocsName.MatchString(base):
			v.Dir, v.Reason = `Документы\Договоры и счета`, "Похоже на договор, счёт или акт"
		case reBookName.MatchString(base) || fileSize(filePath) > 15<<20:
			v.Dir, v.Reason = "Книги", "Большой PDF или книга по названию"
		default:
			v.Dir, v.Reason = `Документы\PDF`, "PDF-документ"
		}
	case extWord[ext]:
		if reDocsName.MatchString(base) {
			v.Dir, v.Reason = `Документы\Договоры и счета`, "Похоже на договор, счёт или акт"
		} else {
			v.Dir, v.Reason = `Документы\Word`, "Документ Word"
		}
	case isTable:
		if reDocsName.MatchString(base) {
			v.Dir, v.Reason = `Документы\Договоры и счета`, "Похоже на счёт или накладную"
		} else {
			v.Dir, v.Reason = `Документы\Таблицы`, "Таблица"
		}
	case extSlides[ext]:
		v.Dir, v.Reason = `Документы\Презентации`, "Презентация"
	case extText[ext]:
		v.Dir, v.Reason = `Документы\Тексты`, "Текстовый файл"
	case extArchive[ext]:
		v.Dir, v.Reason = a.classifyArchive(filePath, ext, base, domain)
	default:
		v.Dir, v.Reason = "Прочее", "Тип файла не распознан"
	}
	if reCodeHost.MatchString(domain) && (strings.HasPrefix(v.Dir, "Архивы") || v.Dir == "Прочее") {
		v.Dir, v.Reason = `Программирование\Исходники`, "Скачано с сайта для разработчиков ("+domain+")"
	}
	return v
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, `\`)
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

// Архив: заглядываем внутрь (zip) и решаем по содержимому.
func (a *App) classifyArchive(p, ext, base, domain string) (string, string) {
	if reCodeHost.MatchString(domain) {
		return `Программирование\Исходники`, "Архив с сайта для разработчиков (" + domain + ")"
	}
	if ext == ".zip" {
		if zr, err := zip.OpenReader(p); err == nil {
			defer zr.Close()
			var n, src, setup, img, aud, vid, doc int
			marker := false
			for _, f := range zr.File {
				if f.FileInfo().IsDir() {
					continue
				}
				n++
				nm := strings.ToLower(path.Base(f.Name))
				e := strings.ToLower(path.Ext(nm))
				switch {
				case nm == "package.json" || nm == "go.mod" || nm == "cargo.toml" || nm == "pom.xml" || nm == "requirements.txt" ||
					nm == "cmakelists.txt" || nm == "makefile" || nm == "build.gradle" || nm == "composer.json" ||
					strings.HasSuffix(nm, ".sln") || strings.HasSuffix(nm, ".dproj") || strings.HasSuffix(nm, ".csproj") ||
					strings.Contains(f.Name, ".git/"):
					marker = true
				case extSource[e]:
					src++
				case extSetup[e]:
					setup++
				case extImage[e] || extDesign[e]:
					img++
				case extAudio[e]:
					aud++
				case extVideo[e]:
					vid++
				case extWord[e] || extTable[e] || e == ".pdf" || extSlides[e]:
					doc++
				}
			}
			switch {
			case marker || (n > 0 && src*3 >= n):
				return `Программирование\Исходники`, "Архив с исходным кодом"
			case setup > 0 && reDriver.MatchString(base):
				return `Дистрибутивы\Драйверы`, "Архив с драйвером"
			case setup > 0 && reDevTool.MatchString(base):
				return `Программирование\Инструменты`, "Архив с инструментом разработчика"
			case setup > 0:
				return `Дистрибутивы\Программы`, "Архив с установщиком"
			case n > 0 && img*2 >= n:
				return "Изображения", "Архив с изображениями"
			case n > 0 && aud*2 >= n:
				return "Музыка", "Архив с музыкой"
			case n > 0 && vid*2 >= n:
				return "Видео", "Архив с видео"
			case n > 0 && doc*2 >= n:
				return "Документы", "Архив с документами"
			}
		}
	}
	if reDriver.MatchString(base) {
		return `Дистрибутивы\Драйверы`, "Архив с драйвером (по названию)"
	}
	if reDevTool.MatchString(base) {
		return `Программирование\Инструменты`, "Архив с инструментом разработчика (по названию)"
	}
	return "Архивы", "Архив"
}

// ---------- содержимое таблиц: прайс или нет ----------

func looksLikePriceList(p, ext string) bool {
	text := strings.ToLower(tableText(p, ext))
	if text == "" {
		return false
	}
	head, item := 0, 0
	for _, w := range priceHeadWords {
		if strings.Contains(text, w) {
			head++
		}
	}
	for _, w := range priceItemWords {
		if strings.Contains(text, w) {
			item++
		}
	}
	return head >= 1 && item >= 2
}

func tableText(p, ext string) string {
	switch ext {
	case ".xlsx", ".xlsm", ".xlsb", ".ods":
		zr, err := zip.OpenReader(p)
		if err != nil {
			return ""
		}
		defer zr.Close()
		var sb strings.Builder
		for _, f := range zr.File {
			if f.Name == "xl/sharedStrings.xml" || f.Name == "content.xml" || (strings.HasPrefix(f.Name, "xl/worksheets/sheet1") && sb.Len() < 1<<20) {
				rc, err := f.Open()
				if err != nil {
					continue
				}
				b, _ := io.ReadAll(io.LimitReader(rc, 2<<20))
				rc.Close()
				sb.Write(b)
			}
		}
		return sb.String()
	case ".csv":
		b := readHead(p, 64<<10)
		if isUTF8ish(b) {
			return string(b)
		}
		return cp1251(b)
	case ".xls":
		b := readHead(p, 4<<20)
		return utf16Strings(b) + " " + cp1251(b)
	}
	return ""
}

func readHead(p string, n int) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, int64(n)))
	return b
}

func isUTF8ish(b []byte) bool {
	if len(b) > 8 {
		b = b[:len(b)-4] // последний символ мог обрезаться
	}
	return utf8.Valid(bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF")))
}

func cp1251(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c >= 0xC0:
			sb.WriteRune(rune(0x410 + int(c) - 0xC0))
		case c == 0xA8:
			sb.WriteRune('Ё')
		case c == 0xB8:
			sb.WriteRune('ё')
		default:
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

// Вытаскивает строки UTF-16LE из бинарного .xls.
func utf16Strings(b []byte) string {
	var sb strings.Builder
	var cur []uint16
	flush := func() {
		if len(cur) >= 3 {
			sb.WriteString(string(utf16.Decode(cur)))
			sb.WriteByte(' ')
		}
		cur = cur[:0]
	}
	for i := 0; i+1 < len(b); i += 2 {
		u := uint16(b[i]) | uint16(b[i+1])<<8
		if (u >= 0x20 && u < 0x7f) || (u >= 0x400 && u < 0x460) {
			cur = append(cur, u)
		} else {
			flush()
		}
	}
	flush()
	return sb.String()
}

// ---------- сигнатуры файлов ----------

func sniffExt(p string) string {
	b := readHead(p, 4096)
	if len(b) < 4 {
		return ""
	}
	has := func(sig string, off int) bool { return len(b) >= off+len(sig) && string(b[off:off+len(sig)]) == sig }
	switch {
	case has("%PDF", 0):
		return ".pdf"
	case has("PK\x03\x04", 0):
		s := string(b)
		switch {
		case strings.Contains(s, "word/"):
			return ".docx"
		case strings.Contains(s, "xl/"):
			return ".xlsx"
		case strings.Contains(s, "ppt/"):
			return ".pptx"
		case strings.Contains(s, "AndroidManifest.xml"):
			return ".apk"
		case strings.Contains(s, "mimetypeapplication/epub"):
			return ".epub"
		}
		return ".zip"
	case has("Rar!", 0):
		return ".rar"
	case has("7z\xBC\xAF\x27\x1C", 0):
		return ".7z"
	case has("\x1F\x8B", 0):
		return ".gz"
	case has("MZ", 0):
		return ".exe"
	case has("\x89PNG", 0):
		return ".png"
	case has("\xFF\xD8\xFF", 0):
		return ".jpg"
	case has("GIF8", 0):
		return ".gif"
	case has("RIFF", 0) && has("WEBP", 8):
		return ".webp"
	case has("RIFF", 0) && has("AVI ", 8):
		return ".avi"
	case has("RIFF", 0) && has("WAVE", 8):
		return ".wav"
	case has("ftyp", 4):
		return ".mp4"
	case has("\x1A\x45\xDF\xA3", 0):
		return ".mkv"
	case has("ID3", 0) || (b[0] == 0xFF && b[1]&0xE0 == 0xE0):
		return ".mp3"
	case has("fLaC", 0):
		return ".flac"
	case has("OggS", 0):
		return ".ogg"
	case has("\xD0\xCF\x11\xE0", 0):
		return ".doc"
	case has("d8:announce", 0) || has("d4:info", 0) || has("d13:announce", 0):
		return ".torrent"
	}
	return ""
}

// ---------- торренты: категория по списку файлов ----------

type tFile struct {
	Name string
	Size int64
}

func classifyTorrent(files []tFile, name string) (string, string) {
	weight := map[string]int64{}
	for _, f := range files {
		e := strings.ToLower(filepath.Ext(f.Name))
		var k string
		switch {
		case extVideo[e]:
			k = "video"
		case extAudio[e]:
			k = "audio"
		case extDisk[e]:
			k = "disk"
		case extSetup[e] || extArchive[e]:
			k = "soft"
		case extBook[e] || e == ".pdf":
			k = "book"
		case extImage[e]:
			k = "image"
		case extSource[e]:
			k = "code"
		case extAndroid[e]:
			k = "android"
		default:
			k = "other"
		}
		weight[k] += f.Size + 1
	}
	type kv struct {
		k string
		v int64
	}
	var arr []kv
	for k, v := range weight {
		arr = append(arr, kv{k, v})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].v > arr[j].v })
	top := "other"
	if len(arr) > 0 {
		top = arr[0].k
	}
	lname := strings.ToLower(name)
	switch top {
	case "video":
		return `Видео\Фильмы и сериалы`, "Торрент: в основном видео"
	case "audio":
		return "Музыка", "Торрент: в основном музыка"
	case "disk":
		return `Дистрибутивы\Образы дисков`, "Торрент: образ диска"
	case "soft":
		if reDevTool.MatchString(lname) {
			return `Программирование\Инструменты`, "Торрент: инструмент разработчика"
		}
		return `Дистрибутивы\Программы`, "Торрент: программы"
	case "book":
		return "Книги", "Торрент: книги"
	case "image":
		return "Изображения", "Торрент: изображения"
	case "code":
		return `Программирование\Исходники`, "Торрент: исходный код"
	case "android":
		return `Дистрибутивы\Android`, "Торрент: приложения Android"
	}
	return `Прочее\Торренты`, "Торрент: смешанное содержимое"
}
