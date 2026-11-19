package main

// Этап 4: управление файлами на Google Диске — новая папка, переименование, перемещение,
// копия, корзина (и возврат), удаление навсегда только из корзины, пометка, доступ.
// Ссылка на файл в Google Диске держится на его ID, поэтому перемещение и переименование
// ссылки не меняют; ломает их только удаление.

import (
	"errors"
	"net/url"
	"strings"
	"sync"
)

type GOpReq struct {
	Acc  string   `json:"acc"`
	Op   string   `json:"op"`
	IDs  []string `json:"ids"`
	To   string   `json:"to"`   // папка назначения (move, copy, mkdir)
	Name string   `json:"name"` // rename, mkdir
	On   bool     `json:"on"`   // star
}

type GOpRes struct {
	OK     bool              `json:"ok"`
	Done   int               `json:"done"`
	Errors []string          `json:"errors,omitempty"`
	Error  string            `json:"error,omitempty"`
	ID     string            `json:"id,omitempty"`   // новая папка
	From   map[string]string `json:"from,omitempty"` // move: где лежал каждый файл (для «Отменить»)
}

var gOpQ = url.Values{"supportsAllDrives": {"true"}, "fields": {"id"}}

func gq(extra ...string) url.Values {
	q := url.Values{"supportsAllDrives": {"true"}, "fields": {"id"}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return q
}

func cleanDriveName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("пустое имя")
	}
	if len([]rune(s)) > 250 {
		return "", errors.New("слишком длинное имя")
	}
	return s, nil
}

