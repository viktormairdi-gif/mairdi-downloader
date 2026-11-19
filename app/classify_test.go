package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func testApp(t *testing.T) *App {
	d := t.TempDir()
	return &App{rules: loadRules(filepath.Join(d, "rules.json")), settings: Settings{Root: d}, lib: newLibrary()}
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func makeZip(t *testing.T, dir, name string, files map[string]string) string {
	p := filepath.Join(dir, name)
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	zw.Close()
	f.Close()
	return p
}

func TestClassify(t *testing.T) {
	a := testApp(t)
	d := t.TempDir()
	cases := []struct {
		name, file, url, want string
		data                  []byte
	}{
		{"прайс по имени", "Прайс-лист 06.11.xlsx", "https://asiamobile.kg/files/p.xlsx", `Прайсы\asiamobile.kg`, []byte("x")},
		{"прайс по содержимому csv", "export.csv", "https://shop.kz/e.csv", `Прайсы\shop.kz`, []byte("Артикул;Наименование;Цена опт;Остаток\n1;iPhone;100;5\n")},
		{"обычная таблица", "отчёт.csv", "", `Документы\Таблицы`, []byte("Дата;Сумма\n")},
		{"установщик", "AnyDesk.exe", "https://anydesk.com/AnyDesk.exe", `Дистрибутивы\Программы`, []byte("MZ")},
		{"драйвер", "nvidia-driver-560.exe", "", `Дистрибутивы\Драйверы`, []byte("MZ")},
		{"инструмент разработчика", "VSCodeUserSetup-x64.exe", "", `Программирование\Инструменты`, []byte("MZ")},
		{"образ", "Win11_24H2.iso", "", `Дистрибутивы\Образы дисков`, []byte("x")},
		{"apk", "app-release.apk", "", `Дистрибутивы\Android`, []byte("PK")},
		{"исходник", "main.pas", "", `Программирование\Исходники`, []byte("unit")},
		{"github zip", "master.zip", "https://codeload.github.com/x/y/zip/master", `Программирование\Исходники`, []byte("PK")},
		{"договор", "Договор поставки №5.pdf", "", `Документы\Договоры и счета`, []byte("%PDF")},
		{"pdf", "инструкция.pdf", "", `Документы\PDF`, []byte("%PDF")},
		{"видео", "clip.mp4", "https://example.com/clip.mp4", `Видео\example.com`, []byte("x")},
		{"музыка", "song.mp3", "", "Музыка", []byte("ID3")},
		{"книга", "book.fb2", "", "Книги", []byte("x")},
		{"без расширения → pdf", "download", "", `Документы\PDF`, []byte("%PDF-1.7")},
		{"неизвестное", "data.xyz", "", "Прочее", []byte("x")},
	}
	for _, c := range cases {
		p := writeFile(t, d, c.file, c.data)
		v := a.Classify(p, c.file, Source{URL: c.url})
		if v.Dir != c.want {
			t.Errorf("%s: %q → %q (%s), ожидалось %q", c.name, c.file, v.Dir, v.Reason, c.want)
		}
	}
	src := makeZip(t, d, "project.zip", map[string]string{"go.mod": "module x", "main.go": "package main"})
	if v := a.Classify(src, "project.zip", Source{}); v.Dir != `Программирование\Исходники` {
		t.Errorf("zip с кодом → %q", v.Dir)
	}
	setup := makeZip(t, d, "realtek-audio-driver.zip", map[string]string{"Setup.exe": "MZ"})
	if v := a.Classify(setup, "realtek-audio-driver.zip", Source{}); v.Dir != `Дистрибутивы\Драйверы` {
		t.Errorf("zip с драйвером → %q", v.Dir)
	}
	xlsx := makeZip(t, d, "book1.xlsx", map[string]string{"xl/sharedStrings.xml": "<sst><si><t>Наименование</t></si><si><t>Цена</t></si><si><t>Кол-во</t></si></sst>"})
	if v := a.Classify(xlsx, "book1.xlsx", Source{URL: "https://supplier.kg/x"}); v.Dir != `Прайсы\supplier.kg` {
		t.Errorf("xlsx-прайс по содержимому → %q", v.Dir)
	}
}

func TestRulesWin(t *testing.T) {
	a := testApp(t)
	d := t.TempDir()
	_ = a.rules.add("domain", "www.asiamobile.kg", `Прайсы\Asia Mobile`)
	p := writeFile(t, d, "list.pdf", []byte("%PDF"))
	if v := a.Classify(p, "list.pdf", Source{URL: "https://cdn.asiamobile.kg/list.pdf"}); v.Dir != `Прайсы\Asia Mobile` {
		t.Errorf("правило по сайту не сработало: %q", v.Dir)
	}
	_ = a.rules.add("ext", "dwg", `Документы\Чертежи`)
	p2 := writeFile(t, d, "plan.dwg", []byte("x"))
	if v := a.Classify(p2, "plan.dwg", Source{}); v.Dir != `Документы\Чертежи` {
		t.Errorf("правило по расширению: %q", v.Dir)
	}
}

func TestTorrentParse(t *testing.T) {
	data := []byte("d8:announce3:x:y4:infod5:filesld6:lengthi1500000000e4:pathl9:movie.mkveed6:lengthi100e4:pathl8:info.nfoeee4:name5:Movie12:piece lengthi1ee6:pieces0:ee")
	name, files, err := parseTorrent(data)
	if err != nil || name != "Movie" || len(files) != 2 {
		t.Fatalf("parse: %v %q %d", err, name, len(files))
	}
	if dir, _ := classifyTorrent(files, name); dir != `Видео\Фильмы и сериалы` {
		t.Errorf("торрент с фильмом → %q", dir)
	}
}

func TestCloud(t *testing.T) {
	r, _ := resolveCloud("https://drive.google.com/file/d/1AbCdEfGhIjKlMnOpQrStUvWxYz012345/view?usp=sharing")
	if r.URL != "https://drive.usercontent.google.com/download?id=1AbCdEfGhIjKlMnOpQrStUvWxYz012345&export=download&confirm=t" {
		t.Errorf("gdrive: %s", r.URL)
	}
	r, _ = resolveCloud("https://docs.google.com/spreadsheets/d/1AbCdEfGhIjKlMnOpQrStUvWxYz012345/edit#gid=0")
	if r.URL != "https://docs.google.com/spreadsheets/d/1AbCdEfGhIjKlMnOpQrStUvWxYz012345/export?format=xlsx" {
		t.Errorf("gsheets: %s", r.URL)
	}
	r, _ = resolveCloud("https://www.dropbox.com/s/abc/file.zip?dl=0")
	if r.URL != "https://www.dropbox.com/s/abc/file.zip?dl=1" {
		t.Errorf("dropbox: %s", r.URL)
	}
	r, _ = resolveCloud("https://github.com/aria2/aria2/blob/master/README.rst")
	if r.URL != "https://raw.githubusercontent.com/aria2/aria2/master/README.rst" {
		t.Errorf("github: %s", r.URL)
	}
	if _, err := resolveCloud("https://mega.nz/file/x#y"); err == nil {
		t.Error("mega должен дать понятную ошибку")
	}
}

func TestDedup(t *testing.T) {
	d := t.TempDir()
	lib := newLibrary()
	orig := writeFile(t, d, "setup.exe", []byte("same-bytes-1234567890"))
	lib.add(orig, fileSize(orig))
	if got := lib.findByNameSize("setup (2).exe", fileSize(orig)); got != orig {
		t.Errorf("имя+размер: %q", got)
	}
	tmp := writeFile(t, t.TempDir(), "other-name.exe", []byte("same-bytes-1234567890"))
	if got := lib.findSameContent(tmp); got != orig {
		t.Errorf("по содержимому: %q", got)
	}
	diff := writeFile(t, t.TempDir(), "x.exe", []byte("diff-bytes-1234567890"))
	if got := lib.findSameContent(diff); got != "" {
		t.Errorf("разные файлы не должны совпасть: %q", got)
	}
	if urlKey("https://youtu.be/abc123?si=xyz") != urlKey("https://www.youtube.com/watch?v=abc123&feature=share") {
		t.Error("youtube-ссылки одного ролика должны совпасть")
	}
	if urlKey("https://a.kg/f.zip?utm_source=tg") != urlKey("https://a.kg/f.zip") {
		t.Error("utm-метки не должны мешать")
	}
}

func TestClipboardWorthy(t *testing.T) {
	yes := []string{"https://a.kg/price.xlsx", "magnet:?xt=urn:btih:abc", "https://disk.yandex.ru/d/xyz", "https://x.com/setup.exe?v=1"}
	no := []string{"https://google.com/search?q=1", "просто текст", "https://youtube.com/watch?v=1"}
	for _, s := range yes {
		if !clipboardWorthy(s) {
			t.Errorf("должно ловиться: %s", s)
		}
	}
	for _, s := range no {
		if clipboardWorthy(s) {
			t.Errorf("не должно ловиться: %s", s)
		}
	}
}
