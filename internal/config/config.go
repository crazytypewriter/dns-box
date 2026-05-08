package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
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
}

type IPSetListConfig struct {
	Name       string      `json:"name"`
	EnableIPv6 bool        `json:"enable_ipv6"`
	Timeout    uint32      `json:"timeout"` // in seconds, 0 means use default
	Rules      RulesConfig `json:"rules"`
}

type IPSetConfig struct {
	IPv4Name string            `json:"ipv4name"` // deprecated, kept for backward compatibility
	IPv6Name string            `json:"ipv6name"` // deprecated, kept for backward compatibility
	Lists    []IPSetListConfig `json:"lists"`    // new multi-list config
	NetLists []NetListConfig   `json:"net_lists"` // static CIDR net lists
}

type NetListConfig struct {
	Name       string   `json:"name"`
	EnableIPv6 bool     `json:"enable_ipv6"`
	Timeout    uint32   `json:"timeout"`
	ASN        string   `json:"asn,omitempty"`
	CIDRs      []string `json:"cidr"`
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

type Config struct {
	Server       ServerConfig    `json:"server"`
	DNS          DNSConfig       `json:"dns"`
	IPSet        IPSetConfig     `json:"ipset"`
	Rules        RulesConfig     `json:"rules"`
	BlockList    BlockListConfig `json:"blocklist"`
	GithubBackup GithubConfig    `json:"github_backup"`
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
	IPSetLists   []IPSetListConfig `json:"ipset_lists,omitempty"` // new multi-list format
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
	return &cfg, nil
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

	return &hostsConfig, nil
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

	file, err := os.Open(c.Path)
	if err == nil {
		var tempConfig struct {
			Server       ServerConfig    `json:"server"`
			DNS          DNSConfig       `json:"dns"`
			IPSet        IPSetConfig     `json:"ipset"`
			GithubBackup GithubConfig    `json:"github_backup"`
			BlockList    BlockListConfig `json:"blocklist"`
			Rules        RulesConfig     `json:"rules"`
		}
		decodeErr := json.NewDecoder(file).Decode(&tempConfig)
		file.Close()
		if decodeErr == nil {
			staticServer = tempConfig.Server
			staticDNS = tempConfig.DNS
			staticGithubBackup = tempConfig.GithubBackup
		} else {
			log.Printf("[config] Warning: failed to decode existing config (%v), using in-memory static values", decodeErr)
		}
	} else if !os.IsNotExist(err) {
		c.mu.Unlock()
		return err
	}

	finalConfig := struct {
		Server       ServerConfig    `json:"server"`
		DNS          DNSConfig       `json:"dns"`
		IPSet        IPSetConfig     `json:"ipset"`
		GithubBackup GithubConfig    `json:"github_backup"`
		Rules        RulesConfig     `json:"rules"`
		BlockList    BlockListConfig `json:"blocklist"`
	}{
		Server:       staticServer,
		DNS:          staticDNS,
		IPSet:        c.IPSet,
		GithubBackup: staticGithubBackup,
		Rules:        c.Rules,
		BlockList:    c.BlockList,
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
	}
	c.mu.Unlock()

	if needGitHubSave {
		log.Printf("[config] Saving rules to GitHub: owner=%s, repo=%s, path=%s, branch=%s",
			githubOwner, githubRepo, githubPath, githubBranch)

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

// GetIPSetLists returns the list of ipset configurations.
// If Lists is empty, it falls back to the legacy IPv4Name/IPv6Name fields.
func (c *Config) GetIPSetLists() []IPSetListConfig {
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
