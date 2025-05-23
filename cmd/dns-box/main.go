package main

import (
	"context"
	"flag"
	"github.com/crazytypewriter/dns-box/internal/api"
	"github.com/crazytypewriter/dns-box/internal/cache"
	C "github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	"github.com/crazytypewriter/dns-box/internal/dns"
	"github.com/crazytypewriter/dns-box/internal/ipset"
	log "github.com/sirupsen/logrus"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var configPath string

func init() {
	flag.StringVar(&configPath, "config", "config.json", "path to config file")
}

func main() {
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Infof("Received signal: %v", sig)
		cancel()
	}()

	if err := run(ctx); err != nil {
		log.Fatalf("Application error: %v", err)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}

	l := log.New()
	l.SetLevel(log.DebugLevel)
	l.SetFormatter(&log.TextFormatter{
		ForceColors: true,
	})

	dnsCache := C.NewDNSCache(1024*1024*8, l) // 8MB
	domainCache := cache.NewDomainCache(1024 * 1024 * 8)

	for _, domain := range cfg.Rules.Domains {
		domainCache.Add(domain)
	}
	for _, suffix := range cfg.Rules.DomainSuffix {
		domainCache.AddSuffix(suffix)
	}

	ipSet := ipset.New()
	if cfg.IPSet.IPv4Name != "" {
		if err := ipSet.CreateIPv4Set(cfg.IPSet.IPv4Name, 7200); err != nil {
			l.Errorf("Error creating IPv4 set: %v", err)
			return err
		}
	}
	if cfg.IPSet.IPv6Name != "" {
		if err := ipSet.CreateIPv6Set(cfg.IPSet.IPv6Name, 7200); err != nil {
			l.Errorf("Error creating IPv6 set: %v", err)
			return err
		}
	}

	dnsHandler := dns.NewDnsHandler(cfg, dnsCache, domainCache, ipSet, l)
	dnsServer := dns.NewServer(cfg, dnsHandler)
	go dnsServer.Start(ctx)
	l.Infof("DNS server started on %s", cfg.Server.Address[0])

	apiServer := api.NewServer(cfg, dnsCache, domainCache, l)
	go apiServer.Start(ctx, ":8090")

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	l.Infof("Shutting down DNS server...")
	dnsServer.Stop(shutdownCtx)
	apiServer.Stop(shutdownCtx)
	err = cfg.SaveWithUpdatedRules()
	if err != nil {
		l.Errorf("Failed to save config: %v", err)
	}

	return nil
}
