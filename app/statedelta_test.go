package main

import (
	"encoding/json"
	"testing"
)

func TestJobsDelta(t *testing.T) {
	d := newJobsDelta()
	ids := func(raw json.RawMessage) []string {
		var js []Job
		if err := json.Unmarshal(raw, &js); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, j := range js {
			out = append(out, j.ID)
		}
		return out
	}
	a, b, c := &Job{ID: "a", Status: "done"}, &Job{ID: "b", Status: "active"}, &Job{ID: "c"}
	jobs := []*Job{a, b, c}

	raw, rm, rev, full := d.build(jobs, "", 0)
	if !full || len(ids(raw)) != 3 || len(rm) != 0 {
		t.Fatalf("первый запрос — весь список: full=%v ids=%v", full, ids(raw))
	}
	// ничего не изменилось — пусто, ревизия та же
	raw, _, rev2, full := d.build(jobs, d.epoch, rev)
	if full || len(ids(raw)) != 0 || rev2 != rev {
		t.Fatalf("без изменений: full=%v ids=%v rev %d→%d", full, ids(raw), rev, rev2)
	}
	// прогресс одной закачки и удаление другой
	b.Done = 100
	jobs = []*Job{a, b}
	raw, rm, rev3, full := d.build(jobs, d.epoch, rev2)
	if full || len(ids(raw)) != 1 || ids(raw)[0] != "b" || len(rm) != 1 || rm[0] != "c" || rev3 <= rev2 {
		t.Fatalf("дельта: full=%v ids=%v removed=%v", full, ids(raw), rm)
	}
	// чужая эпоха (программа перезапущена) — снова весь список
	if raw, _, _, full := d.build(jobs, "old", rev3); !full || len(ids(raw)) != 2 {
		t.Fatalf("другая эпоха: full=%v", full)
	}
	// клиент «из будущего» (ревизия больше нашей) — весь список
	if _, _, _, full := d.build(jobs, d.epoch, rev3+10); !full {
		t.Fatal("since > rev должен давать полный список")
	}
}
