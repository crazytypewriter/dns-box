package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/crazytypewriter/dns-box/internal/github"
)

type ServerConfig struct {
	Address []string `json:"address"`
	Log     string   `json:"log"`
}

type DNSConfig struct {
	UpstreamServers []string `json:"upstream_servers"`
	Timeout         int      `json:"timeout"`
	// ForwardZones — условная пересылка: запросы для указанных зон
	// отправляются на специфичные upstream-серверы вместо глобальных.
	ForwardZones []ForwardZoneConfig `json:"forward_zones"`
	// HostsFile — путь к файлу в формате /etc/hosts для локальных
	// переопределений A/AAAA-записей (пусто = отключено).
	HostsFile string `json:"hosts_file"`
	// RateLimit — максимальное число запросов в секунду с одного клиента
	// (0 = без ограничений).
	RateLimit int `json:"rate_limit"`
	// BlockPrivate — защита от DNS Rebinding: не возвращать приватные
	// IP-адреса (RFC 1918/4193, loopback, link-local) в ответах.
	BlockPrivate bool `json:"block_private"`
}

type ForwardZoneConfig struct {
	// DomainSuffix — суффикс зоны, например ".corp.local" или "corp.local".
	DomainSuffix string `json:"domain_suffix"`
	// Servers — upstream-серверы для этой зоны (те же форматы, что и выше).
	Servers []string `json:"servers"`
}

type IPSetListConfig struct {
	Name       string      `json:"name"`
	EnableIPv6 bool        `json:"enable_ipv6"`
	Timeout    uint32      `json:"timeout"`    // in seconds, 0 means use default
	Persistent bool        `json:"persistent"` // записи без срока жизни (timeout 0)
	Prefetch   bool        `json:"prefetch"`   // префетч доменов этого списка у границы TTL
	MaxElem    uint32      `json:"maxelem"`    // 0 — дефолт ядра (65536); применяется только при создании сета
	Rules      RulesConfig `json:"rules"`
}

type IPSetConfig struct {
	IPv4Name string            `json:"ipv4name"`  // deprecated, kept for backward compatibility
	IPv6Name string            `json:"ipv6name"`  // deprecated, kept for backward compatibility
	Lists    []IPSetListConfig `json:"lists"`     // new multi-list config
	NetLists []NetListConfig   `json:"net_lists"` // static CIDR net lists
}

type NetListConfig struct {
	Name       string   `json:"name"`
	EnableIPv6 bool     `json:"enable_ipv6"`
	Timeout    uint32   `json:"timeout"`
	ASN        string   `json:"asn,omitempty"`
	CIDRs      []string `json:"cidr"`
	// MaxElem — 0 значит дефолт ядра (65536); применяется только при
	// создании сета. Крупным ASN-спискам его стоит поднимать.
	MaxElem uint32 `json:"maxelem"`
	// Persistent — записи без срока жизни. Указатель, чтобы отличить
	// «не задано» от явного false: дефолт для net_lists — true.
	Persistent *bool `json:"persistent"`
	// RefreshMinutes — период передобавления CIDR (reconcile).
	// nil/не задано → 60; явный 0 или отрицательное — выключено.
	RefreshMinutes *int `json:"refresh_minutes"`
}

// IsPersistent — дефолт true для net_lists (статические CIDR не должны истекать).
func (n NetListConfig) IsPersistent() bool {
	return n.Persistent == nil || *n.Persistent
}

// RefreshInterval — интервал reconcile в минутах; 0 — периодический reconcile выключен.
func (n NetListConfig) RefreshInterval() int {
	if n.RefreshMinutes == nil {
		return 60
	}
	if *n.RefreshMinutes < 0 {
		return 0
	}
	return *n.RefreshMinutes
}

// StateConfig — файл состояния ipset для восстановления после ребута.
type StateConfig struct {
	Enabled              bool   `json:"enabled"`
	Path                 string `json:"path"`
	FlushIntervalMinutes int    `json:"flush_interval_minutes"` // дефолт 10
	MaxEntriesPerSet     int    `json:"max_entries_per_set"`    // дефолт 20000
}

type RulesConfig struct {
	Domains      []string `json:"domain"`
	DomainSuffix []string `json:"domain_suffix"`
}

