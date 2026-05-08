package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/crazytypewriter/dns-box/internal/blocklist"
	"github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	"github.com/crazytypewriter/dns-box/internal/ipset"
	"net"
)

type Handlers struct {
	cfg              *config.Config
	dnsCache         *cache.DNSCache
	domainCache      *cache.DomainCache
	blockList        *blocklist.BlockList
	listDomainCaches map[int]*cache.DomainCache
	ipSet            *ipset.IPSet
}

func NewHandlers(cfg *config.Config, dnsCache *cache.DNSCache, domainCache *cache.DomainCache, blockList *blocklist.BlockList, listDomainCaches map[int]*cache.DomainCache, ipSet *ipset.IPSet) *Handlers {
	return &Handlers{
		cfg:              cfg,
		dnsCache:         dnsCache,
		domainCache:      domainCache,
		blockList:        blockList,
		listDomainCaches: listDomainCaches,
		ipSet:            ipSet,
	}
}

func (h *Handlers) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/domains", h.handleDomains)
	mux.HandleFunc("/suffixes", h.handleSuffixes)
	mux.HandleFunc("/blocklist/urls", h.handleBlocklistURLs)
	mux.HandleFunc("/ipset/lists", h.handleIPSetLists)
	mux.HandleFunc("/ipset/net_lists", h.handleNetLists)
	mux.HandleFunc("/ipset/net/", h.handleNetList)
	mux.HandleFunc("/ipset/", h.handleIPSetList)
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
	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

// handleNetLists returns the list of all net list configurations.
func (h *Handlers) handleNetLists(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	lists := h.cfg.GetNetLists()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(lists); err != nil {
		http.Error(w, "failed to encode net lists", http.StatusInternalServerError)
	}
}

