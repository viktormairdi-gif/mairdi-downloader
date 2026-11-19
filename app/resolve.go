package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------- облачные ссылки → прямые ----------

var (
	reGDriveFile = regexp.MustCompile(`drive\.google\.com/(?:file/d/|open\?id=|uc\?(?:.*&)?id=)([\w-]{20,})`)
	reGDocs      = regexp.MustCompile(`docs\.google\.com/(document|spreadsheets|presentation)/d/([\w-]{20,})`)
	reGHBlob     = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/blob/(.+)$`)
)

type resolved struct {
	URL      string
	FileName string
	Note     string
}

// resolveCloud превращает ссылку «на просмотр» в облаке в прямую ссылку на файл.
func resolveCloud(raw string) (resolved, error) {
	r := resolved{URL: raw}
	u, err := url.Parse(raw)
	if err != nil {
		return r, nil
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch {
	case reGDriveFile.MatchString(raw):
		id := reGDriveFile.FindStringSubmatch(raw)[1]
		r.URL = "https://drive.usercontent.google.com/download?id=" + id + "&export=download&confirm=t"
		r.Note = "Google Диск"
	case strings.Contains(raw, "drive.google.com/drive/folders/"):
		return r, errors.New("папки Google Диска целиком пока не поддерживаются — откройте папку и скачайте файлы по одному или архивом")
	case reGDocs.MatchString(raw):
		m := reGDocs.FindStringSubmatch(raw)
		switch m[1] {
		case "document":
			r.URL = "https://docs.google.com/document/d/" + m[2] + "/export?format=docx"
		case "spreadsheets":
			r.URL = "https://docs.google.com/spreadsheets/d/" + m[2] + "/export?format=xlsx"
		case "presentation":
			r.URL = "https://docs.google.com/presentation/d/" + m[2] + "/export/pptx"
		}
		r.Note = "Google Документы"
	case h == "yadi.sk" || ((strings.HasPrefix(h, "disk.yandex.") || strings.HasPrefix(h, "disk.360.yandex.")) && (strings.HasPrefix(u.Path, "/d/") || strings.HasPrefix(u.Path, "/i/"))):
		href, name, err := yandexDiskLink(raw)
		if err != nil {
			return r, err
		}
		r.URL, r.FileName, r.Note = href, name, "Яндекс.Диск"
	case h == "dropbox.com" || strings.HasSuffix(h, ".dropbox.com"):
		q := u.Query()
		q.Set("dl", "1")
		u.RawQuery = q.Encode()
		r.URL, r.Note = u.String(), "Dropbox"
	case h == "1drv.ms" || h == "onedrive.live.com":
		enc := base64.RawURLEncoding.EncodeToString([]byte(raw))
		r.URL, r.Note = "https://api.onedrive.com/v1.0/shares/u!"+enc+"/root/content", "OneDrive"
	case reGHBlob.MatchString(raw):
		m := reGHBlob.FindStringSubmatch(raw)
		r.URL, r.Note = "https://raw.githubusercontent.com/"+m[1]+"/"+m[2]+"/"+m[3], "GitHub"
	case h == "cloud.mail.ru" && strings.HasPrefix(u.Path, "/public/"):
		href, err := mailruLink(strings.TrimPrefix(u.Path, "/public/"))
		if err != nil {
			return r, err
		}
		r.URL, r.Note = href, "Облако Mail.ru"
	case h == "mega.nz" || h == "mega.io":
		return r, errors.New("MEGA шифрует файлы на стороне браузера — такие ссылки пока не поддерживаются")
	}
	return r, nil
}

var httpc = &http.Client{Timeout: 20 * time.Second}

func yandexDiskLink(public string) (string, string, error) {
	api := "https://cloud-api.yandex.net/v1/disk/public/resources/download?public_key=" + url.QueryEscape(public)
	resp, err := httpc.Get(api)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var j struct {
		Href    string `json:"href"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&j)
	if j.Href == "" {
		if j.Message == "" {
			j.Message = "Яндекс.Диск не отдал ссылку (HTTP " + strconv.Itoa(resp.StatusCode) + ")"
		}
		return "", "", errors.New(j.Message)
	}
	name := ""
	if hu, err := url.Parse(j.Href); err == nil {
		name = hu.Query().Get("filename")
	}
	return j.Href, name, nil
}