type BlockListConfig struct {
	Enabled      bool     `json:"enabled"`
	URLs         []string `json:"urls"`
	RefreshHours int      `json:"refresh_hours"`
}

// APIConfig — HTTP-управление. Address по умолчанию ":8090".
// Токен: приоритет у переменной окружения DNS_BOX_API_TOKEN,
// поле token — фолбэк (лучше держать пустым и использовать env).
type APIConfig struct {
	Address string `json:"address"`
	Token   string `json:"token"`
}

const APITokenEnv = "DNS_BOX_API_TOKEN"

// GetToken возвращает токен API: приоритет у переменной окружения.
func (a APIConfig) GetToken() string {
	if token := os.Getenv(APITokenEnv); token != "" {
		return token
	}
	return a.Token
}

type Config struct {
	Server       ServerConfig    `json:"server"`
	DNS          DNSConfig       `json:"dns"`
	IPSet        IPSetConfig     `json:"ipset"`
	Rules        RulesConfig     `json:"rules"`
	BlockList    BlockListConfig `json:"blocklist"`
	GithubBackup GithubConfig    `json:"github_backup"`
	State        StateConfig     `json:"state"`
	API          APIConfig       `json:"api"`
	mu           sync.RWMutex    `json:"-"`
	Path         string          `json:"-"`
}

const GitHubTokenEnv = "DNS_BOX_GITHUB_TOKEN"

type GithubConfig struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Branch  string `json:"branch"`
}

// GetToken возвращает GitHub-токен: приоритет у переменной окружения DNS_BOX_GITHUB_TOKEN,
// если она не задана — используется значение из конфигурации.
func (g GithubConfig) GetToken() string {
	if token := os.Getenv(GitHubTokenEnv); token != "" {
		return token
	}
	return g.Token
}

type HostsConfig struct {
	Domains      []string          `json:"domain"`
	DomainSuffix []string          `json:"domain_suffix"`
	IPSetLists   []IPSetListConfig `json:"ipset_lists"` // new multi-list format
	NetLists     []NetListConfig   `json:"net_lists"`   // static CIDR net lists
}

func LoadConfig(filename string) (*Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var cfg Config
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return nil, err
	}

	cfg.Path = filename
	normalizeSlices(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}
	return &cfg, nil
}

// Validate проверяет критичные поля конфигурации после загрузки.
func (c *Config) Validate() error {
	if len(c.Server.Address) == 0 {
		return errors.New("server.address is empty")
	}
	if len(c.DNS.UpstreamServers) == 0 {
		return errors.New("dns.upstream_servers is empty")
	}
	for i, z := range c.DNS.ForwardZones {
		if z.DomainSuffix == "" {
			return fmt.Errorf("dns.forward_zones[%d].domain_suffix is empty", i)
		}
		if len(z.Servers) == 0 {
			return fmt.Errorf("dns.forward_zones[%d].servers is empty", i)
		}
	}
	if c.IPSet.Lists == nil {
		return errors.New("ipset.lists is nil after normalization")
	}
	if c.IPSet.NetLists == nil {
		return errors.New("ipset.net_lists is nil after normalization")
	}
	if c.GithubBackup.Enabled {
		if c.GithubBackup.Owner == "" {
			return errors.New("github_backup.owner is empty")
		}
		if c.GithubBackup.Repo == "" {
			return errors.New("github_backup.repo is empty")
		}
		if c.GithubBackup.Path == "" {
			return errors.New("github_backup.path is empty")
		}
		if c.GithubBackup.Branch == "" {
			return errors.New("github_backup.branch is empty")
		}
		if c.GithubBackup.GetToken() == "" {
			return errors.New("github_backup token is missing (set DNS_BOX_GITHUB_TOKEN env or token field)")
		}
	}
	return nil
}

