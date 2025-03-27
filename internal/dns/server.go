package dns

import (
	"context"
	"dns-box/internal/config"
	"fmt"
	"github.com/miekg/dns"
	"sync"
)

type Server struct {
	cfg     *config.Config
	handler *Handler
	servers []*dns.Server
	wg      sync.WaitGroup
}

func NewServer(cfg *config.Config, handler *Handler) *Server {
	return &Server{
		cfg:     cfg,
		handler: handler,
	}
}

func (s *Server) Start(ctx context.Context) {
	for _, addr := range s.cfg.Server.Address {
		s.wg.Add(1)
		go s.startServer(ctx, addr)
	}
}

func (s *Server) startServer(ctx context.Context, addr string) {
	defer s.wg.Done()

	server := &dns.Server{
		Addr:      addr,
		Net:       "udp",
		ReusePort: true,
		Handler:   s.handler,
	}

	go func() {
		<-ctx.Done()
		server.Shutdown()
	}()

	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("DNS server error on %s: %v\n", addr, err)
	}
}

func (s *Server) Stop(ctx context.Context) {
	s.wg.Wait()
}
