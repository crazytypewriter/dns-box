package dns

import (
	"context"
	"fmt"
	"github.com/crazytypewriter/dns-box/internal/config"
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

// Start не блокирует: на каждый адрес поднимается своя горутина. Вызывать
// через `go` не нужно — иначе Stop может обогнать s.wg.Add и не дождаться
// остановки серверов.
func (s *Server) Start(ctx context.Context) {
	for _, addr := range s.cfg.Server.Address {
		// UDP и TCP на каждом адресе: при TC=1 клиент уходит в TCP,
		// там его раньше ждал refused
		s.wg.Add(2)
		go s.startServer(ctx, addr, "udp")
		go s.startServer(ctx, addr, "tcp")
	}
}

func (s *Server) startServer(ctx context.Context, addr, net string) {
	defer s.wg.Done()

	server := &dns.Server{
		Addr:      addr,
		Net:       net,
		ReusePort: true,
		Handler:   s.handler,
	}

	go func() {
		<-ctx.Done()
		server.Shutdown()
	}()

	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("DNS server error on %s (%s): %v\n", addr, net, err)
	}
}

func (s *Server) Stop(ctx context.Context) {
	s.wg.Wait()
}