// normalizeSlices заменяет nil-срезы на пустые срезы, чтобы при сохранении
// они сериализовались как [] вместо null и не терялась структура конфигурации.
func normalizeSlices(c *Config) {
	if c.IPSet.Lists == nil {
		c.IPSet.Lists = []IPSetListConfig{}
	}
	if c.IPSet.NetLists == nil {
		c.IPSet.NetLists = []NetListConfig{}
	}
	if c.Rules.Domains == nil {
		c.Rules.Domains = []string{}
	}
	if c.Rules.DomainSuffix == nil {
		c.Rules.DomainSuffix = []string{}
	}
	if c.BlockList.URLs == nil {
		c.BlockList.URLs = []string{}
	}
	for i := range c.IPSet.Lists {
		if c.IPSet.Lists[i].Rules.Domains == nil {
			c.IPSet.Lists[i].Rules.Domains = []string{}
		}
		if c.IPSet.Lists[i].Rules.DomainSuffix == nil {
			c.IPSet.Lists[i].Rules.DomainSuffix = []string{}
		}
	}
	for i := range c.IPSet.NetLists {
		if c.IPSet.NetLists[i].CIDRs == nil {
			c.IPSet.NetLists[i].CIDRs = []string{}
		}
	}
	if c.Server.Address == nil {
		c.Server.Address = []string{}
	}
	if c.DNS.UpstreamServers == nil {
		c.DNS.UpstreamServers = []string{}
	}
	if c.DNS.ForwardZones == nil {
		c.DNS.ForwardZones = []ForwardZoneConfig{}
	}
	for i := range c.DNS.ForwardZones {
		if c.DNS.ForwardZones[i].Servers == nil {
			c.DNS.ForwardZones[i].Servers = []string{}
		}
	}
	// Дефолты state-блока
	if c.State.FlushIntervalMinutes <= 0 {
		c.State.FlushIntervalMinutes = 10
	}
	if c.State.MaxEntriesPerSet <= 0 {
		c.State.MaxEntriesPerSet = 20000
	}
	if c.API.Address == "" {
		c.API.Address = ":8090"
	}
}

// normalizeHostsSlices заменяет nil-срезы в HostsConfig на пустые срезы,
// чтобы при сериализации в GitHub они записывались как [] вместо null.
func normalizeHostsSlices(h *HostsConfig) {
	if h.Domains == nil {
		h.Domains = []string{}
	}
	if h.DomainSuffix == nil {
		h.DomainSuffix = []string{}
	}
	if h.IPSetLists == nil {
		h.IPSetLists = []IPSetListConfig{}
	}
	if h.NetLists == nil {
		h.NetLists = []NetListConfig{}
	}
	for i := range h.IPSetLists {
		if h.IPSetLists[i].Rules.Domains == nil {
			h.IPSetLists[i].Rules.Domains = []string{}
		}
		if h.IPSetLists[i].Rules.DomainSuffix == nil {
			h.IPSetLists[i].Rules.DomainSuffix = []string{}
		}
	}
	for i := range h.NetLists {
		if h.NetLists[i].CIDRs == nil {
			h.NetLists[i].CIDRs = []string{}
		}
	}
}

// rotateBackups сдвигает цепочку локальных бэкапов перед записью нового конфига.
// Схема: config.json -> config.json.bak -> config.json.bak.1 -> config.json.bak.2 -> config.json.bak.3
// Хранится не более 3 исторических бэкапов + config.json.bak.
func rotateBackups(path string) error {
	const maxBackups = 3

	// config.json.bak.3 удаляем, если есть
	oldest := fmt.Sprintf("%s.bak.%d", path, maxBackups)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove oldest backup %s: %w", oldest, err)
	}

	// Сдвигаем: .bak.2 -> .bak.3, .bak.1 -> .bak.2
	for i := maxBackups - 1; i >= 1; i-- {
		oldPath := fmt.Sprintf("%s.bak.%d", path, i)
		newPath := fmt.Sprintf("%s.bak.%d", path, i+1)
		if _, err := os.Stat(oldPath); err == nil {
			if err := os.Rename(oldPath, newPath); err != nil {
				return fmt.Errorf("failed to rotate backup %s -> %s: %w", oldPath, newPath, err)
			}
		}
	}

	// config.json.bak -> config.json.bak.1
	bakPath := path + ".bak"
	if _, err := os.Stat(bakPath); err == nil {
		if err := os.Rename(bakPath, path+".bak.1"); err != nil {
			return fmt.Errorf("failed to rotate backup %s -> %s: %w", bakPath, path+".bak.1", err)
		}
	}

	// Текущий config.json -> config.json.bak
	if _, err := os.Stat(path); err == nil {
		if err := os.Link(path, bakPath); err != nil {
			return fmt.Errorf("failed to create backup link %s -> %s: %w", path, bakPath, err)
		}
	}

	return nil
}

