package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// runTransfers крутит планировщик «в Диск» и загрузки, пока всё не устаканится.
func runTransfers(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		a.forwardToDrive()
		a.scheduleUploads()
		a.mu.Lock()
		busy := false
		for _, j := range a.jobs {
			if j.Kind == "upload" && (j.Status == "waiting" || j.Status == "active" || j.up != nil) {
				busy = true
			}
			if j.Status == "retrying" {
				j.RetryAt = time.Now()
				busy = true
			}
			if j.Kind != "upload" && j.ToDrive != "" && (j.DriveStep == "" || j.DriveStep == "queued") {
				busy = true
			}
		}
		a.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("перенос на Диск не завершился")
}

func (f *fakeUp) folderPath(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := ""
	for id != "root" && id != "" {
		x, ok := f.files[id]
		if !ok {
			return "?" + id + p
		}
		p = "/" + x.name + p
		id = x.parent
	}
	return p
}

func (f *fakeUp) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, x := range f.files {
		if x.name == name {
			n++
		}
	}
	return n
}

func TestDriveTransferFile(t *testing.T) {
	a, fu := uploadApp(t)
	a.settings.normalize()
	p := filepath.Join(a.root(), "Прайсы", "asia.kg", "Прайс.xlsx")
	data := randFile(t, p, 3<<20)
	src := &Job{ID: "j1", Kind: "file", Status: "done", Path: p, Dir: `Прайсы\asia.kg`, OrigURL: "https://asia.kg/price.xlsx",
		Finished: time.Now(), ToDrive: "me@gmail.com"}
	a.jobs = []*Job{src}
	runTransfers(t, a)

	got := fu.find("Прайс.xlsx")
	if got == nil || !bytes.Equal(got.data, data) {
		t.Fatal("файл не загрузился на Диск")
	}
	if fp := fu.folderPath(got.parent); fp != "/Загрузки/Прайсы/asia.kg" {
		t.Errorf("не та папка на Диске: %s", fp)
	}
	if src.DriveStep != "done" || !src.LocalGone || src.DriveFile == "" {
		t.Errorf("итог закачки: step=%s gone=%v file=%q err=%s", src.DriveStep, src.LocalGone, src.DriveFile, src.DriveErr)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("копия на компьютере должна удалиться")
	}
	if _, err := os.Stat(filepath.Join(a.root(), "Прайсы")); err != nil {
		t.Error("папку категории удалять нельзя")
	}

	// та же ссылка ещё раз — «уже на Диске», без новой закачки
	if prev := a.downloadedBefore(src.OrigURL); prev != src {
		t.Fatal("ссылка, перенесённая на Диск, должна считаться скачанной")
	}
	d := a.addDuplicate(src.OrigURL, AddRequest{}, src)
	if d.Status != "duplicate" || !d.LocalGone || d.DriveStep != "done" || d.Path != "" {
		t.Errorf("повтор: %+v", d)
	}
}

func TestDriveTransferKeepAndParent(t *testing.T) {
	a, fu := uploadApp(t)
	a.settings.normalize()
	p := filepath.Join(a.root(), "Документы", "PDF", "Договор.pdf")
	randFile(t, p, 5000)
	src := &Job{ID: "j1", Kind: "file", Status: "done", Path: p, Dir: `Документы\PDF`, ToDrive: "me@gmail.com",
		DriveKeep: true, DriveParent: "P9", DriveParentName: "Мой диск › Договоры"}
	a.jobs = []*Job{src}
	runTransfers(t, a)
	got := fu.find("Договор.pdf")
	if got == nil || got.parent != "P9" {
		t.Fatal("файл должен лечь прямо в выбранную папку Диска")
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("просили оставить копию — она удалена")
	}
	if src.DriveStep != "done" || src.LocalGone {
		t.Errorf("итог: %s gone=%v", src.DriveStep, src.LocalGone)
	}
}

func TestDriveTransferTorrentFolder(t *testing.T) {
	a, fu := uploadApp(t)
	a.settings.normalize()
	dir := filepath.Join(a.root(), "Видео", "Сериал")
	for _, n := range []string{"s01e01.mkv", "s01e02.mkv", "s01e03.mkv", `Субтитры\ru.srt`} {
		randFile(t, filepath.Join(dir, n), 2000)
	}
	src := &Job{ID: "j1", Kind: "torrent", Status: "seeding", Path: dir, Dir: "Видео", ToDrive: "me@gmail.com"}
	a.jobs = []*Job{src}
	runTransfers(t, a)

	if src.Status != "done" {
		t.Errorf("раздача должна остановиться: %s", src.Status)
	}
	if src.DriveStep != "done" || src.DriveTotal != 4 || !src.LocalGone {
		t.Fatalf("итог: step=%s %d/%d gone=%v err=%s", src.DriveStep, src.DriveDone, src.DriveTotal, src.LocalGone, src.DriveErr)
	}
	srt := fu.find("ru.srt")
	if srt == nil || fu.folderPath(srt.parent) != "/Загрузки/Видео/Сериал/Субтитры" {
		t.Error("структура папок торрента на Диске не совпала")
	}
	// три загрузки шли одновременно — папки не должны задвоиться
	for _, n := range []string{"Загрузки", "Видео", "Сериал"} {
		if c := fu.count(n); c != 1 {
			t.Errorf("папка «%s» на Диске %d раз(а)", n, c)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("папка торрента на компьютере должна удалиться целиком")
	}
	if _, err := os.Stat(filepath.Join(a.root(), "Видео")); err != nil {
		t.Error("папку категории удалять нельзя")
	}
}

func TestDriveTransferDuplicateKeepsOriginal(t *testing.T) {
	a, fu := uploadApp(t)
	a.settings.normalize()
	p := filepath.Join(a.root(), "Книги", "book.pdf")
	randFile(t, p, 4000)
	// закачка оказалась дублем уже лежащего файла — его загружаем, но не удаляем
	src := &Job{ID: "j1", Kind: "file", Status: "duplicate", Path: p, DupOf: p, Dir: "Книги", ToDrive: "me@gmail.com"}
	a.jobs = []*Job{src}
	runTransfers(t, a)
	if fu.find("book.pdf") == nil || src.DriveStep != "done" {
		t.Fatal("дубль должен уйти на Диск")
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("файл-оригинал удалять нельзя")
	}
}

func TestDriveAddWithoutAccount(t *testing.T) {
	a := nativeApp(t)
	a.gd = newGDrive(t.TempDir())
	r := a.Add(AddRequest{URL: "https://example.com/a.zip", ToDrive: "1"})
	if r.OK || r.Error == "" {
		t.Error("без подключённого Диска «в Диск» должно отказывать")
	}
}

func TestDriveFolderSetting(t *testing.T) {
	s := defaultSettings()
	s.DriveFolder = ` \Архив\ / ..\Загрузки `
	s.normalize()
	if s.DriveFolder != "Архив/Загрузки" {
		t.Errorf("папка на Диске: %q", s.DriveFolder)
	}
}
