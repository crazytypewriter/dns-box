package config

import (
	"os"
	"testing"
)

func TestConfig(t *testing.T) {
	configContent := `{
		"server": {
			"address": ["127.0.0.1:53"],
			"log": "debug"
		},
		"dns": {
			"upstream_servers": ["8.8.8.8"],
			"timeout": 5
		},
		"ipset": {
			"ipv4name": "test-v4",
			"ipv6name": "test-v6"
		},
		"rules": {
			"domain": ["example.com"],
			"domain_suffix": [".test.com"]
		}
	}`

	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write([]byte(configContent)); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if len(cfg.Server.Address) != 1 || cfg.Server.Address[0] != "127.0.0.1:53" {
		t.Error("Invalid server address")
	}
}

func TestSaveConfigPreservesIPSetLists(t *testing.T) {
	configContent := `{
  "server": {"address": ["127.0.0.1:53"], "log": "debug"},
  "dns": {"upstream_servers": ["8.8.8.8"], "timeout": 5},
  "ipset": {
    "lists": [
      {
        "name": "test-list",
        "enable_ipv6": false,
        "timeout": 3600,
        "rules": {
          "domain": ["example.com"],
          "domain_suffix": [".test.com"]
        }
      }
    ]
  },
  "blocklist": {"enabled": false, "urls": [], "refresh_hours": 24},
  "github_backup": {"enabled": false}
}`

	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write([]byte(configContent)); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Добавляем домен в список через API
	cfg.AddDomainToList(0, "newdomain.com")

	if err := cfg.SaveConfig(); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// Перезагружаем и проверяем, что изменения сохранились
	cfg2, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to reload config: %v", err)
	}

	lists := cfg2.GetIPSetLists()
	if len(lists) != 1 {
		t.Fatalf("Expected 1 ipset list, got %d", len(lists))
	}

	rules := lists[0].Rules
	if len(rules.Domains) != 2 {
		t.Fatalf("Expected 2 domains, got %d", len(rules.Domains))
	}

	found := false
	for _, d := range rules.Domains {
		if d == "newdomain.com" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Expected 'newdomain.com' in domains, got %v", rules.Domains)
	}
}

func TestSaveConfigAtomicWrite(t *testing.T) {
	configContent := `{
  "server": {"address": ["127.0.0.1:53"], "log": "debug"},
  "dns": {"upstream_servers": ["8.8.8.8"], "timeout": 5},
  "ipset": {"lists": []},
  "blocklist": {"enabled": false, "urls": [], "refresh_hours": 24},
  "github_backup": {"enabled": false}
}`

	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write([]byte(configContent)); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	cfg.AddDomain("atomic.test")
	if err := cfg.SaveConfig(); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// Временный файл не должен остаться после успешного сохранения
	tmpPath := tmpFile.Name() + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("Temporary file %s should not exist after successful save", tmpPath)
	}

	// Основной файл должен быть валидным
	cfg2, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	if len(cfg2.Rules.Domains) != 1 || cfg2.Rules.Domains[0] != "atomic.test" {
		t.Errorf("Expected domain 'atomic.test', got %v", cfg2.Rules.Domains)
	}
}

func TestSaveConfigFallbackOnCorruptFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("this is not json"); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg := &Config{
		Path: tmpFile.Name(),
		Server: ServerConfig{
			Address: []string{"127.0.0.1:53"},
			Log:     "info",
		},
		DNS: DNSConfig{
			UpstreamServers: []string{"1.1.1.1"},
			Timeout:         5,
		},
		Rules: RulesConfig{
			Domains:      []string{"fallback.com"},
			DomainSuffix: []string{".fallback.com"},
		},
		BlockList:    BlockListConfig{Enabled: false},
		GithubBackup: GithubConfig{Enabled: false},
	}

	if err := cfg.SaveConfig(); err != nil {
		t.Fatalf("SaveConfig should not fail on corrupt file, got: %v", err)
	}

	cfg2, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("Reload after save failed: %v", err)
	}

	if len(cfg2.Rules.Domains) != 1 || cfg2.Rules.Domains[0] != "fallback.com" {
		t.Errorf("Expected fallback domain, got %v", cfg2.Rules.Domains)
	}
	if len(cfg2.Server.Address) != 1 || cfg2.Server.Address[0] != "127.0.0.1:53" {
		t.Errorf("Expected server address from memory, got %v", cfg2.Server.Address)
	}
}

