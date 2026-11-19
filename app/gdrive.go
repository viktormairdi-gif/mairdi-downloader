package main

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Google Диск как файловый менеджер: просмотр, поиск и скачивание через свой движок.

const gFolderMime = "application/vnd.google-apps.folder"
const gShortcutMime = "application/vnd.google-apps.shortcut"
const gFileFields = "id,name,mimeType,size,modifiedTime,iconLink,thumbnailLink,webViewLink,starred,trashed,driveId," +
	"owners(displayName,me),shortcutDetails(targetId,targetMimeType),exportLinks,shared,parents," +
	"capabilities(canDownload,canRename,canTrash,canUntrash,canDelete,canShare,canMoveItemWithinDrive,canAddChildren,canCopy)"

type gRawFile struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	MimeType      string `json:"mimeType"`
	Size          string `json:"size"`
	ModifiedTime  string `json:"modifiedTime"`
	IconLink      string `json:"iconLink"`
	ThumbnailLink string `json:"thumbnailLink"`
	WebViewLink   string `json:"webViewLink"`
	Starred       bool   `json:"starred"`
	Trashed       bool   `json:"trashed"`
	DriveID       string `json:"driveId"`
	Owners        []struct {
		DisplayName string `json:"displayName"`
		Me          bool   `json:"me"`
	} `json:"owners"`
	ShortcutDetails *struct {
		TargetID       string `json:"targetId"`
		TargetMimeType string `json:"targetMimeType"`
	} `json:"shortcutDetails"`
	ExportLinks  map[string]string `json:"exportLinks"`
	Shared       bool              `json:"shared"`
	Parents      []string          `json:"parents"`
	Capabilities *struct {
		CanDownload            bool `json:"canDownload"`
		CanRename              bool `json:"canRename"`
		CanTrash               bool `json:"canTrash"`
		CanUntrash             bool `json:"canUntrash"`
		CanDelete              bool `json:"canDelete"`
		CanShare               bool `json:"canShare"`
		CanMoveItemWithinDrive bool `json:"canMoveItemWithinDrive"`
		CanAddChildren         bool `json:"canAddChildren"`
		CanCopy                bool `json:"canCopy"`
	} `json:"capabilities"`
}