// LoadConfigFromGitHub загружает конфигурацию из GitHub репозитория.
func LoadConfigFromGitHub(ctx context.Context, githubCfg GithubConfig) (*HostsConfig, error) {
	if !githubCfg.Enabled {
		return nil, errors.New("github backup is not enabled")
	}

	client := github.NewClient(githubCfg.GetToken())
	content, err := client.LoadFile(ctx, githubCfg.Owner, githubCfg.Repo, githubCfg.Path, githubCfg.Branch)
	if err != nil {
		return nil, err
	}

	var hostsConfig HostsConfig
	if err := json.Unmarshal(content, &hostsConfig); err != nil {
		return nil, err
	}

	normalizeHostsSlices(&hostsConfig)
	return &hostsConfig, nil
}

// MergeFromGitHub загружает динамические списки из GitHub и смерживает их
// в текущий конфиг. GitHub считается источником истины для ipset_lists,
// net_lists, domains и domain_suffix. Статические поля (server, dns и т.д.)
// остаются из локального конфига.
func (c *Config) MergeFromGitHub(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.GithubBackup.Enabled {
		log.Printf("[config] GitHub backup is disabled, skipping restore")
		return nil
	}

	log.Printf("[config] Fetching GitHub backup from %s/%s/%s (branch=%s)",
		c.GithubBackup.Owner, c.GithubBackup.Repo, c.GithubBackup.Path, c.GithubBackup.Branch)

	hostsConfig, err := LoadConfigFromGitHub(ctx, c.GithubBackup)
	if err != nil {
		log.Printf("[config] Failed to fetch GitHub backup: %v", err)
		return err
	}

	if len(hostsConfig.IPSetLists) > 0 {
		c.IPSet.Lists = hostsConfig.IPSetLists
		c.Rules.Domains = hostsConfig.Domains
		c.Rules.DomainSuffix = hostsConfig.DomainSuffix
		log.Printf("[config] Restored %d ipset lists from GitHub", len(hostsConfig.IPSetLists))
	}
	if len(hostsConfig.NetLists) > 0 {
		c.IPSet.NetLists = hostsConfig.NetLists
		log.Printf("[config] Restored %d net lists from GitHub", len(hostsConfig.NetLists))
	}

	normalizeSlices(c)
	return nil
}

