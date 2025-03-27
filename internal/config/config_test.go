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
			"nameservers": ["8.8.8.8"],
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
