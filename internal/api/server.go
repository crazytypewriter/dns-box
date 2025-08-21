package api

import (
	"context"
	"github.com/crazytypewriter/dns-box/internal/blocklist"
	"github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	log "github.com/sirupsen/logrus"
	"net/http"
	"time"
)

type Server struct {
	cfg         *config.Config
	domainCache *cache.DomainCache
	dnsCache    *cache.DNSCache
	httpServer  *http.Server
	log         *log.Logger
	blockList   *blocklist.BlockList
}

func NewServer(cfg *config.Config, dnsCache *cache.DNSCache, domainCache *cache.DomainCache, blockList *blocklist.BlockList, l *log.Logger) *Server {
	return &Server{
		cfg:         cfg,
		dnsCache:    dnsCache,
		domainCache: domainCache,
		log:         l,
		blockList:   blockList,
	}
}

func (s *Server) Start(ctx context.Context, addr string) {
	handlers := NewHandlers(s.cfg, s.dnsCache, s.domainCache, s.blockList)

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: handlers.Routes(),
	}

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()
}

func (s *Server) Stop(ctx context.Context) {
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		panic(err)
	}
}
