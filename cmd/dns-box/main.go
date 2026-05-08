package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
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

	// Initialize per-list domain caches
	listDomainCaches := make(map[int]*cache.DomainCache)
	ipSetLists := cfg.GetIPSetLists()
	for i, listCfg := range ipSetLists {
		listCache := cache.NewDomainCache(1024 * 1024 * 2) // 2MB per list
		for _, domain := range listCfg.Rules.Domains {
			listCache.Add(domain)
		}
		for _, suffix := range listCfg.Rules.DomainSuffix {
			listCache.AddSuffix(suffix)
		}
		listDomainCaches[i] = listCache
		l.Debugf("Initialized domain cache for ipset list %d: %s (%d domains, %d suffixes)",
			i, listCfg.Name, len(listCfg.Rules.Domains), len(listCfg.Rules.DomainSuffix))
	}

	for _, listCfg := range ipSetLists {
		timeout := listCfg.Timeout
		if timeout == 0 {
			timeout = 7200 // default timeout
		}

		l.Debugf("Creating IPv4 set: %s", listCfg.Name)
		if err := ipSet.CreateIPv4Set(listCfg.Name, timeout); err != nil {
			l.Errorf("Error creating IPv4 set %s: %v", listCfg.Name, err)
			return err
		}
		l.Debugf("IPv4 set %s created successfully.", listCfg.Name)

		if listCfg.EnableIPv6 {
			ipv6Name := listCfg.Name + "6"
			l.Debugf("Creating IPv6 set: %s", ipv6Name)
			if err := ipSet.CreateIPv6Set(ipv6Name, timeout); err != nil {
				l.Errorf("Error creating IPv6 set %s: %v", ipv6Name, err)
				return err
			}
			l.Debugf("IPv6 set %s created successfully.", ipv6Name)
		}
	}

	// Initialize net lists (hash:net sets with static CIDRs)
	for _, netListCfg := range cfg.IPSet.NetLists {
		timeout := netListCfg.Timeout
		if timeout == 0 {
			timeout = 7200
		}

		l.Debugf("Creating IPv4 net set: %s", netListCfg.Name)
		if err := ipSet.CreateIPv4NetSet(netListCfg.Name, timeout); err != nil {
			l.Errorf("Error creating IPv4 net set %s: %v", netListCfg.Name, err)
			return err
		}
		l.Debugf("IPv4 net set %s created successfully.", netListCfg.Name)

		if netListCfg.EnableIPv6 {
			ipv6Name := netListCfg.Name + "6"
			l.Debugf("Creating IPv6 net set: %s", ipv6Name)
			if err := ipSet.CreateIPv6NetSet(ipv6Name, timeout); err != nil {
				l.Errorf("Error creating IPv6 net set %s: %v", ipv6Name, err)
				return err
			}
			l.Debugf("IPv6 net set %s created successfully.", ipv6Name)
		}

		for _, cidr := range netListCfg.CIDRs {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				l.Warnf("Invalid CIDR %s in net list %s: %v", cidr, netListCfg.Name, err)
				continue
			}
			if ipNet.IP.To4() != nil {
				if addErr := ipSet.AddElement(netListCfg.Name, cidr, timeout); addErr != nil {
					l.Errorf("Error adding CIDR %s to net set %s: %v", cidr, netListCfg.Name, addErr)
				}
			} else {
				if netListCfg.EnableIPv6 {
					ipv6Name := netListCfg.Name + "6"
					if addErr := ipSet.AddElement(ipv6Name, cidr, timeout); addErr != nil {
						l.Errorf("Error adding CIDR %s to net set %s: %v", cidr, ipv6Name, addErr)
					}
				}
			}
		}
	}

	// Инициализация и запуск BlockList
	var blockList *blocklist.BlockList
	if cfg.BlockList.Enabled {
		blockList = blocklist.NewBlockList(&cfg.BlockList, l)
		go blockList.Start(ctx)
	}

	dnsHandler := dns.NewDnsHandler(cfg, dnsCache, domainCache, ipSet, blockList, listDomainCaches, l)
	dnsServer := dns.NewServer(cfg, dnsHandler)
	go dnsServer.Start(ctx)
	l.Infof("DNS server started on %s", cfg.Server.Address[0])

	apiServer := api.NewServer(cfg, dnsCache, domainCache, blockList, listDomainCaches, ipSet, l)
	go apiServer.Start(ctx, ":8090")

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	l.Infof("Shutting down DNS server...")

	// Сохраняем конфиг в GitHub ПЕРЕД остановкой DNS сервера
	l.Info("Saving config to disk and GitHub...")
	if err := cfg.SaveConfig(); err != nil {
		l.Errorf("Failed to save config: %v", err)
	} else {
		l.Info("Config saved successfully")
	}

	l.Info("Stopping DNS server...")
	dnsServer.Stop(shutdownCtx)
	l.Info("Stopping API server...")
	apiServer.Stop(shutdownCtx)

	return ctx.Err()
}
