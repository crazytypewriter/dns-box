package blocklist

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	log "github.com/sirupsen/logrus"
)

// errTruncated — поток источника оборвался на середине: часть его доменов
// уже попала в собираемый набор, поэтому набор больше не целый.
var errTruncated = errors.New("blocklist stream truncated")

type BlockList struct {
	// blockedDomains меняется целиком (swap указателя) из горутины
	// обновления и читается из всех DNS-горутин.
	blockedDomains atomic.Pointer[cache.DomainCache]

	// mu защищает изменяемое состояние: urls правит API, статус читает
	// GetStatus.
	mu           sync.RWMutex
	urls         []string
	lastUpdated  time.Time
	totalDomains int

	refreshTicker *time.Ticker
	httpClient    *http.Client
	logger        *log.Logger
	forceUpdate   chan struct{}
}

func NewBlockList(cfg *config.BlockListConfig, logger *log.Logger) *BlockList {
	refreshHours := time.Duration(cfg.RefreshHours)
	if refreshHours <= 0 {
		logger.Warnf("Non-positive refresh interval specified (%d hours), defaulting to 24 hours", cfg.RefreshHours)
		refreshHours = 24
	}

	b := &BlockList{
		urls:   append([]string(nil), cfg.URLs...),
		logger: logger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		refreshTicker: time.NewTicker(refreshHours * time.Hour),
		forceUpdate:   make(chan struct{}, 1), // Буферизованный канал
	}
	b.blockedDomains.Store(cache.NewDomainCache(1000000)) // Размер кеша можно вынести в конфиг
	return b
}

func (b *BlockList) Start(ctx context.Context) {
	b.logger.Info("Starting blocklist service...")
	go func() {
		b.updateLists()

		for {
			select {
			case <-b.refreshTicker.C:
				b.updateLists()
			case <-b.forceUpdate:
				b.updateLists()
			case <-ctx.Done():
				b.logger.Info("Stopping blocklist service...")
				b.refreshTicker.Stop()
				return
			}
		}
	}()
}

// loadSource читает один источник (URL или локальный файл) в dst и
// возвращает число загруженных доменов. Обрыв потока отдаётся как
// errTruncated: вызывающий обязан считать весь набор испорченным.
func (b *BlockList) loadSource(url string, dst *cache.DomainCache) (int, error) {
	var body io.ReadCloser

	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		resp, err := b.httpClient.Get(url)
		if err != nil {
			return 0, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return 0, fmt.Errorf("status code %d", resp.StatusCode)
		}
		body = resp.Body
	} else {
		f, err := os.Open(url)
		if err != nil {
			return 0, err
		}
		body = f
	}
	defer body.Close()

	// Парсинг доменов
	scanner := bufio.NewScanner(body)
	domainsLoaded := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			dst.Add(parts[1])
			domainsLoaded++
		}
	}
	if err := scanner.Err(); err != nil {
		return domainsLoaded, fmt.Errorf("%w: %v", errTruncated, err)
	}
	return domainsLoaded, nil
}

// updateLists пересобирает список блокировки.
//
// Главное правило: неудачный прогон не публикуется. Пустой или частичный
// набор молча выключил бы блокировку до следующего обновления (сутки по
// умолчанию), а протухший список стоит несравнимо дешевле.
func (b *BlockList) updateLists() {
	urls := b.URLs()
	b.logger.Info("Updating blocklists...")

	newBlockedDomains := cache.NewDomainCache(1000000) // Временный кеш для обновления
	totalDomains := 0
	loaded := 0
	corrupted := false

	for _, url := range urls {
		b.logger.Infof("Processing blocklist from %s...", url)
		n, err := b.loadSource(url, newBlockedDomains)
		if err != nil {
			b.logger.Errorf("Failed to load blocklist from %s: %v", url, err)
			if errors.Is(err, errTruncated) {
				// Часть доменов источника уже в наборе — дальше собирать
				// бессмысленно, весь прогон бракуем.
				corrupted = true
				break
			}
			continue
		}
		b.logger.Infof("Loaded %d domains from %s", n, url)
		totalDomains += n
		loaded++
	}

	if corrupted || (len(urls) > 0 && loaded == 0) {
		if _, prevTotal, _ := b.GetStatus(); prevTotal > 0 {
			b.logger.Errorf("Blocklist refresh failed (loaded %d of %d sources), keeping the previous list of %d domains",
				loaded, len(urls), prevTotal)
			return
		}
		b.logger.Warnf("Blocklist refresh failed (loaded %d of %d sources) and there is no previous list; blocking stays off until the next refresh",
			loaded, len(urls))
	}

	b.blockedDomains.Store(newBlockedDomains)

	b.mu.Lock()
	b.totalDomains = totalDomains
	b.lastUpdated = time.Now()
	b.mu.Unlock()

	if loaded < len(urls) {
		b.logger.Warnf("Blocklists updated from %d of %d sources. Total domains: %d", loaded, len(urls), totalDomains)
		return
	}
	b.logger.Infof("Blocklists updated successfully. Total domains: %d", totalDomains)
}

func (b *BlockList) IsBlocked(domain string) bool {
	blocked := b.blockedDomains.Load()
	if blocked == nil {
		return false
	}
	return blocked.Contains(domain)
}

// ForceRefresh инициирует немедленное обновление списков блокировки.
func (b *BlockList) ForceRefresh() {
	select {
	case b.forceUpdate <- struct{}{}:
	default:
	}
}

// UpdateURLs обновляет список URL-адресов для списков блокировки.
func (b *BlockList) UpdateURLs(urls []string) {
	b.mu.Lock()
	b.urls = append([]string(nil), urls...)
	b.mu.Unlock()
}

// URLs возвращает копию текущего списка источников.
func (b *BlockList) URLs() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]string(nil), b.urls...)
}

func (b *BlockList) GetStatus() (time.Time, int, []string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastUpdated, b.totalDomains, append([]string(nil), b.urls...)
}

func (b *BlockList) Stop() {
	// Логика остановки уже обрабатывается через контекст в Start
}
