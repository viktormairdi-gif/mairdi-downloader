package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Поддельный Google для операций с файлами: папки, корзина, перемещение, доступ.
type fakeOpsFile struct {
	Name, Mime       string
	Parents          []string
	Trashed, Starred bool
	Perms            []GPerm
	deleted          bool
}

type fakeOps struct {
	mu    sync.Mutex
	n     int
	files map[string]*fakeOpsFile
	calls []string
}

func (f *fakeOps) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "AT1", "expires_in": 3600})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	p := strings.TrimPrefix(r.URL.Path, "/drive/v3/files")
	newID := func() string { f.n++; return "n" + strconv.Itoa(f.n) }
	switch {
	case p == "" && r.Method == "POST":
		id := newID()
		ps := []string{}
		for _, x := range body["parents"].([]any) {
			ps = append(ps, x.(string))
		}
		f.files[id] = &fakeOpsFile{Name: body["name"].(string), Mime: body["mimeType"].(string), Parents: ps}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	case p == "/trash" && r.Method == "DELETE":
		for id, x := range f.files {
			if x.Trashed {
				delete(f.files, id)
			}
		}
		w.WriteHeader(204)
	case strings.Contains(p, "/permissions"):
		parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
		x := f.files[parts[0]]
		if x == nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{"permissions": func() []map[string]any {
				out := []map[string]any{}
				for _, q := range x.Perms {
					out = append(out, map[string]any{"id": q.ID, "type": q.Type, "role": q.Role, "emailAddress": q.Email})
				}
				return out
			}()})
		case r.Method == "POST":
			q := GPerm{ID: newID(), Type: body["type"].(string), Role: body["role"].(string)}
			if e, ok := body["emailAddress"].(string); ok {
				q.Email = e
			}
			x.Perms = append(x.Perms, q)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": q.ID})
		case r.Method == "PATCH" || r.Method == "DELETE":
			for i := range x.Perms {
				if x.Perms[i].ID == parts[2] {
					if r.Method == "DELETE" {
						x.Perms = append(x.Perms[:i], x.Perms[i+1:]...)
					} else {
						x.Perms[i].Role = body["role"].(string)
					}
					break
				}
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte("{}"))
		}
	case strings.HasSuffix(p, "/copy"):
		src := f.files[strings.Trim(strings.TrimSuffix(p, "/copy"), "/")]
		c := *src
		c.Perms = nil
		if ps, ok := body["parents"].([]any); ok {
			c.Parents = []string{ps[0].(string)}
		}
		if n, ok := body["name"].(string); ok {
			c.Name = n
		}
		id := newID()
		f.files[id] = &c
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	default:
		id := strings.TrimPrefix(p, "/")
		x := f.files[id]
		if x == nil {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": x.Name, "parents": x.Parents, "trashed": x.Trashed, "webViewLink": "https://drive/" + id})
		case "DELETE":
			delete(f.files, id)
			w.WriteHeader(204)
		case "PATCH":
			if v, ok := body["name"].(string); ok {
				x.Name = v
			}
			if v, ok := body["trashed"].(bool); ok {
				x.Trashed = v
			}
			if v, ok := body["starred"].(bool); ok {
				x.Starred = v
			}
			if add := r.URL.Query().Get("addParents"); add != "" {
				rm := strings.Split(r.URL.Query().Get("removeParents"), ",")
				keep := []string{}
				for _, q := range x.Parents {
					drop := false
					for _, z := range rm {
						drop = drop || z == q
					}
					if !drop {
						keep = append(keep, q)
					}
				}
				x.Parents = append(keep, add)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		}
	}
}

func opsApp(t *testing.T) (*GDrive, *fakeOps) {
	fo := &fakeOps{files: map[string]*fakeOpsFile{
		"A":   {Name: "Отчёт.xlsx", Parents: []string{"root"}, Perms: []GPerm{{ID: "p0", Type: "user", Role: "owner", Email: "me@gmail.com"}, {ID: "pa", Type: "anyone", Role: "reader"}}},
		"B":   {Name: "Фото.jpg", Parents: []string{"root"}},
		"DIR": {Name: "Архив", Mime: gFolderMime, Parents: []string{"root"}},
	}}
	srv := httptest.NewServer(http.HandlerFunc(fo.handler))
	t.Cleanup(srv.Close)
	oldAPI, oldTok := gAPI, gTokenURL
	gAPI, gTokenURL = srv.URL, srv.URL+"/token"
	t.Cleanup(func() { gAPI, gTokenURL = oldAPI, oldTok })
	g := newGDrive(t.TempDir())
	enc, _ := dpapiProtect([]byte("RT"))
	g.cfg = GConfig{ClientID: "x.apps.googleusercontent.com", ClientSecret: "SEC", Accounts: []*GAccount{{Email: "me@gmail.com", RefreshEnc: enc}}}
	return g, fo
}