// GFile — то, что видит окно.
type GFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mime     string `json:"mime"`
	Folder   bool   `json:"folder"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Icon     string `json:"icon"`
	Thumb    string `json:"thumb"`
	Link     string `json:"link"`
	Owner    string `json:"owner"`
	Starred  bool   `json:"starred"`
	DriveID  string `json:"driveId,omitempty"`
	Shortcut bool   `json:"shortcut,omitempty"`
	Kind     string `json:"kind"` // как скачается: «docx», «xlsx»… или пусто — как есть
	NoDown   bool   `json:"noDown,omitempty"`
	Self     string `json:"self,omitempty"` // у ярлыка — ID самого ярлыка (ID = цель); операции идут над ярлыком
	Shared   bool   `json:"shared,omitempty"`
	Parent   string `json:"parent,omitempty"`
	Can      string `json:"can,omitempty"` // r — переименовать, t — в корзину, u — восстановить, d — удалить навсегда, s — доступ, m — переместить, a — класть внутрь, c — копия
}

func (r *gRawFile) toFile() GFile {
	f := GFile{ID: r.ID, Name: r.Name, Mime: r.MimeType, Modified: r.ModifiedTime, Icon: r.IconLink,
		Thumb: r.ThumbnailLink, Link: r.WebViewLink, Starred: r.Starred, DriveID: r.DriveID}
	f.Size, _ = strconv.ParseInt(r.Size, 10, 64)
	mime := r.MimeType
	f.Shared = r.Shared
	if len(r.Parents) > 0 {
		f.Parent = r.Parents[0]
	}
	if c := r.Capabilities; c != nil {
		for _, x := range []struct {
			on bool
			ch string
		}{{c.CanRename, "r"}, {c.CanTrash, "t"}, {c.CanUntrash, "u"}, {c.CanDelete, "d"}, {c.CanShare, "s"},
			{c.CanMoveItemWithinDrive, "m"}, {c.CanAddChildren, "a"}, {c.CanCopy, "c"}} {
			if x.on {
				f.Can += x.ch
			}
		}
	}
	if r.ShortcutDetails != nil && r.ShortcutDetails.TargetID != "" {
		f.Shortcut = true
		f.Self = r.ID
		f.ID = r.ShortcutDetails.TargetID
		mime = r.ShortcutDetails.TargetMimeType
		f.Mime = mime
	}
	f.Folder = mime == gFolderMime
	for _, o := range r.Owners {
		if o.Me {
			f.Owner = "я"
		} else if f.Owner == "" {
			f.Owner = o.DisplayName
		}
	}
	if ex, ok := gExport[mime]; ok {
		f.Kind = ex.ext
	} else if strings.HasPrefix(mime, "application/vnd.google-apps.") && !f.Folder {
		f.NoDown = true
	}
	if r.Capabilities != nil && !r.Capabilities.CanDownload && !f.Folder {
		f.NoDown = true
	}
	return f
}

type gExportFmt struct{ mime, ext string }

// Документы Google скачиваются в привычных форматах Office.
var gExport = map[string]gExportFmt{
	"application/vnd.google-apps.document":     {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "docx"},
	"application/vnd.google-apps.spreadsheet":  {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "xlsx"},
	"application/vnd.google-apps.presentation": {"application/vnd.openxmlformats-officedocument.presentationml.presentation", "pptx"},
	"application/vnd.google-apps.drawing":      {"image/png", "png"},
	"application/vnd.google-apps.script":       {"application/vnd.google-apps.script+json", "json"},
}

func gQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

type GListReq struct {
	Acc         string `json:"acc"`
	Section     string `json:"section"` // my | shared | drives | starred | recent | trash
	Folder      string `json:"folder"`
	DriveID     string `json:"driveId"`
	Q           string `json:"q"`
	PageToken   string `json:"pageToken"`
	Order       string `json:"order"`       // name | modified | size (+ "-" в конце — по убыванию)
	FoldersOnly bool   `json:"foldersOnly"` // выбор папки для перемещения
}

// Сортировка на стороне Google — иначе при постраничной загрузке порядок «плывёт».
func gOrderBy(order string) string {
	desc := strings.HasSuffix(order, "-")
	key := map[string]string{"name": "name_natural", "modified": "modifiedTime", "size": "quotaBytesUsed"}[strings.TrimSuffix(order, "-")]
	if key == "" {
		return ""
	}
	if desc {
		key += " desc"
	}
	return "folder," + key
}

type GListResp struct {
	Files []GFile `json:"files"`
	Next  string  `json:"next"`
}

func (g *GDrive) list(r GListReq) (*GListResp, error) {
	if r.Section == "drives" && r.Folder == "" && r.Q == "" {
		return g.listDrives(r)
	}
	q := url.Values{
		"pageSize": {"200"}, "fields": {"nextPageToken,files(" + gFileFields + ")"},
		"supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}, "orderBy": {"folder,name_natural"},
	}
	if r.PageToken != "" {
		q.Set("pageToken", r.PageToken)
	}
	switch {
	case strings.TrimSpace(r.Q) != "":
		q.Set("q", "name contains "+gQuote(strings.TrimSpace(r.Q))+" and trashed=false")
		q.Set("corpora", "allDrives")
		q.Set("orderBy", "folder,modifiedTime desc")
	case r.Folder != "":
		q.Set("q", gQuote(r.Folder)+" in parents and trashed=false")
		if r.DriveID != "" {
			q.Set("corpora", "drive")
			q.Set("driveId", r.DriveID)
		}
	case r.Section == "shared":
		q.Set("q", "sharedWithMe and trashed=false")
	case r.Section == "starred":
		q.Set("q", "starred and trashed=false")
	case r.Section == "recent":
		q.Set("q", "trashed=false and mimeType != "+gQuote(gFolderMime))
		q.Set("orderBy", "viewedByMeTime desc")
		q.Set("pageSize", "100")
	case r.Section == "trash":
		q.Set("q", "trashed=true")
	default:
		q.Set("q", "'root' in parents and trashed=false")
	}
	if r.FoldersOnly {
		q.Set("q", q.Get("q")+" and mimeType = "+gQuote(gFolderMime))
	}
	if ob := gOrderBy(r.Order); ob != "" && r.Section != "shared" {
		q.Set("orderBy", ob)
	}
	var out struct {
		NextPageToken string     `json:"nextPageToken"`
		Files         []gRawFile `json:"files"`
	}
	if err := g.call(r.Acc, "GET", "/drive/v3/files", q, nil, &out); err != nil {
		return nil, err
	}
	res := &GListResp{Files: []GFile{}, Next: out.NextPageToken}
	if r.Section == "recent" {
		res.Next = "" // «Недавние» — одна страница, как в самом Диске
	}
	for i := range out.Files {
		res.Files = append(res.Files, out.Files[i].toFile())
	}
	return res, nil
}

func (g *GDrive) listDrives(r GListReq) (*GListResp, error) {
	q := url.Values{"pageSize": {"100"}}
	if r.PageToken != "" {
		q.Set("pageToken", r.PageToken)
	}
	var out struct {
		NextPageToken string `json:"nextPageToken"`
		Drives        []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"drives"`
	}
	if err := g.call(r.Acc, "GET", "/drive/v3/drives", q, nil, &out); err != nil {
		return nil, err
	}
	res := &GListResp{Files: []GFile{}, Next: out.NextPageToken}
	for _, d := range out.Drives {
		res.Files = append(res.Files, GFile{ID: d.ID, Name: d.Name, Mime: gFolderMime, Folder: true, DriveID: d.ID, Owner: "общий диск"})
	}
	return res, nil
}

