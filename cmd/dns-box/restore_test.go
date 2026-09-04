package main

import (
	"errors"
	"testing"
	"time"

	"github.com/crazytypewriter/dns-box/internal/ipsetstate"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// fakeIPSet — минимальный ipset.Manager: помнит добавленные записи и
// отдаёт заранее заданное «текущее содержимое» сетов.
type fakeIPSet struct {
	added    map[string]map[string]uint32 // set -> ip -> ttl
	existing map[string][]string
	listErr  error
}

func newFakeIPSet() *fakeIPSet {
	return &fakeIPSet{added: map[string]map[string]uint32{}, existing: map[string][]string{}}
}

func (f *fakeIPSet) CreateIPv4Set(name string, timeout, maxElem uint32) error    { return nil }
func (f *fakeIPSet) CreateIPv6Set(name string, timeout, maxElem uint32) error    { return nil }
func (f *fakeIPSet) CreateIPv4NetSet(name string, timeout, maxElem uint32) error { return nil }
func (f *fakeIPSet) CreateIPv6NetSet(name string, timeout, maxElem uint32) error { return nil }
func (f *fakeIPSet) RemoveElement(set, ip string) error                          { return nil }

func (f *fakeIPSet) AddElement(set, ip string, ttl uint32) error {
	if f.added[set] == nil {
		f.added[set] = map[string]uint32{}
	}
	f.added[set][ip] = ttl
	return nil
}

func (f *fakeIPSet) ListElements(set string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.existing[set], nil
}

func testLogger() *log.Logger {
	l := log.New()
	l.SetLevel(log.PanicLevel)
	return l
}

func TestRestoreEntriesSkipsPresentAndExpired(t *testing.T) {
	now := time.Now().Unix()
	entries := []ipsetstate.Entry{
		{Set: "vpn", IP: "1.1.1.1", Expire: now + 600},  // новая — добавить
		{Set: "vpn", IP: "2.2.2.2", Expire: now + 600},  // уже в сете — пропустить
		{Set: "vpn", IP: "3.3.3.3", Expire: now - 10},   // истекла — не добавлять
		{Set: "vpn", IP: "4.4.4.4", Expire: 0},          // вечная — добавить с ttl 0
		{Set: "gone", IP: "5.5.5.5", Expire: now + 600}, // сета нет в конфиге
	}
	valid := map[string]bool{"vpn": true}

	f := newFakeIPSet()
	f.existing["vpn"] = []string{"2.2.2.2"}

	restored, skipped := restoreEntries(entries, valid, f, testLogger())

	require.Equal(t, 1, skipped)
	require.Equal(t, 2, restored["vpn"])
	require.Len(t, f.added["vpn"], 2)
	require.Contains(t, f.added["vpn"], "1.1.1.1")
	require.Equal(t, uint32(0), f.added["vpn"]["4.4.4.4"], "вечная запись восстанавливается с ttl 0")
	require.NotContains(t, f.added["vpn"], "2.2.2.2", "запись из ядра не перезаписывается")
	require.NotContains(t, f.added["vpn"], "3.3.3.3")
	require.NotContains(t, f.added, "gone")
}

// Если List недоступен (старое ядро, нет прав), восстанавливаем всё —
// поведение должно деградировать к прежнему, а не отваливаться.
func TestRestoreEntriesListErrorRestoresAll(t *testing.T) {
	now := time.Now().Unix()
	entries := []ipsetstate.Entry{
		{Set: "vpn", IP: "1.1.1.1", Expire: now + 600},
		{Set: "vpn", IP: "2.2.2.2", Expire: now + 600},
	}

	f := newFakeIPSet()
	f.existing["vpn"] = []string{"2.2.2.2"}
	f.listErr = errors.New("permission denied")

	restored, skipped := restoreEntries(entries, map[string]bool{"vpn": true}, f, testLogger())

	require.Equal(t, 0, skipped)
	require.Equal(t, 2, restored["vpn"])
}
