package ipsetstate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestRemainingTTL(t *testing.T) {
	now := int64(1000)

	require.Equal(t, uint32(0), RemainingTTL(Entry{Expire: 0}, now), "вечная запись → 0 (без срока)")
	require.Equal(t, uint32(100), RemainingTTL(Entry{Expire: 1100}, now))
	require.Equal(t, uint32(0), RemainingTTL(Entry{Expire: 1000}, now), "истекающая ровно сейчас → 0")
	require.Equal(t, uint32(0), RemainingTTL(Entry{Expire: 900}, now), "истёкшая → 0")
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ipset-state.json")
	s := NewStore(100, log.New())

	// Обычная запись с TTL и вечная
	s.Record("vpn", "1.2.3.4", 3600)
	s.Record("vpn", "10.0.0.1", 0)

	require.NoError(t, s.Flush(path))

	entries, err := Load(path)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	// Вечная запись сохраняется как Expire=0
	var eternal, timed Entry
	for _, e := range entries {
		switch e.IP {
		case "1.2.3.4":
			timed = e
		case "10.0.0.1":
			eternal = e
		}
	}
	require.Equal(t, int64(0), eternal.Expire)
	require.NotZero(t, timed.Expire)
	require.InDelta(t, time.Now().Add(time.Hour).Unix(), timed.Expire, 5)

	// Round-trip: Store → файл → Store
	s2 := NewStore(100, log.New())
	s2.Restore(entries)
	require.ElementsMatch(t, entries, s2.Snapshot())
}

func TestStoreEvictionKeepsEternal(t *testing.T) {
	s := NewStore(3, log.New())

	// Заполняем лимит вечными — вытеснять некого
	for i := 0; i < 5; i++ {
		s.Record("vpn", "10.0.0."+string(rune('1'+i)), 0)
	}
	require.Len(t, s.Snapshot(), 5, "вечные записи не вытесняются")

	// Теперь лимит обычных записей: уходят ближайшие к истечению
	s2 := NewStore(2, log.New())
	s2.Record("a", "1.1.1.1", 100)  // истечёт раньше
	s2.Record("a", "2.2.2.2", 1000) // позже
	s2.Record("a", "3.3.3.3", 5000) // сверх лимита → вытеснит 1.1.1.1
	entries := s2.Snapshot()
	require.Len(t, entries, 2)
	for _, e := range entries {
		require.NotEqual(t, "1.1.1.1", e.IP, "вытеснена запись с ближайшим expire")
	}
}

func TestFlushIfDirty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(10, log.New())

	// Без изменений флаш не пишет
	require.NoError(t, s.FlushIfDirty(path))
	_, err := Load(path)
	require.Error(t, err, "файл не должен был создаться без dirty")

	s.Record("vpn", "1.2.3.4", 60)
	require.NoError(t, s.FlushIfDirty(path))

	// После флаша dirty сброшен — повторный вызов ничего не делает
	require.NoError(t, s.FlushIfDirty(path))
}

func TestLoadBrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0600))
	_, err := Load(path)
	require.Error(t, err, "битый файл должен давать ошибку (вызывающий стартует с пустым зеркалом)")
}

func TestStoreBatchEviction(t *testing.T) {
	// Лимит 20 → батч 10% = 2 записи за раз
	s := NewStore(20, log.New())
	for i := 0; i < 22; i++ {
		// Чем больше i, тем позже истекает
		s.Record("vpn", ipOf(i), uint32(100+i))
	}
	entries := s.Snapshot()
	require.Len(t, entries, 20, "при превышении лимита вытесняется батч, а не одна запись")

	ips := map[string]bool{}
	for _, e := range entries {
		ips[e.IP] = true
	}
	// Вытеснены записи с ближайшим истечением (i = 0 и 1)
	require.False(t, ips[ipOf(0)])
	require.False(t, ips[ipOf(1)])
	require.True(t, ips[ipOf(21)])
}

func ipOf(i int) string {
	return fmt.Sprintf("10.0.%d.%d", i/256, i%256+1)
}

func TestStoreEvictionEqualExpires(t *testing.T) {
	// Все записи с одинаковым expire: батч должен добираться полностью,
	// без потери кандидатов на тай-брейке
	s := NewStore(10, log.New())
	for i := 0; i < 12; i++ {
		s.Record("vpn", ipOf(i), 500)
	}
	require.Len(t, s.Snapshot(), 10, "вытеснен батч 10% = 1 запись")
}

func BenchmarkStoreRecordEviction(b *testing.B) {
	l := log.New()
	l.SetLevel(log.PanicLevel) // не спамим warn в бенчмарке
	s := NewStore(20000, l)
	// Заполняем до лимита
	for i := 0; i < 20000; i++ {
		s.Record("vpn", ipOf(i), uint32(100+i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Record("vpn", ipOf(20000+i%1000), uint32(300+i))
	}
}