type GAbout struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Photo string `json:"photo"`
	Usage int64  `json:"usage"`
	Limit int64  `json:"limit"` // 0 — без ограничения
	Trash int64  `json:"trash"`
}

func (g *GDrive) about(acc string) (*GAbout, error) {
	var out struct {
		User struct {
			DisplayName  string `json:"displayName"`
			EmailAddress string `json:"emailAddress"`
			PhotoLink    string `json:"photoLink"`
		} `json:"user"`
		StorageQuota struct {
			Limit             string `json:"limit"`
			Usage             string `json:"usage"`
			UsageInDriveTrash string `json:"usageInDriveTrash"`
		} `json:"storageQuota"`
	}
	if err := g.call(acc, "GET", "/drive/v3/about", url.Values{"fields": {"user,storageQuota"}}, nil, &out); err != nil {
		return nil, err
	}
	a := &GAbout{Email: out.User.EmailAddress, Name: out.User.DisplayName, Photo: out.User.PhotoLink}
	a.Usage, _ = strconv.ParseInt(out.StorageQuota.Usage, 10, 64)
	a.Limit, _ = strconv.ParseInt(out.StorageQuota.Limit, 10, 64)
	a.Trash, _ = strconv.ParseInt(out.StorageQuota.UsageInDriveTrash, 10, 64)
	return a, nil
}

func (g *GDrive) meta(acc, id string) (*gRawFile, error) {
	var f gRawFile
	err := g.call(acc, "GET", "/drive/v3/files/"+url.PathEscape(id),
		url.Values{"fields": {gFileFields}, "supportsAllDrives": {"true"}}, nil, &f)
	if err != nil {
		return nil, err
	}
	if f.ShortcutDetails != nil && f.ShortcutDetails.TargetID != "" {
		return g.meta(acc, f.ShortcutDetails.TargetID)
	}
	return &f, nil
}

