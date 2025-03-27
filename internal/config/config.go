package config

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
)

type ServerConfig struct {
	Address []string `json:"address"`
	Log     string   `json:"log"`
}

type DNSConfig struct {
	Nameservers []string `json:"nameservers"`
	Timeout     int      `json:"timeout"`
}

type IPSetConfig struct {
	IPv4Name string `json:"ipv4name"`
	IPv6Name string `json:"ipv6name"`
}

type RulesConfig struct {
	Domains      []string `json:"domain"`
	DomainSuffix []string `json:"domain_suffix"`
}

type Config struct {
	Server ServerConfig `json:"server"`
	DNS    DNSConfig    `json:"dns"`
	IPSet  IPSetConfig  `json:"ipset"`
	Rules  RulesConfig  `json:"rules"`
	mu     sync.RWMutex
	Path   string
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

func (c *Config) SaveWithUpdatedRules() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	file, err := os.Open(c.Path)
	if err != nil {
		return err
	}
	defer file.Close()

	var tempConfig struct {
		Server ServerConfig `json:"server"`
		DNS    DNSConfig    `json:"dns"`
		IPSet  IPSetConfig  `json:"ipset"`
	}

	if err := json.NewDecoder(file).Decode(&tempConfig); err != nil {
		return err
	}

	rules := RulesConfig{
		Domains:      c.Rules.Domains,
		DomainSuffix: c.Rules.DomainSuffix,
	}

	finalConfig := Config{
		Server: tempConfig.Server,
		DNS:    tempConfig.DNS,
		IPSet:  tempConfig.IPSet,
		Rules:  rules,
		Path:   c.Path,
	}

	outFile, err := os.Create(c.Path)
	if err != nil {
		return err
	}
	defer outFile.Close()

	encoder := json.NewEncoder(outFile)
	encoder.SetIndent("", "  ")
	return encoder.Encode(finalConfig)
}

func (c *Config) AddDomain(domain string) {
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
