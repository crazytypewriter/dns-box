package api

import (
	"context"
	"dns-box/internal/cache"
	"dns-box/internal/config"
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"net/http"
	"os"
	"time"
)

type Server struct {
	cfg         *config.Config
	domainCache *cache.DomainCache
	dnsCache    *cache.DNSCache
	httpServer  *http.Server
	log         *log.Logger
}

func NewServer(cfg *config.Config, dnsCache *cache.DNSCache, domainCache *cache.DomainCache, l *log.Logger) *Server {
	return &Server{
		cfg:         cfg,
		dnsCache:    dnsCache,
		domainCache: domainCache,
		log:         l,
	}
}

func (s *Server) Start(ctx context.Context, addr string) {
	handlers := NewHandlers(s.cfg, s.dnsCache, s.domainCache)

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

	s.log.Println("Shutting down api server, saving config...")
	if err := s.saveConfig(); err != nil {
		s.log.Printf("Error saving config: %s", err)
	} else {
		s.log.Println("Config saved.")
	}

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		panic(err)
	}
}

func (s *Server) saveConfig() error {
	file, err := os.Create(s.cfg.Path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(&s.cfg)
}