func TestDriveOpsFolderRenameMove(t *testing.T) {
	g, fo := opsApp(t)
	acc := "me@gmail.com"
	r := g.op(GOpReq{Acc: acc, Op: "mkdir", To: "root", Name: "  Документы  "})
	if !r.OK || fo.files[r.ID] == nil || fo.files[r.ID].Name != "Документы" || fo.files[r.ID].Mime != gFolderMime {
		t.Fatalf("mkdir: %+v", r)
	}
	if r := g.op(GOpReq{Acc: acc, Op: "mkdir", Name: "   "}); r.OK {
		t.Fatal("пустое имя папки прошло")
	}
	if r := g.op(GOpReq{Acc: acc, Op: "rename", IDs: []string{"A"}, Name: "Отчёт 2026.xlsx"}); !r.OK || fo.files["A"].Name != "Отчёт 2026.xlsx" {
		t.Fatalf("rename: %+v", r)
	}
	mv := g.op(GOpReq{Acc: acc, Op: "move", IDs: []string{"A", "B"}, To: "DIR"})
	if !mv.OK || mv.Done != 2 || fo.files["A"].Parents[0] != "DIR" || len(fo.files["A"].Parents) != 1 {
		t.Fatalf("move: %+v %v", mv, fo.files["A"].Parents)
	}
	if mv.From["A"] != "root" || mv.From["B"] != "root" {
		t.Fatalf("move не вернул, откуда взял: %v", mv.From)
	}
	// перемещение не трогает доступ — публичная ссылка остаётся
	if len(fo.files["A"].Perms) != 2 {
		t.Fatal("перемещение сбросило доступ")
	}
	if r := g.op(GOpReq{Acc: acc, Op: "move", IDs: []string{"DIR"}, To: "DIR"}); r.OK {
		t.Fatal("папку положили в саму себя")
	}
	if r := g.op(GOpReq{Acc: acc, Op: "copy", IDs: []string{"B"}, To: "root"}); !r.OK || r.Done != 1 {
		t.Fatalf("copy: %+v", r)
	}
}

func TestDriveOpsTrashOnlyDeletesTrashed(t *testing.T) {
	g, fo := opsApp(t)
	acc := "me@gmail.com"
	// живой файл навсегда не удаляется
	if r := g.op(GOpReq{Acc: acc, Op: "delete", IDs: []string{"B"}}); r.OK || fo.files["B"] == nil {
		t.Fatalf("удалили файл не из корзины: %+v", r)
	}
	if r := g.op(GOpReq{Acc: acc, Op: "trash", IDs: []string{"A", "B"}}); !r.OK || !fo.files["A"].Trashed || !fo.files["B"].Trashed {
		t.Fatalf("trash: %+v", r)
	}
	if r := g.op(GOpReq{Acc: acc, Op: "untrash", IDs: []string{"A"}}); !r.OK || fo.files["A"].Trashed {
		t.Fatalf("untrash: %+v", r)
	}
	if r := g.op(GOpReq{Acc: acc, Op: "delete", IDs: []string{"B"}}); !r.OK || fo.files["B"] != nil {
		t.Fatalf("delete из корзины: %+v", r)
	}
	g.op(GOpReq{Acc: acc, Op: "trash", IDs: []string{"DIR"}})
	if r := g.op(GOpReq{Acc: acc, Op: "empty-trash"}); !r.OK || fo.files["DIR"] != nil || fo.files["A"] == nil {
		t.Fatalf("empty-trash: %+v", r)
	}
	if r := g.op(GOpReq{Acc: acc, Op: "star", IDs: []string{"A"}, On: true}); !r.OK || !fo.files["A"].Starred {
		t.Fatalf("star: %+v", r)
	}
	if r := g.op(GOpReq{Acc: acc, Op: "trash", IDs: []string{"NOPE"}}); r.OK || r.Error == "" {
		t.Fatalf("ошибка не вернулась: %+v", r)
	}
}

func TestDriveSharing(t *testing.T) {
	g, fo := opsApp(t)
	acc := "me@gmail.com"
	ps, link, err := g.perms(acc, "A")
	if err != nil || len(ps) != 2 || link == "" {
		t.Fatalf("perms: %v %v %q", err, ps, link)
	}
	// ссылка: читатель → редактор (то же разрешение, без пересоздания) → закрыть
	if err := g.setLinkAccess(acc, "A", "writer"); err != nil || len(fo.files["A"].Perms) != 2 || fo.files["A"].Perms[1].Role != "writer" || fo.files["A"].Perms[1].ID != "pa" {
		t.Fatalf("link writer: %v %+v", err, fo.files["A"].Perms)
	}
	if err := g.setLinkAccess(acc, "A", ""); err != nil || len(fo.files["A"].Perms) != 1 {
		t.Fatalf("link off: %v %+v", err, fo.files["A"].Perms)
	}
	if err := g.setLinkAccess(acc, "B", "reader"); err != nil || len(fo.files["B"].Perms) != 1 || fo.files["B"].Perms[0].Type != "anyone" {
		t.Fatalf("link on: %v %+v", err, fo.files["B"].Perms)
	}
	if err := g.setLinkAccess(acc, "B", "owner"); err == nil {
		t.Fatal("роль владельца по ссылке прошла")
	}
	if err := g.addPerson(acc, "B", "a@b.kg, c@d.kg", "reader", false); err == nil {
		t.Fatal("два адреса сразу прошли")
	}
	if err := g.addPerson(acc, "B", "a@b.kg", "commenter", false); err != nil {
		t.Fatal(err)
	}
	pid := fo.files["B"].Perms[1].ID
	if err := g.changePerm(acc, "B", pid, "writer"); err != nil || fo.files["B"].Perms[1].Role != "writer" {
		t.Fatalf("change: %v", err)
	}
	if err := g.changePerm(acc, "B", pid, ""); err != nil || len(fo.files["B"].Perms) != 1 {
		t.Fatalf("remove: %v", err)
	}
}

func TestGOrderBy(t *testing.T) {
	for in, want := range map[string]string{"name": "folder,name_natural", "modified-": "folder,modifiedTime desc", "size-": "folder,quotaBytesUsed desc", "owner": "", "": ""} {
		if got := gOrderBy(in); got != want {
			t.Errorf("gOrderBy(%q) = %q, want %q", in, got, want)
		}
	}
}