// SaveConfig сохраняет текущую конфигурацию в файл и, при необходимости, в GitHub.
// GitHub-сохранение выполняется без удержания мьютекса, чтобы не блокировать DNS.
func (c *Config) SaveConfig() error {
	c.mu.Lock()

	if c.Path == "" {
		c.mu.Unlock()
		return ErrNoConfigPath
	}

	// Читаем статичные части из существующего файла, если он есть и валиден.
	// При сбое питания файл может быть пустым/битым — тогда используем in-memory значения.
	staticServer := c.Server
	staticDNS := c.DNS
	staticGithubBackup := c.GithubBackup
	staticAPI := c.API

	file, err := os.Open(c.Path)
	if err == nil {
		var tempConfig struct {
			Server       ServerConfig    `json:"server"`
			DNS          DNSConfig       `json:"dns"`
			IPSet        IPSetConfig     `json:"ipset"`
			GithubBackup GithubConfig    `json:"github_backup"`
			BlockList    BlockListConfig `json:"blocklist"`
			Rules        RulesConfig     `json:"rules"`
			State        StateConfig     `json:"state"`
			API          APIConfig       `json:"api"`
		}
		decodeErr := json.NewDecoder(file).Decode(&tempConfig)
		file.Close()
		if decodeErr == nil {
			staticServer = tempConfig.Server
			staticDNS = tempConfig.DNS
			staticGithubBackup = tempConfig.GithubBackup
			staticAPI = tempConfig.API
		} else {
			log.Printf("[config] Warning: failed to decode existing config (%v), using in-memory static values", decodeErr)
		}
	} else if !os.IsNotExist(err) {
		c.mu.Unlock()
		return err
	}

	// Нормализуем срезы перед сохранением, чтобы пустые списки
	// записывались как [] вместо null. Копируем только динамические части,
	// не трогая мьютекс (ветирование copylocks).
	cfgCopy := Config{IPSet: c.IPSet, Rules: c.Rules, BlockList: c.BlockList}
	normalizeSlices(&cfgCopy)

	finalConfig := struct {
		Server       ServerConfig    `json:"server"`
		DNS          DNSConfig       `json:"dns"`
		IPSet        IPSetConfig     `json:"ipset"`
		GithubBackup GithubConfig    `json:"github_backup"`
		Rules        RulesConfig     `json:"rules"`
		BlockList    BlockListConfig `json:"blocklist"`
		State        StateConfig     `json:"state"`
		API          APIConfig       `json:"api"`
	}{
		Server:       staticServer,
		DNS:          staticDNS,
		IPSet:        cfgCopy.IPSet,
		GithubBackup: staticGithubBackup,
		Rules:        cfgCopy.Rules,
		BlockList:    cfgCopy.BlockList,
		State:        cfgCopy.State,
		API:          staticAPI,
	}

	// Ротируем локальные бэкапы перед записью.
	if err := rotateBackups(c.Path); err != nil {
		log.Printf("[config] Warning: failed to rotate backups: %v", err)
	}

	// Атомарная запись: пишем во временный файл, fsync, затем rename.
	// Если свет моргнёт во время записи, основной config.json останется целым.
	tmpPath := c.Path + ".tmp"
	outFile, err := os.Create(tmpPath)
	if err != nil {
		c.mu.Unlock()
		return err
	}

	encoder := json.NewEncoder(outFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(finalConfig); err != nil {
		outFile.Close()
		os.Remove(tmpPath)
		c.mu.Unlock()
		return err
	}
	if err := outFile.Sync(); err != nil {
		outFile.Close()
		os.Remove(tmpPath)
		c.mu.Unlock()
		return err
	}
	if err := outFile.Close(); err != nil {
		os.Remove(tmpPath)
		c.mu.Unlock()
		return err
	}

	if err := os.Rename(tmpPath, c.Path); err != nil {
		os.Remove(tmpPath)
		c.mu.Unlock()
		return err
	}

	// Копируем данные для GitHub под локом, чтобы отпустить его до сетевого вызова
	var needGitHubSave bool
	var githubToken, githubOwner, githubRepo, githubPath, githubBranch string
	var hostsConfig HostsConfig
	if c.GithubBackup.Enabled {
		needGitHubSave = true
		githubToken = c.GithubBackup.GetToken()
		githubOwner = c.GithubBackup.Owner
		githubRepo = c.GithubBackup.Repo
		githubPath = c.GithubBackup.Path
		githubBranch = c.GithubBackup.Branch

		if len(c.IPSet.Lists) > 0 {
			hostsConfig.IPSetLists = make([]IPSetListConfig, len(c.IPSet.Lists))
			copy(hostsConfig.IPSetLists, c.IPSet.Lists)
		} else {
			hostsConfig.Domains = make([]string, len(c.Rules.Domains))
			copy(hostsConfig.Domains, c.Rules.Domains)
			hostsConfig.DomainSuffix = make([]string, len(c.Rules.DomainSuffix))
			copy(hostsConfig.DomainSuffix, c.Rules.DomainSuffix)
		}
		hostsConfig.NetLists = make([]NetListConfig, len(c.IPSet.NetLists))
		copy(hostsConfig.NetLists, c.IPSet.NetLists)

		log.Printf("[config] Preparing GitHub save: ipset_lists=%d, net_lists=%d, domains=%d, suffixes=%d",
			len(hostsConfig.IPSetLists), len(hostsConfig.NetLists), len(hostsConfig.Domains), len(hostsConfig.DomainSuffix))
	}
	c.mu.Unlock()

	if needGitHubSave {
		log.Printf("[config] Saving rules to GitHub: owner=%s, repo=%s, path=%s, branch=%s",
			githubOwner, githubRepo, githubPath, githubBranch)

		normalizeHostsSlices(&hostsConfig)

		data, err := json.MarshalIndent(hostsConfig, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal hosts config: %w", err)
		}

		client := github.NewClient(githubToken)
		if err := client.SaveFile(context.Background(), githubOwner, githubRepo, githubPath, githubBranch, data); err != nil {
			log.Printf("[config] ERROR: failed to save config to github: %v", err)
			return fmt.Errorf("failed to save config to github: %w", err)
		}
		log.Printf("[config] Successfully saved rules to GitHub")
	}

	return nil
}

func (c *Config) AddDomain(domain string) {
	log.Printf("DEBUG: AddDomain called with domain: %s", domain)
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, d := range c.Rules.Domains {
		if d == domain {
			return
		}
	}

	c.Rules.Domains = append(c.Rules.Domains, domain)
}

func (c *Config) RemoveDomain(domain string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	newDomains := make([]string, 0, len(c.Rules.Domains))
	for _, d := range c.Rules.Domains {
		if d != domain {
			newDomains = append(newDomains, d)
		}
	}
	c.Rules.Domains = newDomains
}

func (c *Config) AddSuffix(suffix string) {
	log.Printf("DEBUG: AddSuffix called with suffix: %s", suffix)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.Rules.DomainSuffix {
		if s == suffix {
			return
		}
	}
	c.Rules.DomainSuffix = append(c.Rules.DomainSuffix, suffix)
}

func (c *Config) RemoveSuffix(suffix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	newSuffixes := make([]string, 0, len(c.Rules.DomainSuffix))
	for _, s := range c.Rules.DomainSuffix {
		if s != suffix {
			newSuffixes = append(newSuffixes, s)
		}
	}
	c.Rules.DomainSuffix = newSuffixes
}

var ErrNoConfigPath = errors.New("no config file path specified")

// AddBlockListURL добавляет новый URL в список, если он еще не существует.
func (c *Config) AddBlockListURL(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, u := range c.BlockList.URLs {
		if u == url {
			return // URL уже существует
		}
	}
	c.BlockList.URLs = append(c.BlockList.URLs, url)
}

// RemoveBlockListURL удаляет URL из списка.
func (c *Config) RemoveBlockListURL(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	newURLs := make([]string, 0, len(c.BlockList.URLs))
	for _, u := range c.BlockList.URLs {
		if u != url {
			newURLs = append(newURLs, u)
		}
	}
	c.BlockList.URLs = newURLs
}

const defaultIPSetTimeout = 7200 // default timeout in seconds

// FindForwardZone возвращает upstream-серверы условной пересылки для домена
// (самая специфичная зона), либо nil, если домен не попадает ни под одну зону.
func (c *Config) FindForwardZone(domain string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	bestLen := -1
	var best []string
	for _, z := range c.DNS.ForwardZones {
		suffix := strings.TrimSuffix(strings.ToLower(z.DomainSuffix), ".")
		if suffix == "" {
			continue
		}
		if domain == suffix || strings.HasSuffix(domain, "."+suffix) {
			// Выбираем самую длинную (специфичную) зону
			if len(suffix) > bestLen {
				bestLen = len(suffix)
				best = z.Servers
			}
		}
	}
	return best
}

// GetIPSetLists returns the list of ipset configurations.
// If Lists is empty, it falls back to the legacy IPv4Name/IPv6Name fields.
func (c *Config) GetIPSetLists() []IPSetListConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.IPSet.Lists) > 0 {
		return c.IPSet.Lists
	}

	// Backward compatibility: convert legacy IPv4Name/IPv6Name to list format
	var lists []IPSetListConfig
	if c.IPSet.IPv4Name != "" {
		lists = append(lists, IPSetListConfig{
			Name:       c.IPSet.IPv4Name,
			EnableIPv6: c.IPSet.IPv6Name != "",
			Timeout:    0,       // will use default
			Rules:      c.Rules, // legacy mode uses shared rules
		})
	}
	return lists
}

