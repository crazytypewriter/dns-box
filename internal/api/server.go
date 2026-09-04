package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/crazytypewriter/dns-box/internal/blocklist"
	"github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	"github.com/crazytypewriter/dns-box/internal/ipset"
	"github.com/crazytypewriter/dns-box/internal/ipsetstate"
	log "github.com/sirupsen/logrus"
)

type Server struct {
	cfg              *config.Config
	domainCache      *cache.DomainCache
	dnsCache         *cache.DNSCache
	httpServer       *http.Server
	log              *log.Logger
	blockList        *blocklist.BlockList
	listDomainCaches map[int]*cache.DomainCache
	ipSet            ipset.Manager
	stateStore       *ipsetstate.Store

	// mu защищает httpServer: Start и Stop вызываются из разных горутин.
	mu sync.Mutex
}

func NewServer(cfg *config.Config, dnsCache *cache.DNSCache, domainCache *cache.DomainCache, blockList *blocklist.BlockList, listDomainCaches map[int]*cache.DomainCache, ipSet ipset.Manager, stateStore *ipsetstate.Store, l *log.Logger) *Server {
	return &Server{
		cfg:              cfg,
		dnsCache:         dnsCache,
		domainCache:      domainCache,
		log:              l,
		blockList:        blockList,
		listDomainCaches: listDomainCaches,
		ipSet:            ipSet,
		stateStore:       stateStore,
	}
}

// Start не блокирует: слушатель уходит в отдельную горутину. Вызывать
// через `go` не нужно — иначе Stop может обогнать инициализацию.
func (s *Server) Start(ctx context.Context, addr string) {
	handlers := NewHandlers(s.cfg, s.dnsCache, s.domainCache, s.blockList, s.listDomainCaches, s.ipSet, s.stateStore)

	srv := &http.Server{
		Addr:    addr,
		Handler: handlers.Routes(),
	}
	s.mu.Lock()
	s.httpServer = srv
	s.mu.Unlock()

	go func() {
		// Занятый порт — не повод ронять DNS: логируем и живём дальше.
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Errorf("API server on %s stopped: %v", addr, err)
		}
	}()
}

func (s *Server) Stop(ctx context.Context) {
	s.mu.Lock()
	srv := s.httpServer
	s.mu.Unlock()
	if srv == nil {
		return // Start не успел отработать
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		s.log.Errorf("API server shutdown error: %v", err)
	}
}
