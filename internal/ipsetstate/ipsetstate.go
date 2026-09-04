// Package ipsetstate хранит в памяти зеркало добавленных в ipset записей
// и периодически сбрасывает его на диск, чтобы пережить ребут роутера,
// когда сеты в ядре пропадают вместе с ними.
//
// Зеркало — «нижняя граница» реального содержимого сета: продления
// таймеров правилом -j SET --exist происходят в ядре и приложению не
// видны. Поэтому при восстановлении записи, уже лежащие в сете,
// пропускаются (ipset.Manager.ListElements) — таймер ядра точнее нашего.
package ipsetstate

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

type Entry struct {
	Set    string `json:"set"`
	IP     string `json:"ip"`
	Expire int64  `json:"expire"` // unix; 0 = вечная запись
}

// RemainingTTL возвращает остаток TTL записи относительно now, 0 для вечной
// записи и для истёкших.
func RemainingTTL(e Entry, now int64) uint32 {
	if e.Expire == 0 {
		return 0
	}
	if r := e.Expire - now; r > 0 {
		return uint32(r)
	}
	return 0
}

type Store struct {
	mu      sync.RWMutex
	entries map[string]map[string]int64 // set -> ip -> expire
	dirty   atomic.Bool
	limit   int // max_entries_per_set
	log     *log.Logger
}

func NewStore(limit int, l *log.Logger) *Store {
	if limit <= 0 {
		limit = 20000
	}
	if l == nil {
		l = log.New()
	}
	return &Store{
		entries: map[string]map[string]int64{},
		limit:   limit,
		log:     l,
	}
}

// Record фиксирует добавление записи. ttl == 0 — вечная запись.
func (s *Store) Record(set, ip string, ttl uint32) {
	var expire int64
	if ttl > 0 {
		expire = time.Now().Add(time.Duration(ttl) * time.Second).Unix()
	}

	s.mu.Lock()
	setEntries, ok := s.entries[set]
	if !ok {
		setEntries = map[string]int64{}
		s.entries[set] = setEntries
	}
	if old, exists := setEntries[ip]; !exists || old != expire {
		setEntries[ip] = expire
		s.dirty.Store(true)
	}

	// Вытеснение при переполнении — батчами по ~10% сета (записи с ближайшим
	// expire, вечные не трогаем). Отбор — один проход по мапе со сбором
	// невечных записей в срез и сортировкой: O(n log n), без повторных
	// проходов под write-локом.
	if len(setEntries) > s.limit {
		batch := s.limit / 10
		if batch < 1 {
			batch = 1
		}
		type expEntry struct {
			ip     string
			expire int64
		}
		candidates := make([]expEntry, 0, len(setEntries))
		for ip2, exp := range setEntries {
			if exp == 0 {
				continue // вечная
			}
			candidates = append(candidates, expEntry{ip: ip2, expire: exp})
		}
		if len(candidates) > 0 {
			if batch > len(candidates) {
				batch = len(candidates)
			}
			slices.SortFunc(candidates, func(a, b expEntry) int {
				return cmp.Compare(a.expire, b.expire)
			})
			for _, v := range candidates[:batch] {
				delete(setEntries, v.ip)
			}
			s.dirty.Store(true)
			s.log.Warnf("ipset state: set %s exceeded %d entries, evicted %d (nearest to expire) — consider raising maxelem/max_entries_per_set", set, s.limit, batch)
		} else {
			s.log.Warnf("ipset state: set %s exceeds limit %d with only eternal entries, keeping all", set, s.limit)
		}
	}
	s.mu.Unlock()
}

// Snapshot возвращает копию всех записей, вычищая просроченные.
func (s *Store) Snapshot() []Entry {
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Entry, 0)
	for set, setEntries := range s.entries {
		for ip, exp := range setEntries {
			if exp != 0 && exp <= now {
				delete(setEntries, ip)
				s.dirty.Store(true)
				continue
			}
			out = append(out, Entry{Set: set, IP: ip, Expire: exp})
		}
	}
	return out
}

// FlushIfDirty атомарно (tmp → Sync → rename) пишет состояние на диск,
// если были изменения с прошлого флаша.
func (s *Store) FlushIfDirty(path string) error {
	if path == "" || !s.dirty.Load() {
		return nil
	}
	return s.Flush(path)
}

// Flush пишет состояние на диск независимо от флага dirty.
func (s *Store) Flush(path string) error {
	entries := s.Snapshot()

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("failed to marshal ipset state: %w", err)
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}

	s.dirty.Store(false)
	s.log.Debugf("ipset state flushed to %s: %d entries", path, len(entries))
	return nil
}

// Load читает состояние с диска. Битый/отсутствующий файл — ошибка,
// вызывающий обязан относиться к ней как к «пустой старт», не фатал.
func Load(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("failed to parse ipset state %s: %w", path, err)
	}
	return entries, nil
}

// Restore заливает записи в Store (после фактического AddElement в ipset).
func (s *Store) Restore(entries []Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		setEntries, ok := s.entries[e.Set]
		if !ok {
			setEntries = map[string]int64{}
			s.entries[e.Set] = setEntries
		}
		setEntries[e.IP] = e.Expire
	}
	s.dirty.Store(false) // на диске уже то же самое
}
