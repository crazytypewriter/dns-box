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

	cfg.Path = filename // Сохраняем путь
	return &cfg, nil
}

func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.Path == "" {
		return ErrNoConfigPath
	}

	file, err := os.Create(c.Path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(c)
}

func (c *Config) SaveAs(filename string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(c)
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

func (c *Config) GetTimeout() int {
	return c.DNS.Timeout
}

var ErrNoConfigPath = errors.New("no config file path specified")