// children — всё содержимое папки (все страницы).
func (g *GDrive) children(acc, id string) ([]gRawFile, error) {
	var all []gRawFile
	tok := ""
	for {
		q := url.Values{"q": {gQuote(id) + " in parents and trashed=false"}, "pageSize": {"1000"},
			"fields": {"nextPageToken,files(" + gFileFields + ")"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
		if tok != "" {
			q.Set("pageToken", tok)
		}
		var out struct {
			NextPageToken string     `json:"nextPageToken"`
			Files         []gRawFile `json:"files"`
		}
		if err := g.call(acc, "GET", "/drive/v3/files", q, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Files...)
		if out.NextPageToken == "" {
			return all, nil
		}
		tok = out.NextPageToken
	}
}

// ссылка, по которой свой движок скачает файл (с поддержкой докачки для обычных файлов)
func gDownloadURL(f *gRawFile) (u, name string, err error) {
	if ex, ok := gExport[f.MimeType]; ok {
		name = f.Name
		if !strings.HasSuffix(strings.ToLower(name), "."+ex.ext) {
			name += "." + ex.ext
		}
		if l := f.ExportLinks[ex.mime]; l != "" {
			return l, name, nil // у ссылок выгрузки нет лимита 10 МБ, как у API
		}
		return gAPI + "/drive/v3/files/" + url.PathEscape(f.ID) + "/export?mimeType=" + url.QueryEscape(ex.mime), name, nil
	}
	if strings.HasPrefix(f.MimeType, "application/vnd.google-apps.") {
		return "", "", errors.New("«" + f.Name + "» — этот тип Google (форма, сайт, карта) скачать нельзя")
	}
	if f.Capabilities != nil && !f.Capabilities.CanDownload {
		return "", "", errors.New("владелец запретил скачивать «" + f.Name + "»")
	}
	return gAPI + "/drive/v3/files/" + url.PathEscape(f.ID) + "?alt=media&supportsAllDrives=true&acknowledgeAbuse=false", f.Name, nil
}

type GDownloadRes struct {
	OK      bool     `json:"ok"`
	Added   int      `json:"added"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors,omitempty"`
}

const gFolderLimit = 20000

// DriveDownload ставит в очередь файлы и папки Диска. Одиночные файлы раскладываются
// по папкам как обычно; папка целиком ложится в «Google Диск\<папка>» со своей структурой.
func (a *App) DriveDownload(acc string, ids []string) GDownloadRes {
	res := GDownloadRes{}
	for _, id := range ids {
		f, err := a.gd.meta(acc, id)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		if f.MimeType != gFolderMime {
			if err := a.addDriveFile(acc, f, "", false); err != nil {
				res.Errors = append(res.Errors, err.Error())
			} else {
				res.Added++
			}
			continue
		}
		base := filepath.Join("Google Диск", safeName(f.Name))
		type item struct {
			id, dir string
		}
		queue := []item{{f.ID, base}}
		count := 0
		for len(queue) > 0 && count < gFolderLimit {
			it := queue[0]
			queue = queue[1:]
			kids, err := a.gd.children(acc, it.id)
			if err != nil {
				res.Errors = append(res.Errors, err.Error())
				continue
			}
			_ = os.MkdirAll(filepath.Join(a.root(), it.dir), 0o755) // пустые папки тоже переносим
			for i := range kids {
				k := &kids[i]
				if k.ShortcutDetails != nil && k.ShortcutDetails.TargetID != "" {
					if t, err := a.gd.meta(acc, k.ShortcutDetails.TargetID); err == nil {
						t.Name = k.Name
						k = t
					} else {
						continue
					}
				}
				if k.MimeType == gFolderMime {
					queue = append(queue, item{k.ID, filepath.Join(it.dir, safeName(k.Name))})
					continue
				}
				count++
				if count > gFolderLimit {
					res.Errors = append(res.Errors, "в папке больше "+strconv.Itoa(gFolderLimit)+" файлов — остальные пропущены")
					break
				}
				// уже лежит на месте с тем же размером — повторно не качаем
				_, name, err := gDownloadURL(k)
				if err != nil {
					res.Errors = append(res.Errors, err.Error())
					continue
				}
				sz, _ := strconv.ParseInt(k.Size, 10, 64)
				if st, err := os.Stat(filepath.Join(a.root(), it.dir, safeName(name))); err == nil && (sz == 0 || st.Size() == sz) {
					res.Skipped++
					continue
				}
				if err := a.addDriveFile(acc, k, it.dir, true); err != nil {
					res.Errors = append(res.Errors, err.Error())
				} else {
					res.Added++
				}
			}
		}
	}
	res.OK = res.Added > 0 || res.Skipped > 0
	if res.Added > 0 {
		a.sawWork = true
		go a.saveState()
	}
	return res
}

func (a *App) addDriveFile(acc string, f *gRawFile, dir string, inFolder bool) error {
	u, name, err := gDownloadURL(f)
	if err != nil {
		return err
	}
	orig := "https://drive.google.com/file/d/" + f.ID + "/view"
	req := AddRequest{Source: "gdrive", FileName: name, Force: inFolder}
	if !inFolder && a.getSettings().SkipDupes {
		if prev := a.downloadedBefore(orig); prev != nil {
			a.addDuplicate(orig, req, prev)
			return nil
		}
		sz, _ := strconv.ParseInt(f.Size, 10, 64)
		if sz > 0 && a.lib != nil {
			if ex := a.library().findByNameSize(safeName(name), sz); ex != "" {
				a.addLibDuplicate(orig, req, name, ex, sz)
				return nil
			}
		}
	}
	j := a.baseJob(orig, req)
	j.Kind, j.Engine, j.URL, j.Note = "file", "native", u, "Google Диск"
	j.GAcc = acc
	j.Name = name
	j.Domain = "drive.google.com"
	j.Total, _ = strconv.ParseInt(f.Size, 10, 64)
	if dir != "" {
		j.ForcedDir = dir
	}
	a.mu.Lock()
	j.ID = a.newID()
	a.jobs = append(a.jobs, j)
	a.markDirty()
	a.mu.Unlock()
	return a.startFile(j, false)
}

// Такой файл (имя + размер) уже есть в папке загрузок.
func (a *App) addLibDuplicate(u string, req AddRequest, name, existing string, size int64) {
	j := a.baseJob(u, req)
	j.Kind, j.Name, j.Path, j.DupOf, j.Status = "file", name, existing, existing, "duplicate"
	j.Dir = relTo(a.root(), filepath.Dir(existing))
	j.Category = topCategory(j.Dir)
	j.Total, j.Done, j.Finished = size, size, time.Now()
	j.Reason = "Файл с таким именем и размером уже есть — повторно не качаю"
	a.mu.Lock()
	j.ID = a.newID()
	a.jobs = append(a.jobs, j)
	a.markDirty()
	a.mu.Unlock()
}

// Превью файлов Диска: окно не может само приложить токен, поэтому картинку отдаёт программа.
func (a *App) handleGThumb(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !a.hostOK(r) || q.Get("t") != a.token {
		http.Error(w, "forbidden", 403)
		return
	}
	u, err := url.Parse(q.Get("u"))
	if err != nil || u.Scheme != "https" || !(strings.HasSuffix(u.Hostname(), ".googleusercontent.com") || u.Hostname() == "drive.google.com") {
		http.Error(w, "bad url", 400)
		return
	}
	tok, err := a.gd.accessToken(q.Get("acc"))
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), "GET", u.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.gd.client.Do(req)
	if err != nil {
		http.Error(w, "fetch", 502)
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != 200 || !strings.HasPrefix(ct, "image/") {
		http.Error(w, "no thumb", 404)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 10<<20))
}

// ссылку «Открыть в браузере» берём у Google, а не из окна
func (a *App) driveOpen(acc, id string) error {
	f, err := a.gd.meta(acc, id)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(f.WebViewLink, "https://") {
		return errors.New("у файла нет ссылки для просмотра")
	}
	shellOpen(f.WebViewLink)
	return nil
}
