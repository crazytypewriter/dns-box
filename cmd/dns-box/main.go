package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/crazytypewriter/dns-box/internal/api"
	"github.com/crazytypewriter/dns-box/internal/blocklist"
	"github.com/crazytypewriter/dns-box/internal/cache"
	C "github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	"github.com/crazytypewriter/dns-box/internal/dns"
	"github.com/crazytypewriter/dns-box/internal/ipset"
	"github.com/crazytypewriter/dns-box/internal/ipsetstate"
	log "github.com/sirupsen/logrus"
)

// Значения подставляются при сборке через -ldflags "-X main.version=..."
// (см. LDFLAGS в Makefile). version обязана совпадать с тегом релиза:
// deploy/dns-box.init.d сравнивает вывод `dns-box -version` с tag_name из
// GitHub API и без совпадения перекачивает бинарь на каждом рестарте.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	configPath  string
	showVersion bool
)

func init() {
	flag.StringVar(&configPath, "config", "config.json", "path to config file")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
}

// buildVersion возвращает версию, подставленную линковщиком, а при сборке
// без -ldflags (`go build`, `go install`) достаёт что может из build info.
func buildVersion() (ver, rev string) {
	ver, rev = version, commit
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ver, rev
	}
	if ver == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		ver = info.Main.Version
	}
	if rev == "none" {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				rev = s.Value
				if len(rev) > 7 {
					rev = rev[:7]
				}
			}
		}
	}
	return ver, rev
}

