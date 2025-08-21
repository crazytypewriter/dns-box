package blocklist

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	log "github.com/sirupsen/logrus"
)

type BlockList struct {
	urls           []string
	refreshTicker  *time.Ticker
	blockedDomains *cache.DomainCache
	httpClient     *http.Client
	logger         *log.Logger
	stopChan       chan struct{}
	forceUpdate    chan struct{}
	lastUpdated    time.Time
	totalDomains   int
}

func NewBlockList(cfg *config.BlockListConfig, logger *log.Logger) *BlockList {
	refreshHours := time.Duration(cfg.RefreshHours)
	if refreshHours <= 0 {
		logger.Warnf("Non-positive refresh interval specified (%d hours), defaulting to 24 hours", cfg.RefreshHours)
		refreshHours = 24
	}

	return &BlockList{
		urls:           cfg.URLs,
		logger:         logger,
		blockedDomains: cache.NewDomainCache(1000000), // Размер кеша можно вынести в конфиг
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		refreshTicker: time.NewTicker(refreshHours * time.Hour),
		stopChan:      make(chan struct{}),
		forceUpdate:   make(chan struct{}, 1), // Буферизованный канал
	}
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

func (b *BlockList) updateLists() {
	b.logger.Info("Updating blocklists...")
	newBlockedDomains := cache.NewDomainCache(1000000) // Временный кеш для обновления
	totalDomains := 0

	for _, url := range b.urls {
		b.logger.Infof("Processing blocklist from %s...", url)
		var body io.ReadCloser
		var err error

		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
			var resp *http.Response
			resp, err = b.httpClient.Get(url)
			if err != nil {
				b.logger.Errorf("Failed to download blocklist from %s: %v", url, err)
				continue
			}
			if resp.StatusCode != http.StatusOK {
				b.logger.Errorf("Failed to download blocklist from %s: status code %d", url, resp.StatusCode)
				continue
			}
			body = resp.Body
		} else {
			body, err = os.Open(url)
			if err != nil {
				b.logger.Errorf("Failed to open blocklist file %s: %v", url, err)
				continue
			}
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
				domain := parts[1]
				newBlockedDomains.Add(domain)
				domainsLoaded++
			}
		}

		if err := scanner.Err(); err != nil {
			b.logger.Errorf("Error reading blocklist from %s: %v", url, err)
			continue
		}

		b.logger.Infof("Loaded %d domains from %s", domainsLoaded, url)
		totalDomains += domainsLoaded
	}

	b.blockedDomains = newBlockedDomains
	b.totalDomains = totalDomains
	b.lastUpdated = time.Now()
	b.logger.Infof("Blocklists updated successfully. Total domains: %d", totalDomains)
}

func (b *BlockList) IsBlocked(domain string) bool {
	return b.blockedDomains.Contains(domain)
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
	b.urls = urls
}

func (b *BlockList) GetStatus() (time.Time, int, []string) {
	return b.lastUpdated, b.totalDomains, b.urls
}

func (b *BlockList) Stop() {
	// Логика остановки уже обрабатывается через контекст в Start
}
