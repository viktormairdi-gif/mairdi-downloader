package main

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rule — правило раскладки, которому пользователь научил программу.
type Rule struct {
	Kind    string    `json:"kind"` // domain | ext | keyword
	Value   string    `json:"value"`
	Dir     string    `json:"dir"`
	Created time.Time `json:"created"`
}

func (r *Rule) describe() string {
	switch r.Kind {
	case "domain":
		return "файлы с сайта " + r.Value + " → " + r.Dir
	case "ext":
		return "все *" + r.Value + " → " + r.Dir
	}
	return "«" + r.Value + "» в названии → " + r.Dir
}

type Rules struct {
	mu    sync.Mutex
	path  string
	Items []Rule
}

func loadRules(path string) *Rules {
	r := &Rules{path: path}
	loadJSON(path, &r.Items)
	return r
}

func (r *Rules) save() { _ = saveJSON(r.path, r.Items) }

func (r *Rules) match(domain, name, ext string) *Rule {
	r.mu.Lock()
	defer r.mu.Unlock()
	lname := strings.ToLower(name)
	for _, kind := range []string{"domain", "keyword", "ext"} {
		for i := len(r.Items) - 1; i >= 0; i-- {
			it := r.Items[i]
			if it.Kind != kind {
				continue
			}
			switch kind {
			case "domain":
				if domain != "" && (domain == it.Value || strings.HasSuffix(domain, "."+it.Value)) {
					return &it
				}
			case "keyword":
				if strings.Contains(lname, it.Value) {
					return &it
				}
			case "ext":
				if ext == it.Value {
					return &it
				}
			}
		}
	}
	return nil
}

func (r *Rules) add(kind, value, dir string) error {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return errors.New("пустое значение правила")
	}
	if kind == "ext" && !strings.HasPrefix(value, ".") {
		value = "." + value
	}
	if kind == "domain" {
		value = strings.TrimPrefix(value, "www.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.Items[:0]
	for _, it := range r.Items {
		if !(it.Kind == kind && it.Value == value) {
			out = append(out, it)
		}
	}
	r.Items = append(out, Rule{Kind: kind, Value: value, Dir: cleanRel(dir), Created: time.Now()})
	r.save()
	return nil
}

func (r *Rules) remove(i int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.Items) {
		return errors.New("нет правила №" + strconv.Itoa(i))
	}
	r.Items = append(r.Items[:i], r.Items[i+1:]...)
	r.save()
	return nil
}

func (r *Rules) list() []Rule {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Rule(nil), r.Items...)
}

// Относительный путь внутри корня без выхода наружу.
func cleanRel(dir string) string {
	dir = strings.ReplaceAll(dir, "/", `\`)
	var parts []string
	for _, p := range strings.Split(dir, `\`) {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." {
			continue
		}
		parts = append(parts, safeName(p))
	}
	if len(parts) == 0 {
		return "Прочее"
	}
	return filepath.Join(parts...)
}

func topCategory(rel string) string {
	if i := strings.IndexAny(rel, `\/`); i >= 0 {
		return rel[:i]
	}
	return rel
}