// handleNetList handles per-list CIDR management.
// Routes:
//
//	GET    /ipset/net/{name}/cidrs  - get CIDRs for net list
//	POST   /ipset/net/{name}/cidrs  - add CIDRs to net list
//	DELETE /ipset/net/{name}/cidrs  - remove CIDRs from net list
func (h *Handlers) handleNetList(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/ipset/net/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.Error(w, "Invalid path. Expected /ipset/net/{name}/cidrs", http.StatusBadRequest)
		return
	}

	listName := parts[0]
	resource := parts[1]
	if resource != "cidrs" {
		http.Error(w, "Invalid resource. Expected 'cidrs'", http.StatusBadRequest)
		return
	}

	listIndex := h.findNetListIndex(listName)
	if listIndex == -1 {
		http.Error(w, fmt.Sprintf("Net list '%s' not found", listName), http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.getNetListCIDRs(w, r, listIndex)
	case http.MethodPost:
		h.addNetListCIDRs(w, r, listIndex)
	case http.MethodDelete:
		h.removeNetListCIDRs(w, r, listIndex)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handlers) findNetListIndex(name string) int {
	lists := h.cfg.GetNetLists()
	for i, list := range lists {
		if list.Name == name {
			return i
		}
	}
	return -1
}

func (h *Handlers) getNetListCIDRs(w http.ResponseWriter, r *http.Request, listIndex int) {
	cidrs := h.cfg.GetNetListCIDRs(listIndex)
	if cidrs == nil {
		http.Error(w, "Net list not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cidrs); err != nil {
		http.Error(w, "failed to encode cidrs", http.StatusInternalServerError)
	}
}

func (h *Handlers) addNetListCIDRs(w http.ResponseWriter, r *http.Request, listIndex int) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	lists := h.cfg.GetNetLists()
	if listIndex < 0 || listIndex >= len(lists) {
		http.Error(w, "Net list not found", http.StatusNotFound)
		return
	}
	listCfg := lists[listIndex]
	timeout := listCfg.Timeout
	if timeout == 0 {
		timeout = 7200
	}

	lines := strings.Split(string(bodyBytes), "\n")
	for _, line := range lines {
		cidr := strings.TrimSpace(line)
		if cidr == "" {
			continue
		}
		if _, _, parseErr := net.ParseCIDR(cidr); parseErr != nil {
			w.Write([]byte(fmt.Sprintf("invalid cidr %s: %v\n", cidr, parseErr)))
			continue
		}
		if addErr := h.ipSet.AddElement(listCfg.Name, cidr, timeout); addErr != nil {
			w.Write([]byte(fmt.Sprintf("error adding cidr %s to ipset: %v\n", cidr, addErr)))
			continue
		}
		h.cfg.AddCIDRToNetList(listIndex, cidr)
	}

	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

func (h *Handlers) removeNetListCIDRs(w http.ResponseWriter, r *http.Request, listIndex int) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	lists := h.cfg.GetNetLists()
	if listIndex < 0 || listIndex >= len(lists) {
		http.Error(w, "Net list not found", http.StatusNotFound)
		return
	}
	listCfg := lists[listIndex]

	lines := strings.Split(string(bodyBytes), "\n")
	for _, line := range lines {
		cidr := strings.TrimSpace(line)
		if cidr == "" {
			continue
		}
		if delErr := h.ipSet.RemoveElement(listCfg.Name, cidr); delErr != nil {
			w.Write([]byte(fmt.Sprintf("error removing cidr %s from ipset: %v\n", cidr, delErr)))
			continue
		}
		h.cfg.RemoveCIDRFromNetList(listIndex, cidr)
	}

	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
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
	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
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
	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

func (h *Handlers) handleBlocklistURLs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getBlocklistURLs(w, r)
	case http.MethodPost:
		h.addBlocklistURL(w, r)
	case http.MethodDelete:
		h.removeBlocklistURL(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handlers) getBlocklistURLs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(h.cfg.BlockList.URLs); err != nil {
		http.Error(w, "failed to encode URLs", http.StatusInternalServerError)
	}
}

func (h *Handlers) addBlocklistURL(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	h.cfg.AddBlockListURL(payload.URL)
	if h.blockList != nil {
		h.blockList.UpdateURLs(h.cfg.BlockList.URLs)
		h.blockList.ForceRefresh()
	}

	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) removeBlocklistURL(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	h.cfg.RemoveBlockListURL(payload.URL)
	if h.blockList != nil {
		h.blockList.UpdateURLs(h.cfg.BlockList.URLs)
		h.blockList.ForceRefresh()
	}

	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

// handleIPSetLists returns the list of all ipset configurations.
func (h *Handlers) handleIPSetLists(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	lists := h.cfg.GetIPSetLists()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(lists); err != nil {
		http.Error(w, "failed to encode ipset lists", http.StatusInternalServerError)
	}
}

// handleIPSetList handles per-list domain/suffix management.
// Routes:
//
//	GET    /ipset/{name}/domains   - get domains for list
//	POST   /ipset/{name}/domains   - add domains to list
//	DELETE /ipset/{name}/domains   - remove domains from list
//	GET    /ipset/{name}/suffixes  - get suffixes for list
//	POST   /ipset/{name}/suffixes  - add suffixes to list
//	DELETE /ipset/{name}/suffixes  - remove suffixes from list
func (h *Handlers) handleIPSetList(w http.ResponseWriter, r *http.Request) {
	// Extract list name and resource type from path
	// Path format: /ipset/{name}/{domains|suffixes}
	path := strings.TrimPrefix(r.URL.Path, "/ipset/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.Error(w, "Invalid path. Expected /ipset/{name}/{domains|suffixes}", http.StatusBadRequest)
		return
	}

	listName := parts[0]
	resource := parts[1]
	if resource != "domains" && resource != "suffixes" {
		http.Error(w, "Invalid resource. Expected 'domains' or 'suffixes'", http.StatusBadRequest)
		return
	}

	// Find list index by name
	listIndex := h.findListIndex(listName)
	if listIndex == -1 {
		http.Error(w, fmt.Sprintf("List '%s' not found", listName), http.StatusNotFound)
		return
	}

	switch resource {
	case "domains":
		switch r.Method {
		case http.MethodGet:
			h.getListDomains(w, r, listIndex)
		case http.MethodPost:
			h.addListDomains(w, r, listIndex)
		case http.MethodDelete:
			h.removeListDomains(w, r, listIndex)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	case "suffixes":
		switch r.Method {
		case http.MethodGet:
			h.getListSuffixes(w, r, listIndex)
		case http.MethodPost:
			h.addListSuffixes(w, r, listIndex)
		case http.MethodDelete:
			h.removeListSuffixes(w, r, listIndex)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// findListIndex returns the index of the list with the given name, or -1 if not found.
func (h *Handlers) findListIndex(name string) int {
	lists := h.cfg.GetIPSetLists()
	for i, list := range lists {
		if list.Name == name {
			return i
		}
	}
	return -1
}

func (h *Handlers) getListDomains(w http.ResponseWriter, r *http.Request, listIndex int) {
	rules := h.cfg.GetListRules(listIndex)
	if rules == nil {
		http.Error(w, "List not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rules.Domains); err != nil {
		http.Error(w, "failed to encode domains", http.StatusInternalServerError)
	}
}

func (h *Handlers) addListDomains(w http.ResponseWriter, r *http.Request, listIndex int) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	lines := strings.Split(string(bodyBytes), "\n")
	listCache := h.listDomainCaches[listIndex]

	for _, line := range lines {
		domain := strings.TrimSpace(line)
		if domain != "" {
			// Add to list config
			h.cfg.AddDomainToList(listIndex, domain)
			// Add to per-list cache if available
			if listCache != nil && !listCache.Contains(domain) {
				listCache.Add(domain)
			}
		}
	}

	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

func (h *Handlers) removeListDomains(w http.ResponseWriter, r *http.Request, listIndex int) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	lines := strings.Split(string(bodyBytes), "\n")
	listCache := h.listDomainCaches[listIndex]

	for _, line := range lines {
		domain := strings.TrimSpace(line)
		if domain != "" {
			// Remove from list config
			h.cfg.RemoveDomainFromList(listIndex, domain)
			// Remove from per-list cache if available
			if listCache != nil {
				listCache.Remove(domain)
			}
		}
	}

	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

func (h *Handlers) getListSuffixes(w http.ResponseWriter, r *http.Request, listIndex int) {
	rules := h.cfg.GetListRules(listIndex)
	if rules == nil {
		http.Error(w, "List not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rules.DomainSuffix); err != nil {
		http.Error(w, "failed to encode suffixes", http.StatusInternalServerError)
	}
}

func (h *Handlers) addListSuffixes(w http.ResponseWriter, r *http.Request, listIndex int) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	lines := strings.Split(string(bodyBytes), "\n")
	listCache := h.listDomainCaches[listIndex]

	for _, line := range lines {
		suffix := strings.TrimSpace(line)
		if suffix != "" {
			// Add to list config
			h.cfg.AddSuffixToList(listIndex, suffix)
			// Add to per-list cache if available
			if listCache != nil && !listCache.ContainsSuffix(suffix) {
				listCache.AddSuffix(suffix)
			}
		}
	}

	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

func (h *Handlers) removeListSuffixes(w http.ResponseWriter, r *http.Request, listIndex int) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	lines := strings.Split(string(bodyBytes), "\n")
	listCache := h.listDomainCaches[listIndex]

	for _, line := range lines {
		suffix := strings.TrimSpace(line)
		if suffix != "" {
			// Remove from list config
			h.cfg.RemoveSuffixFromList(listIndex, suffix)
			// Remove from per-list cache if available
			if listCache != nil {
				listCache.Remove(suffix)
			}
		}
	}

	if err := h.cfg.SaveConfig(); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}