// AddDomainToList adds a domain to a specific ipset list's rules.
func (c *Config) AddDomainToList(listIndex int, domain string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Legacy mode: use shared rules
	if len(c.IPSet.Lists) == 0 {
		if listIndex != 0 {
			return
		}
		for _, d := range c.Rules.Domains {
			if d == domain {
				return
			}
		}
		c.Rules.Domains = append(c.Rules.Domains, domain)
		return
	}

	if listIndex < 0 || listIndex >= len(c.IPSet.Lists) {
		return
	}

	rules := &c.IPSet.Lists[listIndex].Rules
	for _, d := range rules.Domains {
		if d == domain {
			return
		}
	}
	rules.Domains = append(rules.Domains, domain)
}

// RemoveDomainFromList removes a domain from a specific ipset list's rules.
func (c *Config) RemoveDomainFromList(listIndex int, domain string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Legacy mode: use shared rules
	if len(c.IPSet.Lists) == 0 {
		if listIndex != 0 {
			return
		}
		newDomains := make([]string, 0, len(c.Rules.Domains))
		for _, d := range c.Rules.Domains {
			if d != domain {
				newDomains = append(newDomains, d)
			}
		}
		c.Rules.Domains = newDomains
		return
	}

	if listIndex < 0 || listIndex >= len(c.IPSet.Lists) {
		return
	}

	rules := &c.IPSet.Lists[listIndex].Rules
	newDomains := make([]string, 0, len(rules.Domains))
	for _, d := range rules.Domains {
		if d != domain {
			newDomains = append(newDomains, d)
		}
	}
	rules.Domains = newDomains
}

