package blocklist

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func testLogger() *log.Logger {
	l := log.New()
	l.SetOutput(io.Discard)
	l.SetLevel(log.PanicLevel)
	return l
}

func newList(t *testing.T, urls ...string) *BlockList {
	t.Helper()
	return NewBlockList(&config.BlockListConfig{Enabled: true, URLs: urls, RefreshHours: 24}, testLogger())
}

// writeList кладёт список в hosts-формате во временный файл.
func writeList(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "list.txt")
	require.NoError(t, os.WriteFile(p, []byte(body), 0600))
	return p
}

func newDomainCacheForTest() *cache.DomainCache { return cache.NewDomainCache(1024 * 1024) }

func TestUpdateListsLoadsHostsFormat(t *testing.T) {
	p := writeList(t, "# comment\n\n0.0.0.0 ads.example.com\n127.0.0.1 tracker.example.net\nбез-полей\n")
	b := newList(t, p)
	b.updateLists()

	require.True(t, b.IsBlocked("ads.example.com"))
	require.True(t, b.IsBlocked("tracker.example.net"))
	require.False(t, b.IsBlocked("example.org"))

	_, total, _ := b.GetStatus()
	require.Equal(t, 2, total)
}

// Все источники недоступны — прежний список обязан остаться на месте.
func TestUpdateListsKeepsPreviousWhenAllSourcesFail(t *testing.T) {
	good := writeList(t, "0.0.0.0 ads.example.com\n")
	b := newList(t, good)
	b.updateLists()
	require.True(t, b.IsBlocked("ads.example.com"))
	_, total, _ := b.GetStatus()
	require.Equal(t, 1, total)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	b.UpdateURLs([]string{srv.URL, filepath.Join(t.TempDir(), "нет-такого-файла.txt")})
	b.updateLists()

	require.True(t, b.IsBlocked("ads.example.com"), "старый список должен пережить неудачное обновление")
	_, total, _ = b.GetStatus()
	require.Equal(t, 1, total, "статистика не должна обнуляться")
}

// Первый запуск без прежнего списка: сохранять нечего, публикуем пустой.
func TestUpdateListsFirstRunFailurePublishesEmpty(t *testing.T) {
	b := newList(t, filepath.Join(t.TempDir(), "нет-такого-файла.txt"))
	b.updateLists()

	require.False(t, b.IsBlocked("ads.example.com"))
	_, total, _ := b.GetStatus()
	require.Equal(t, 0, total)
}

// Часть источников жива — публикуем то, что прочиталось: иначе один
// навсегда протухший URL заморозил бы обновления насмерть.
func TestUpdateListsPublishesPartialWhenSomeSourcesLoad(t *testing.T) {
	good := writeList(t, "0.0.0.0 ads.example.com\n")
	b := newList(t, good, filepath.Join(t.TempDir(), "нет-такого-файла.txt"))
	b.updateLists()

	require.True(t, b.IsBlocked("ads.example.com"))
	_, total, _ := b.GetStatus()
	require.Equal(t, 1, total)
}

// Обрыв потока на середине: часть доменов уже в наборе, публиковать
// такой набор нельзя — он молча урезал бы блокировку.
func TestUpdateListsKeepsPreviousOnTruncatedSource(t *testing.T) {
	good := writeList(t, "0.0.0.0 ads.example.com\n0.0.0.0 tracker.example.net\n")
	b := newList(t, good)
	b.updateLists()
	require.True(t, b.IsBlocked("tracker.example.net"))

	// Строка длиннее лимита bufio.Scanner — Err() вернёт ErrTooLong уже
	// после того, как первая запись попала в набор.
	broken := writeList(t, "0.0.0.0 ads.example.com\n0.0.0.0 "+strings.Repeat("x", 128*1024)+"\n")
	b.UpdateURLs([]string{broken})
	b.updateLists()

	require.True(t, b.IsBlocked("tracker.example.net"), "частичный набор не должен подменять прежний список")
	_, total, _ := b.GetStatus()
	require.Equal(t, 2, total)
}

func TestLoadSourceReportsTruncation(t *testing.T) {
	p := writeList(t, "0.0.0.0 ads.example.com\n0.0.0.0 "+strings.Repeat("x", 128*1024)+"\n")
	b := newList(t)
	n, err := b.loadSource(p, newDomainCacheForTest())
	require.ErrorIs(t, err, errTruncated)
	require.Equal(t, 1, n, "домены до обрыва уже разобраны — потому набор и бракуется")
}

func TestUpdateListsHTTPSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	b := newList(t, srv.URL)
	b.updateLists()
	require.True(t, b.IsBlocked("ads.example.com"))
}

// Обновление списка идёт параллельно чтениям из DNS-горутин и правкам
// URL из API: проверяем под -race.
func TestConcurrentAccess(t *testing.T) {
	p := writeList(t, "0.0.0.0 ads.example.com\n")
	b := newList(t, p)
	b.updateLists()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.IsBlocked("ads.example.com")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			b.updateLists()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			b.UpdateURLs([]string{p})
			b.GetStatus()
		}
	}()
	wg.Wait()

	require.True(t, b.IsBlocked("ads.example.com"))
}
