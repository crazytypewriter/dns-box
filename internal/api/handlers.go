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

func (h *Handlers) getDomains(w http.ResponseWriter, r *http.Request) {
	domains := h.cfg.Rules.Domains
	json.NewEncoder(w).Encode(domains)
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
	domains := h.cfg.Rules.Domains
	json.NewEncoder(w).Encode(domains)
}