// AddSuffixToList adds a suffix to a specific ipset list's rules.
func (c *Config) AddSuffixToList(listIndex int, suffix string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Legacy mode: use shared rules
	if len(c.IPSet.Lists) == 0 {
		if listIndex != 0 {
			return
		}
		for _, s := range c.Rules.DomainSuffix {
			if s == suffix {
				return
			}
		}
		c.Rules.DomainSuffix = append(c.Rules.DomainSuffix, suffix)
		return
	}

	if listIndex < 0 || listIndex >= len(c.IPSet.Lists) {
		return
	}

	rules := &c.IPSet.Lists[listIndex].Rules
	for _, s := range rules.DomainSuffix {
		if s == suffix {
			return
		}
	}
	rules.DomainSuffix = append(rules.DomainSuffix, suffix)
}

// RemoveSuffixFromList removes a suffix from a specific ipset list's rules.
func (c *Config) RemoveSuffixFromList(listIndex int, suffix string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Legacy mode: use shared rules
	if len(c.IPSet.Lists) == 0 {
		if listIndex != 0 {
			return
		}
		newSuffixes := make([]string, 0, len(c.Rules.DomainSuffix))
		for _, s := range c.Rules.DomainSuffix {
			if s != suffix {
				newSuffixes = append(newSuffixes, s)
			}
		}
		c.Rules.DomainSuffix = newSuffixes
		return
	}

	if listIndex < 0 || listIndex >= len(c.IPSet.Lists) {
		return
	}

	rules := &c.IPSet.Lists[listIndex].Rules
	newSuffixes := make([]string, 0, len(rules.DomainSuffix))
	for _, s := range rules.DomainSuffix {
		if s != suffix {
			newSuffixes = append(newSuffixes, s)
		}
	}
	rules.DomainSuffix = newSuffixes
}

// GetListRules returns the rules for a specific ipset list.
func (c *Config) GetListRules(listIndex int) *RulesConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Legacy mode: return shared rules
	if len(c.IPSet.Lists) == 0 {
		if listIndex != 0 {
			return nil
		}
		return &c.Rules
	}

	if listIndex < 0 || listIndex >= len(c.IPSet.Lists) {
		return nil
	}

	return &c.IPSet.Lists[listIndex].Rules
}

// GetNetLists returns the net list configurations.
func (c *Config) GetNetLists() []NetListConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IPSet.NetLists
}

// AddCIDRToNetList adds a CIDR to a specific net list.
func (c *Config) AddCIDRToNetList(listIndex int, cidr string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if listIndex < 0 || listIndex >= len(c.IPSet.NetLists) {
		return
	}

	list := &c.IPSet.NetLists[listIndex]
	for _, existing := range list.CIDRs {
		if existing == cidr {
			return
		}
	}
	list.CIDRs = append(list.CIDRs, cidr)
}

// RemoveCIDRFromNetList removes a CIDR from a specific net list.
func (c *Config) RemoveCIDRFromNetList(listIndex int, cidr string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if listIndex < 0 || listIndex >= len(c.IPSet.NetLists) {
		return
	}

	list := &c.IPSet.NetLists[listIndex]
	newCIDRs := make([]string, 0, len(list.CIDRs))
	for _, existing := range list.CIDRs {
		if existing != cidr {
			newCIDRs = append(newCIDRs, existing)
		}
	}
	list.CIDRs = newCIDRs
}

// GetNetListCIDRs returns the CIDRs for a specific net list.
func (c *Config) GetNetListCIDRs(listIndex int) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if listIndex < 0 || listIndex >= len(c.IPSet.NetLists) {
		return nil
	}
	return c.IPSet.NetLists[listIndex].CIDRs
}
