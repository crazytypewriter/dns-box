package api

import (
	"dns-box/internal/cache"
	"dns-box/internal/config"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Handlers struct {
	cfg         *config.Config
	dnsCache    *cache.DNSCache
	domainCache *cache.DomainCache
}

func NewHandlers(cfg *config.Config, dnsCache *cache.DNSCache, domainCache *cache.DomainCache) *Handlers {
	return &Handlers{
		cfg:         cfg,
		dnsCache:    dnsCache,
		domainCache: domainCache,
	}
}

func (h *Handlers) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/domains", h.handleDomains)
	mux.HandleFunc("/suffixes", h.handleSuffixes)
	return mux
}

func (h *Handlers) handleDomains(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getDomains(w, r)
	case http.MethodPost:
		h.addDomains(w, r)
	case http.MethodDelete:
		h.removeDomains(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handlers) handleSuffixes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getSuffixes(w, r)
	case http.MethodPost:
		h.addSuffixes(w, r)
	case http.MethodDelete:
		h.removeSuffixes(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handlers) getDomains(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(h.cfg.Rules.Domains); err != nil {
		http.Error(w, "failed to encode domains", http.StatusInternalServerError)
	}
}

func (h *Handlers) addDomains(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	lines := strings.Split(string(bodyBytes), "\n")
	for _, line := range lines {
		domain := strings.TrimSpace(line)
		if domain != "" {
			if h.domainCache.Contains(domain) {
				w.Write([]byte(fmt.Sprintf("domain %s exist\n", domain)))
				continue
			}
			h.domainCache.Add(domain)
			h.cfg.AddDomain(domain)
		}
	}
	w.Write([]byte("ok"))
}

func (h *Handlers) removeDomains(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	lines := strings.Split(string(bodyBytes), "\n")
	for _, line := range lines {
		domain := strings.TrimSpace(line)
		if domain != "" {
			if !h.domainCache.Contains(domain) {
				w.Write([]byte(fmt.Sprintf("domain %s not found\n", domain)))
				continue
			}
			h.domainCache.Remove(domain)
			h.cfg.RemoveDomain(domain)
		}
	}
	w.Write([]byte("ok"))
}

func (h *Handlers) getSuffixes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(h.cfg.Rules.DomainSuffix); err != nil {
		http.Error(w, "failed to encode suffixes", http.StatusInternalServerError)
	}
}

func (h *Handlers) addSuffixes(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	lines := strings.Split(string(bodyBytes), "\n")
	for _, line := range lines {
		suffix := strings.TrimSpace(line)
		if suffix != "" {
			if h.domainCache.ContainsSuffix(suffix) {
				w.Write([]byte(fmt.Sprintf("suffix %s exist\n", suffix)))
				continue
			}
			h.domainCache.AddSuffix(suffix)
			h.cfg.AddSuffix(suffix)
		}
	}
	w.Write([]byte("ok"))
}

func (h *Handlers) removeSuffixes(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	lines := strings.Split(string(bodyBytes), "\n")
	for _, line := range lines {
		suffix := strings.TrimSpace(line)
		if suffix != "" {
			if !h.domainCache.ContainsSuffix(suffix) {
				w.Write([]byte(fmt.Sprintf("suffix %s not found\n", suffix)))
				continue
			}
			h.domainCache.Remove(suffix)
			h.cfg.RemoveSuffix(suffix)
		}
	}
	w.Write([]byte("ok"))
}