// versionInfo печатает версию. Первая строка — "dns-box <version>": её
// парсит init.d-скрипт как `head -n1 | awk '{print $NF}'`, так что версия
// обязана оставаться последним полем первой строки.
func versionInfo() string {
	ver, rev := buildVersion()
	return fmt.Sprintf("dns-box %s\ncommit: %s\nbuilt:  %s\ngo:     %s %s/%s\n",
		ver, rev, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func main() {
	flag.Parse()

	if showVersion {
		fmt.Print(versionInfo())
		os.Exit(0)
	}

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

	ver, rev := buildVersion()
	l.Infof("dns-box %s (commit %s, built %s, %s %s/%s)", ver, rev, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// Восстановление динамических списков из GitHub backup.
	if cfg.GithubBackup.Enabled {
		l.Info("Loading dynamic lists from GitHub backup...")
		githubCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := cfg.MergeFromGitHub(githubCtx); err != nil {
			l.Warnf("Failed to load GitHub backup: %v. Continuing with local config.", err)
		} else {
			l.Info("GitHub backup loaded and merged successfully")
			// Синхронизируем локальный config.json с GitHub сразу после старта.
			l.Info("Syncing local config after GitHub restore...")
			if saveErr := cfg.SaveConfig(); saveErr != nil {
				l.Warnf("Failed to sync local config after GitHub restore: %v", saveErr)
			} else {
				l.Info("Local config synced successfully")
			}
		}
		cancel()
	}

	dnsCache := C.NewDNSCache(1024*1024*8, l) // 8MB
	domainCache := cache.NewDomainCache(1024 * 1024 * 8)

	for _, domain := range cfg.Rules.Domains {
		domainCache.Add(domain)
	}
	for _, suffix := range cfg.Rules.DomainSuffix {
		domainCache.AddSuffix(suffix)
	}

	l.Infof("Initializing ipset...")
	ipSet, err := ipset.New()
	if err != nil {
		l.Errorf("Failed to initialize ipset: %v", err)
		return err
	}
	l.Infof("IPSet initialized.")

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

		if listCfg.MaxElem != 0 {
			l.Infof("List %s: maxelem %d (применяется только если сет создаётся сейчас; у существующего сета лимит не меняется)", listCfg.Name, listCfg.MaxElem)
		}

		l.Infof("Creating IPv4 set: %s", listCfg.Name)
		if err := ipSet.CreateIPv4Set(listCfg.Name, timeout, listCfg.MaxElem); err != nil {
			l.Errorf("Error creating IPv4 set %s: %v", listCfg.Name, err)
			return err
		}
		l.Infof("IPv4 set %s ready.", listCfg.Name)

		if listCfg.EnableIPv6 {
			ipv6Name := listCfg.Name + "6"
			l.Infof("Creating IPv6 set: %s", ipv6Name)
			if err := ipSet.CreateIPv6Set(ipv6Name, timeout, listCfg.MaxElem); err != nil {
				l.Errorf("Error creating IPv6 set %s: %v", ipv6Name, err)
				return err
			}
			l.Infof("IPv6 set %s ready.", ipv6Name)
		}
	}

	// Initialize net lists (hash:net sets with static CIDRs)
	for _, netListCfg := range cfg.IPSet.NetLists {
		timeout := netListCfg.Timeout
		if timeout == 0 {
			timeout = 7200
		}

		l.Infof("Creating IPv4 net set: %s", netListCfg.Name)
		if err := ipSet.CreateIPv4NetSet(netListCfg.Name, timeout, netListCfg.MaxElem); err != nil {
			l.Errorf("Error creating IPv4 net set %s: %v", netListCfg.Name, err)
			return err
		}
		l.Infof("IPv4 net set %s ready.", netListCfg.Name)

		if netListCfg.EnableIPv6 {
			ipv6Name := netListCfg.Name + "6"
			l.Infof("Creating IPv6 net set: %s", ipv6Name)
			if err := ipSet.CreateIPv6NetSet(ipv6Name, timeout, netListCfg.MaxElem); err != nil {
				l.Errorf("Error creating IPv6 net set %s: %v", ipv6Name, err)
				return err
			}
			l.Infof("IPv6 net set %s ready.", ipv6Name)
		}
	}

	// Файл состояния ipset: восстановление после ребута (фаза 3 спеки).
	// Порядок важен: сеты уже созданы, DNS-сервер и API ещё не запущены.
	var stateStore *ipsetstate.Store
	if cfg.State.Enabled {
		statePath := cfg.State.Path
		if statePath == "" {
			statePath = filepath.Join(filepath.Dir(configPath), "ipset-state.json")
		}
		stateStore = ipsetstate.NewStore(cfg.State.MaxEntriesPerSet, l)

		if entries, err := ipsetstate.Load(statePath); err != nil {
			l.Warnf("Failed to load ipset state (%v), starting with empty sets mirror", err)
		} else {
			validSets := make(map[string]bool)
			for _, listCfg := range ipSetLists {
				validSets[listCfg.Name] = true
				if listCfg.EnableIPv6 {
					validSets[listCfg.Name+"6"] = true
				}
			}
			for _, netListCfg := range cfg.IPSet.NetLists {
				validSets[netListCfg.Name] = true
				if netListCfg.EnableIPv6 {
					validSets[netListCfg.Name+"6"] = true
				}
			}

			restored, skipped := restoreEntries(entries, validSets, ipSet, l)
			if skipped > 0 {
				l.Infof("Skipped %d ipset entries already present in the kernel", skipped)
			}
			stateStore.Restore(entries)
			for set, n := range restored {
				if n > 0 {
					l.Infof("Restored %d entries into ipset %s", n, set)
				}
			}
		}

		// Периодический флаш на диск + флаш при остановке.
		stateFlushPath := statePath
		go func() {
			ticker := time.NewTicker(time.Duration(cfg.State.FlushIntervalMinutes) * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := stateStore.FlushIfDirty(stateFlushPath); err != nil {
						l.Warnf("Failed to flush ipset state: %v", err)
					}
				}
			}
		}()
		defer func() {
			if err := stateStore.FlushIfDirty(stateFlushPath); err != nil {
				l.Errorf("Failed to flush ipset state on shutdown: %v", err)
			}
		}()
	}

	// Наполнение net lists: первый прогон reconciler'а сразу при старте,
	// далее периодически по refresh_minutes.
	reconcileNetLists(cfg, ipSet, stateStore, l)
	go netListReconciler(ctx, cfg, ipSet, stateStore, l)

	// Инициализация и запуск BlockList
	var blockList *blocklist.BlockList
	if cfg.BlockList.Enabled {
		blockList = blocklist.NewBlockList(&cfg.BlockList, l)
		go blockList.Start(ctx)
	}

	dnsHandler := dns.NewDnsHandler(cfg, dnsCache, domainCache, ipSet, blockList, listDomainCaches, stateStore, l)
	dnsHandler.StartHostsReloader(ctx)
	dnsServer := dns.NewServer(cfg, dnsHandler)
	dnsServer.Start(ctx)
	l.Infof("DNS server started on %s", cfg.Server.Address[0])

	apiServer := api.NewServer(cfg, dnsCache, domainCache, blockList, listDomainCaches, ipSet, stateStore, l)
	apiServer.Start(ctx, ":8090")

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

// restoreEntries заливает записи файла состояния обратно в ipset и
// возвращает счётчики восстановленных по сетам и пропущенных.
//
// Записи, которые уже лежат в сете, пропускаются: при перезапуске
// процесса (в отличие от ребута) сеты живы, и таймеры ядра точнее наших —
// в том числе продлённые правилом -j SET --exist, которого файл состояния
// не видит. Повторный Add укоротил бы им срок жизни.
func restoreEntries(entries []ipsetstate.Entry, validSets map[string]bool, ipSet ipset.Manager, l *log.Logger) (map[string]int, int) {
	present := make(map[string]map[string]bool, len(validSets))
	for set := range validSets {
		ips, err := ipSet.ListElements(set)
		if err != nil {
			l.Debugf("Cannot list ipset %s (%v), restoring all entries for it", set, err)
			continue
		}
		inSet := make(map[string]bool, len(ips))
		for _, ip := range ips {
			inSet[ip] = true
		}
		present[set] = inSet
	}

	now := time.Now().Unix()
	restored := make(map[string]int)
	skipped := 0
	for _, e := range entries {
		if !validSets[e.Set] {
			continue // сета больше нет в конфиге
		}
		if present[e.Set][e.IP] {
			skipped++
			continue // уже в сете, таймер ядра оставляем как есть
		}
		remaining := ipsetstate.RemainingTTL(e, now)
		if e.Expire != 0 && remaining == 0 {
			continue // истекла за время простоя
		}
		if err := ipSet.AddElement(e.Set, e.IP, remaining); err != nil {
			l.Warnf("Failed to restore %s in set %s: %v", e.IP, e.Set, err)
			continue
		}
		restored[e.Set]++
	}
	return restored, skipped
}

// reconcileNetLists передобавляет все CIDR из net_lists в соответствующие
// сеты. Повторный Add существующей записи безвреден и заодно сбрасывает
// таймер у неперсистентных. Для persistent-списков записи добавляются с
// timeout=0 (без срока жизни).
func reconcileNetLists(cfg *config.Config, ipSet ipset.Manager, stateStore *ipsetstate.Store, l *log.Logger) {
	for _, netListCfg := range cfg.GetNetLists() {
		timeout := netListCfg.Timeout
		if timeout == 0 {
			timeout = 7200
		}
		if netListCfg.IsPersistent() {
			timeout = 0 // вечные записи
		}

		for _, cidr := range netListCfg.CIDRs {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				l.Warnf("Invalid CIDR %s in net list %s: %v", cidr, netListCfg.Name, err)
				continue
			}
			target := netListCfg.Name
			if ipNet.IP.To4() == nil {
				if !netListCfg.EnableIPv6 {
					continue
				}
				target = netListCfg.Name + "6"
			}
			if addErr := ipSet.AddElement(target, cidr, timeout); addErr != nil {
				l.Warnf("Reconcile: error adding CIDR %s to net set %s: %v", cidr, target, addErr)
				continue
			}
			if stateStore != nil {
				stateStore.Record(target, cidr, timeout)
			}
		}
	}
}

// netListReconciler периодически вызывает reconcileNetLists. Для каждого
// списка берётся свой refresh_minutes (nil → 60, явный 0/минус — выключено);
// тикер общий — по минимальному включённому интервалу.
func netListReconciler(ctx context.Context, cfg *config.Config, ipSet ipset.Manager, stateStore *ipsetstate.Store, l *log.Logger) {
	minutes := 0
	for _, netListCfg := range cfg.GetNetLists() {
		if m := netListCfg.RefreshInterval(); m > 0 && (minutes == 0 || m < minutes) {
			minutes = m
		}
	}
	if minutes == 0 {
		l.Info("Net list reconciler disabled (refresh_minutes = 0 for all lists)")
		return
	}

	ticker := time.NewTicker(time.Duration(minutes) * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileNetLists(cfg, ipSet, stateStore, l)
		}
	}
}