// Над каждым ID — не больше 4 запросов одновременно (лимиты Google).
func (g *GDrive) each(ids []string, fn func(id string) error) (done int, errs []string) {
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, id := range ids {
		id := id
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			err := fn(id)
			mu.Lock()
			if err != nil {
				errs = append(errs, err.Error())
			} else {
				done++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return
}

func (g *GDrive) parentsOf(acc, id string) ([]string, string, error) {
	var f struct {
		Name    string   `json:"name"`
		Parents []string `json:"parents"`
		Trashed bool     `json:"trashed"`
	}
	err := g.call(acc, "GET", "/drive/v3/files/"+url.PathEscape(id), gq("fields", "name,parents,trashed"), nil, &f)
	return f.Parents, f.Name, err
}

func (g *GDrive) op(r GOpReq) GOpRes {
	if r.Acc == "" {
		return GOpRes{Error: "не выбран аккаунт Google"}
	}
	path := func(id string) string { return "/drive/v3/files/" + url.PathEscape(id) }
	var res GOpRes
	fail := func(err error) GOpRes { return GOpRes{Error: err.Error()} }

	switch r.Op {
	case "mkdir":
		name, err := cleanDriveName(r.Name)
		if err != nil {
			return fail(err)
		}
		if r.To == "" {
			r.To = "root"
		}
		var f struct {
			ID string `json:"id"`
		}
		if err := g.call(r.Acc, "POST", "/drive/v3/files", gOpQ,
			map[string]any{"name": name, "mimeType": gFolderMime, "parents": []string{r.To}}, &f); err != nil {
			return fail(err)
		}
		return GOpRes{OK: true, Done: 1, ID: f.ID}

	case "rename":
		name, err := cleanDriveName(r.Name)
		if err != nil {
			return fail(err)
		}
		if len(r.IDs) != 1 {
			return fail(errors.New("переименовать можно один файл"))
		}
		if err := g.call(r.Acc, "PATCH", path(r.IDs[0]), gOpQ, map[string]any{"name": name}, nil); err != nil {
			return fail(err)
		}
		return GOpRes{OK: true, Done: 1}

	case "trash", "untrash":
		on := r.Op == "trash"
		res.Done, res.Errors = g.each(r.IDs, func(id string) error {
			return g.call(r.Acc, "PATCH", path(id), gOpQ, map[string]any{"trashed": on}, nil)
		})

	case "delete":
		// Навсегда — только то, что уже лежит в корзине: случайно стереть живой файл нельзя.
		res.Done, res.Errors = g.each(r.IDs, func(id string) error {
			var f struct {
				Name    string `json:"name"`
				Trashed bool   `json:"trashed"`
			}
			if err := g.call(r.Acc, "GET", path(id), gq("fields", "name,trashed"), nil, &f); err != nil {
				return err
			}
			if !f.Trashed {
				return errors.New(f.Name + ": навсегда удаляется только из корзины")
			}
			return g.call(r.Acc, "DELETE", path(id), url.Values{"supportsAllDrives": {"true"}}, nil, nil)
		})

	case "empty-trash":
		if err := g.call(r.Acc, "DELETE", "/drive/v3/files/trash", nil, nil, nil); err != nil {
			return fail(err)
		}
		return GOpRes{OK: true}

	case "star":
		res.Done, res.Errors = g.each(r.IDs, func(id string) error {
			return g.call(r.Acc, "PATCH", path(id), gOpQ, map[string]any{"starred": r.On}, nil)
		})

	case "move":
		if r.To == "" {
			return fail(errors.New("не выбрана папка"))
		}
		res.From = map[string]string{}
		var mu sync.Mutex
		res.Done, res.Errors = g.each(r.IDs, func(id string) error {
			if id == r.To {
				return errors.New("папку нельзя положить в саму себя")
			}
			old, name, err := g.parentsOf(r.Acc, id)
			if err != nil {
				return err
			}
			for _, p := range old {
				if p == r.To {
					return nil // уже здесь
				}
			}
			q := gq("addParents", r.To, "removeParents", strings.Join(old, ","))
			if err := g.call(r.Acc, "PATCH", path(id), q, map[string]any{}, nil); err != nil {
				return errors.New(name + ": " + err.Error())
			}
			if len(old) > 0 {
				mu.Lock()
				res.From[id] = old[0]
				mu.Unlock()
			}
			return nil
		})

	case "copy":
		res.Done, res.Errors = g.each(r.IDs, func(id string) error {
			body := map[string]any{}
			if r.To != "" {
				body["parents"] = []string{r.To}
			} else if _, name, err := g.parentsOf(r.Acc, id); err == nil {
				body["name"] = "Копия " + name
			}
			return g.call(r.Acc, "POST", path(id)+"/copy", gOpQ, body, nil)
		})

	default:
		return fail(errors.New("неизвестная операция"))
	}
	res.OK = len(res.Errors) == 0 || res.Done > 0
	if !res.OK && len(res.Errors) > 0 {
		res.Error = res.Errors[0]
	}
	return res
}

// ---------- доступ ----------

type GPerm struct {
	ID      string `json:"id"`
	Type    string `json:"type"` // user | group | domain | anyone
	Role    string `json:"role"` // owner | organizer | fileOrganizer | writer | commenter | reader
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`
	Domain  string `json:"domain,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

func (g *GDrive) perms(acc, id string) ([]GPerm, string, error) {
	var out struct {
		Permissions []struct {
			ID           string `json:"id"`
			Type         string `json:"type"`
			Role         string `json:"role"`
			EmailAddress string `json:"emailAddress"`
			DisplayName  string `json:"displayName"`
			Domain       string `json:"domain"`
			Deleted      bool   `json:"deleted"`
		} `json:"permissions"`
	}
	err := g.call(acc, "GET", "/drive/v3/files/"+url.PathEscape(id)+"/permissions",
		url.Values{"supportsAllDrives": {"true"}, "fields": {"permissions(id,type,role,emailAddress,displayName,domain,deleted)"}}, nil, &out)
	if err != nil {
		return nil, "", err
	}
	ps := []GPerm{}
	for _, p := range out.Permissions {
		ps = append(ps, GPerm{ID: p.ID, Type: p.Type, Role: p.Role, Email: p.EmailAddress, Name: p.DisplayName, Domain: p.Domain, Deleted: p.Deleted})
	}
	var f struct {
		WebViewLink string `json:"webViewLink"`
	}
	_ = g.call(acc, "GET", "/drive/v3/files/"+url.PathEscape(id), gq("fields", "webViewLink"), nil, &f)
	return ps, f.WebViewLink, nil
}

var gRoles = map[string]bool{"reader": true, "commenter": true, "writer": true}

// Доступ по ссылке: role "" — закрыть, иначе reader/commenter/writer.
func (g *GDrive) setLinkAccess(acc, id, role string) error {
	if role != "" && !gRoles[role] {
		return errors.New("неизвестная роль")
	}
	ps, _, err := g.perms(acc, id)
	if err != nil {
		return err
	}
	base := "/drive/v3/files/" + url.PathEscape(id) + "/permissions"
	sq := url.Values{"supportsAllDrives": {"true"}}
	for _, p := range ps {
		if p.Type != "anyone" {
			continue
		}
		if role == "" {
			return g.call(acc, "DELETE", base+"/"+url.PathEscape(p.ID), sq, nil, nil)
		}
		return g.call(acc, "PATCH", base+"/"+url.PathEscape(p.ID), sq, map[string]any{"role": role}, nil)
	}
	if role == "" {
		return nil
	}
	return g.call(acc, "POST", base, sq, map[string]any{"type": "anyone", "role": role}, nil)
}

func (g *GDrive) addPerson(acc, id, email, role string, notify bool) error {
	email = strings.TrimSpace(email)
	if !strings.Contains(email, "@") || strings.ContainsAny(email, " ,;") {
		return errors.New("укажите один адрес почты")
	}
	if !gRoles[role] {
		return errors.New("неизвестная роль")
	}
	q := url.Values{"supportsAllDrives": {"true"}, "sendNotificationEmail": {"false"}}
	if notify {
		q.Set("sendNotificationEmail", "true")
	}
	return g.call(acc, "POST", "/drive/v3/files/"+url.PathEscape(id)+"/permissions", q,
		map[string]any{"type": "user", "role": role, "emailAddress": email}, nil)
}

func (g *GDrive) changePerm(acc, id, permID, role string) error {
	base := "/drive/v3/files/" + url.PathEscape(id) + "/permissions/" + url.PathEscape(permID)
	sq := url.Values{"supportsAllDrives": {"true"}}
	if role == "" {
		return g.call(acc, "DELETE", base, sq, nil, nil)
	}
	if !gRoles[role] {
		return errors.New("неизвестная роль")
	}
	return g.call(acc, "PATCH", base, sq, map[string]any{"role": role}, nil)
}