func mailruLink(publicPath string) (string, error) {
	resp, err := httpc.Get("https://cloud.mail.ru/api/v2/dispatcher/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var j struct {
		Body struct {
			WeblinkGet []struct {
				URL string `json:"url"`
			} `json:"weblink_get"`
		} `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil || len(j.Body.WeblinkGet) == 0 {
		return "", errors.New("Облако Mail.ru не отдало ссылку")
	}
	return strings.TrimRight(j.Body.WeblinkGet[0].URL, "/") + "/" + publicPath, nil
}

// ---------- видео-сайты ----------

var videoHosts = []string{"youtube.com", "youtu.be", "instagram.com", "tiktok.com", "vk.com", "vkvideo.ru", "vk.ru",
	"rutube.ru", "facebook.com", "fb.watch", "x.com", "twitter.com", "vimeo.com", "dailymotion.com", "ok.ru",
	"twitch.tv", "soundcloud.com", "reddit.com", "bilibili.com", "dzen.ru", "kick.com", "threads.net", "coub.com",
	"pinterest.com", "music.youtube.com", "t.me"}

func isVideoSite(raw string) bool {
	h := hostOf(raw)
	if h == "" {
		return false
	}
	for _, v := range videoHosts {
		if h == v || strings.HasSuffix(h, "."+v) {
			return true
		}
	}
	return false
}

// Ссылка ведёт прямо на файл (по расширению в пути)?
func looksLikeFileURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	e := strings.ToLower(pathExt(u.Path))
	if e == "" || e == ".html" || e == ".htm" || e == ".php" || e == ".aspx" || e == ".asp" || e == ".jsp" {
		return false
	}
	return true
}

func pathExt(p string) string {
	i := strings.LastIndex(p, ".")
	j := strings.LastIndex(p, "/")
	if i < 0 || i < j || len(p)-i > 10 {
		return ""
	}
	return p[i:]
}

// ---------- торрент-файл: имя и список файлов ----------

type bnode struct {
	s    []byte
	i    int64
	list []*bnode
	dict map[string]*bnode
	kind byte
}

func bdecode(b []byte, pos int) (*bnode, int, error) {
	if pos >= len(b) {
		return nil, pos, errors.New("eof")
	}
	switch c := b[pos]; {
	case c == 'i':
		end := bytes.IndexByte(b[pos:], 'e')
		if end < 0 {
			return nil, pos, errors.New("bad int")
		}
		v, _ := strconv.ParseInt(string(b[pos+1:pos+end]), 10, 64)
		return &bnode{kind: 'i', i: v}, pos + end + 1, nil
	case c == 'l':
		n := &bnode{kind: 'l'}
		pos++
		for pos < len(b) && b[pos] != 'e' {
			ch, np, err := bdecode(b, pos)
			if err != nil {
				return nil, np, err
			}
			n.list = append(n.list, ch)
			pos = np
		}
		return n, pos + 1, nil
	case c == 'd':
		n := &bnode{kind: 'd', dict: map[string]*bnode{}}
		pos++
		for pos < len(b) && b[pos] != 'e' {
			k, np, err := bdecode(b, pos)
			if err != nil || k.kind != 's' {
				return nil, np, errors.New("bad key")
			}
			v, np2, err := bdecode(b, np)
			if err != nil {
				return nil, np2, err
			}
			n.dict[string(k.s)] = v
			pos = np2
		}
		return n, pos + 1, nil
	case c >= '0' && c <= '9':
		colon := bytes.IndexByte(b[pos:], ':')
		if colon < 0 {
			return nil, pos, errors.New("bad string")
		}
		l, _ := strconv.Atoi(string(b[pos : pos+colon]))
		start := pos + colon + 1
		if start+l > len(b) {
			return nil, pos, errors.New("short string")
		}
		return &bnode{kind: 's', s: b[start : start+l]}, start + l, nil
	}
	return nil, pos, fmt.Errorf("bad token at %d", pos)
}

func parseTorrent(data []byte) (string, []tFile, error) {
	root, _, err := bdecode(data, 0)
	if err != nil || root.kind != 'd' {
		return "", nil, errors.New("это не торрент-файл")
	}
	info := root.dict["info"]
	if info == nil || info.kind != 'd' {
		return "", nil, errors.New("в торренте нет раздела info")
	}
	name := ""
	if n := info.dict["name.utf-8"]; n != nil {
		name = string(n.s)
	} else if n := info.dict["name"]; n != nil {
		name = string(n.s)
	}
	var files []tFile
	if l := info.dict["length"]; l != nil {
		files = append(files, tFile{Name: name, Size: l.i})
	}
	if fl := info.dict["files"]; fl != nil {
		for _, f := range fl.list {
			var parts []string
			p := f.dict["path.utf-8"]
			if p == nil {
				p = f.dict["path"]
			}
			if p != nil {
				for _, s := range p.list {
					parts = append(parts, string(s.s))
				}
			}
			var size int64
			if l := f.dict["length"]; l != nil {
				size = l.i
			}
			files = append(files, tFile{Name: strings.Join(parts, "/"), Size: size})
		}
	}
	return name, files, nil
}
