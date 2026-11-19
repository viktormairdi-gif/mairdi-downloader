package main

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"strconv"
	"sync"
	"time"
)

// jobsDelta — окно опрашивает /api/state раз в секунду. Раньше каждый раз уходил весь список
// (18 тыс. закачек ≈ 16 МБ), теперь — только изменившиеся записи и id удалённых.
// Клиент присылает {epoch, since}; если epoch не совпал (программу перезапустили) или since
// старше выброшенного журнала удалений — отдаём список целиком (full=true).
type jobsDelta struct {
	mu      sync.Mutex
	epoch   string
	rev     int64
	floor   int64 // since < floor → только полный список
	hash    map[string]uint64
	at      map[string]int64 // id → rev последнего изменения
	removed map[string]int64 // id → rev удаления
}

const maxRemovedLog = 5000

func newJobsDelta() *jobsDelta {
	return &jobsDelta{
		epoch:   strconv.FormatInt(time.Now().UnixNano(), 36),
		hash:    map[string]uint64{},
		at:      map[string]int64{},
		removed: map[string]int64{},
	}
}

// build вызывается под a.mu (записи меняются под ним). Возвращает JSON-массив записей для клиента,
// id удалённых, номер ревизии и признак полного списка.
func (d *jobsDelta) build(jobs []*Job, epoch string, since int64) (raw json.RawMessage, removed []string, rev int64, full bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	enc := make([][]byte, len(jobs))
	seen := make(map[string]bool, len(jobs))
	changed := false
	for i, j := range jobs {
		b, _ := json.Marshal(j)
		enc[i] = b
		seen[j.ID] = true
		h := fnv.New64a()
		h.Write(b)
		sum := h.Sum64()
		if old, ok := d.hash[j.ID]; !ok || old != sum {
			if !changed {
				changed = true
				d.rev++
			}
			d.hash[j.ID] = sum
			d.at[j.ID] = d.rev
			delete(d.removed, j.ID)
		}
	}
	for id := range d.hash {
		if !seen[id] {
			if !changed {
				changed = true
				d.rev++
			}
			delete(d.hash, id)
			delete(d.at, id)
			d.removed[id] = d.rev
		}
	}
	if len(d.removed) > maxRemovedLog {
		d.removed = map[string]int64{}
		d.floor = d.rev
	}

	full = epoch != d.epoch || since <= 0 || since < d.floor || since > d.rev
	var buf bytes.Buffer
	buf.WriteByte('[')
	n := 0
	for i, j := range jobs {
		if !full && d.at[j.ID] <= since {
			continue
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		buf.Write(enc[i])
		n++
	}
	buf.WriteByte(']')
	if !full {
		for id, r := range d.removed {
			if r > since {
				removed = append(removed, id)
			}
		}
	}
	return buf.Bytes(), removed, d.rev, full
}