func TestSaveConfigNoPath(t *testing.T) {
	cfg := &Config{}
	if err := cfg.SaveConfig(); err != ErrNoConfigPath {
		t.Errorf("Expected ErrNoConfigPath, got %v", err)
	}
}

func TestGithubConfigGetToken(t *testing.T) {
	// Сохраняем и очищаем env после теста
	orig := os.Getenv(GitHubTokenEnv)
	if orig != "" {
		os.Unsetenv(GitHubTokenEnv)
		defer os.Setenv(GitHubTokenEnv, orig)
	} else {
		defer os.Unsetenv(GitHubTokenEnv)
	}

	cfg := GithubConfig{Token: "config-token"}

	// Без env должен вернуться токен из конфига
	if got := cfg.GetToken(); got != "config-token" {
		t.Errorf("Expected 'config-token', got %q", got)
	}

	// С env переменной приоритет у неё
	os.Setenv(GitHubTokenEnv, "env-token")
	if got := cfg.GetToken(); got != "env-token" {
		t.Errorf("Expected 'env-token', got %q", got)
	}
}

func TestNetListsConfig(t *testing.T) {
	configContent := `{
  "server": {"address": ["127.0.0.1:53"], "log": "debug"},
  "dns": {"upstream_servers": ["8.8.8.8"], "timeout": 5},
  "ipset": {
    "net_lists": [
      {
        "name": "test-net",
        "enable_ipv6": true,
        "timeout": 3600,
        "asn": "12345",
        "cidr": ["192.168.1.0/24", "10.0.0.0/8"]
      }
    ]
  },
  "blocklist": {"enabled": false, "urls": [], "refresh_hours": 24},
  "github_backup": {"enabled": false}
}`

	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write([]byte(configContent)); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	netLists := cfg.GetNetLists()
	if len(netLists) != 1 {
		t.Fatalf("Expected 1 net list, got %d", len(netLists))
	}

	list := netLists[0]
	if list.Name != "test-net" {
		t.Errorf("Expected name 'test-net', got %s", list.Name)
	}
	if list.ASN != "12345" {
		t.Errorf("Expected ASN '12345', got %s", list.ASN)
	}
	if len(list.CIDRs) != 2 {
		t.Fatalf("Expected 2 CIDRs, got %d", len(list.CIDRs))
	}

	// Добавляем CIDR
	cfg.AddCIDRToNetList(0, "172.16.0.0/12")
	if len(cfg.GetNetListCIDRs(0)) != 3 {
		t.Errorf("Expected 3 CIDRs after add, got %d", len(cfg.GetNetListCIDRs(0)))
	}

	// Повторное добавление того же CIDR не дублирует
	cfg.AddCIDRToNetList(0, "172.16.0.0/12")
	if len(cfg.GetNetListCIDRs(0)) != 3 {
		t.Errorf("Expected 3 CIDRs after duplicate add, got %d", len(cfg.GetNetListCIDRs(0)))
	}

	// Удаляем CIDR
	cfg.RemoveCIDRFromNetList(0, "10.0.0.0/8")
	if len(cfg.GetNetListCIDRs(0)) != 2 {
		t.Errorf("Expected 2 CIDRs after remove, got %d", len(cfg.GetNetListCIDRs(0)))
	}

	// Сохраняем и перезагружаем
	if err := cfg.SaveConfig(); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	cfg2, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to reload config: %v", err)
	}

	reloaded := cfg2.GetNetLists()
	if len(reloaded) != 1 {
		t.Fatalf("Expected 1 net list after reload, got %d", len(reloaded))
	}
	if len(reloaded[0].CIDRs) != 2 {
		t.Errorf("Expected 2 CIDRs after reload, got %d", len(reloaded[0].CIDRs))
	}
}
