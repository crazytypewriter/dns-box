package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crazytypewriter/dns-box/internal/api"
	"github.com/crazytypewriter/dns-box/internal/blocklist"
	"github.com/crazytypewriter/dns-box/internal/cache"
	C "github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	"github.com/crazytypewriter/dns-box/internal/dns"
	"github.com/crazytypewriter/dns-box/internal/ipset"
	log "github.com/sirupsen/logrus"
)

var configPath string

func init() {
	flag.StringVar(&configPath, "config", "config.json", "path to config file")
}

func main() {
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Debug("Setting up signal handler...")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigChan:
			log.Infof("Received signal: %v. Initiating graceful shutdown...", sig)
			cancel()
		case <-ctx.Done():
			log.Debug("Context cancelled, signal handler goroutine exiting.")
			return
		}
	}()

	if err := run(ctx, configPath, nil); err != nil && err != context.Canceled {
		log.Fatalf("Application error: %v", err)
	}
}

func run(ctx context.Context, configPath string, logOutput io.Writer) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}

	l := log.New()
	if logOutput != nil {
		l.SetOutput(logOutput)
	}

	logLevel, err := log.ParseLevel(cfg.Server.Log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing log level: %v\n", err)
		l.SetLevel(log.InfoLevel)
	} else {
		l.SetLevel(logLevel)
	}

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

	l.Debugf("Initializing ipset...")
	ipSet := ipset.New()
	l.Debugf("IPSet initialized.")
	if cfg.IPSet.IPv4Name != "" {
		l.Debugf("Creating IPv4 set: %s", cfg.IPSet.IPv4Name)
		if err := ipSet.CreateIPv4Set(cfg.IPSet.IPv4Name, 7200); err != nil {
			l.Errorf("Error creating IPv4 set: %v", err)
			return err
		}
		l.Debugf("IPv4 set %s created successfully.", cfg.IPSet.IPv4Name)
	}
	if cfg.IPSet.IPv6Name != "" {
		l.Debugf("Creating IPv6 set: %s", cfg.IPSet.IPv6Name)
		if err := ipSet.CreateIPv6Set(cfg.IPSet.IPv6Name, 7200); err != nil {
			l.Errorf("Error creating IPv6 set: %v", err)
			return err
		}
		l.Debugf("IPv6 set %s created successfully.", cfg.IPSet.IPv6Name)
	}

	// Инициализация и запуск BlockList
	var blockList *blocklist.BlockList
	if cfg.BlockList.Enabled {
		blockList = blocklist.NewBlockList(&cfg.BlockList, l)
		go blockList.Start(ctx)
	}

	dnsHandler := dns.NewDnsHandler(cfg, dnsCache, domainCache, ipSet, blockList, l)
	dnsServer := dns.NewServer(cfg, dnsHandler)
	go dnsServer.Start(ctx)
	l.Infof("DNS server started on %s", cfg.Server.Address[0])

	apiServer := api.NewServer(cfg, dnsCache, domainCache, blockList, l)
	go apiServer.Start(ctx, ":8090")

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	l.Infof("Shutting down DNS server...")
	dnsServer.Stop(shutdownCtx)
	apiServer.Stop(shutdownCtx)
	err = cfg.SaveConfig()
	if err != nil {
		l.Errorf("Failed to save config: %v", err)
	}

	return ctx.Err()
}
