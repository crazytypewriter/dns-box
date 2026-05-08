package dns

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crazytypewriter/dns-box/internal/blocklist"
	"github.com/crazytypewriter/dns-box/internal/cache"
	C "github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	"github.com/crazytypewriter/dns-box/internal/ipset"
	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
)

type Handler struct {
	config      *config.Config
	dnsCache    *C.DNSCache
	domainCache *cache.DomainCache
	ipSet       *ipset.IPSet
	blockList   *blocklist.BlockList
	log         *log.Logger
	httpClient  *http.Client // общий клиент для DoH с reuse соединений
	timeout     time.Duration

	// Per-list domain caches for routing IPs to correct ipsets
	listDomainCaches map[int]*cache.DomainCache
}

func NewDnsHandler(cfg *config.Config, dnsCache *C.DNSCache, domainCache *cache.DomainCache, ipSet *ipset.IPSet, blockList *blocklist.BlockList, listDomainCaches map[int]*cache.DomainCache, l *log.Logger) *Handler {
	timeout := time.Duration(cfg.DNS.Timeout) * time.Second
	if cfg.DNS.Timeout <= 0 {
		timeout = 5 * time.Second
	}

	h := &Handler{
		config:      cfg,
		dnsCache:    dnsCache,
		domainCache: domainCache,
		ipSet:       ipSet,
		blockList:   blockList,
		log:         l,
		timeout:     timeout,
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
		listDomainCaches: listDomainCaches,
	}

	return h
}

func (h *Handler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	if edns := r.IsEdns0(); edns != nil {
		msg.SetEdns0(4096, edns.Do())
	}

	for _, question := range r.Question {
		domain := strings.TrimSuffix(question.Name, ".")
		if h.blockList != nil && h.blockList.IsBlocked(domain) {
			h.log.Debugf("Blocked domain: %s", domain)
			rr, err := dns.NewRR(fmt.Sprintf("%s A 0.0.0.0", question.Name))
			if err == nil {
				msg.Answer = append(msg.Answer, rr)
			}
			continue
		}

		answers, rcode := h.resolver(question.Name, question.Qtype, 0)
		if h.shouldProcess(question.Name) {
			h.log.Debugf("Processing question: %s", question.Name)
			h.processAnswers(answers, question.Name)
		}
		msg.Answer = append(msg.Answer, answers...)
		if rcode != dns.RcodeSuccess {
			msg.Rcode = rcode
		}
	}

	if err := w.WriteMsg(msg); err != nil {
		h.log.Errorf("Failed to write response: %v", err)
	}
}

// normalizeTTL применяет политику ограничения TTL:
//   - TTL <= 0    → 3600 (защита от нулевых/отрицательных)
//   - TTL < 180   → 900  (минимум 15 минут для коротких TTL)
//   - иначе       → TTL
//   - затем clamp [300, 3600]
func normalizeTTL(ttl uint32) uint32 {
	var effective uint32

	if ttl <= 0 {
		effective = 3600
	} else if ttl < 180 {
		effective = 900
	} else {
		effective = ttl
	}

	// Жёсткие границы
	if effective < 300 {
		effective = 300
	}
	if effective > 3600 {
		effective = 3600
	}

	return effective
}

func (h *Handler) shouldProcess(domain string) bool {
	domainWithoutDot := strings.TrimSuffix(domain, ".")
	h.log.Debugf("Check if domain exists in config or suffix config for: %s", domainWithoutDot)

	// Check global cache (legacy mode)
	if h.domainCache.Contains(domainWithoutDot) {
		h.log.Debugf("Domain found in config, process: %s", domainWithoutDot)
		return true
	}

	parts := strings.Split(domainWithoutDot, ".")
	for i := 0; i <= len(parts)-2; i++ {
		suffix := "." + strings.Join(parts[i:], ".")
		h.log.Debugf("Check if suffix exists in config or suffix for: %s", suffix)
		if h.domainCache.ContainsSuffix(suffix) {
			h.log.Debugf("Domain matches suffix config, process: %s (suffix: %s)", domainWithoutDot, suffix)
			return true
		}
	}

	// Check per-list caches (new multi-list mode)
	for listIndex, listCache := range h.listDomainCaches {
		if listCache.Contains(domainWithoutDot) {
			h.log.Debugf("Domain found in list %d config, process: %s", listIndex, domainWithoutDot)
			return true
		}
		for i := 0; i <= len(parts)-2; i++ {
			suffix := "." + strings.Join(parts[i:], ".")
			if listCache.ContainsSuffix(suffix) {
				h.log.Debugf("Domain matches suffix in list %d config, process: %s (suffix: %s)", listIndex, domainWithoutDot, suffix)
				return true
			}
		}
	}

	h.log.Debugf("Domain not found in domain cache: %s", domainWithoutDot)
	return false
}

func (h *Handler) processAnswers(answers []dns.RR, question string) {
	ipSetLists := h.config.GetIPSetLists()

	for _, rr := range answers {
		switch r := rr.(type) {
		case *dns.A:
			// Find which lists this domain belongs to
			for i, listCfg := range ipSetLists {
				if h.isDomainInList(question, i) {
					ipv4Name := listCfg.Name
					effectiveTTL := normalizeTTL(r.Hdr.Ttl)
					err := h.ipSet.AddElement(ipv4Name, r.A.String(), effectiveTTL)
					if err != nil {
						h.log.Error(fmt.Sprintf("Error %v added address %s to ipset: %s", err.Error(), r.A.String(), ipv4Name))
					}
					h.log.Debugf("Added IPv4 address %s with original TTL %d, effective TTL %d for domain: %s, to ipset: %s", r.A.String(), r.Hdr.Ttl, effectiveTTL, question, ipv4Name)
				}
			}
		case *dns.AAAA:
			// Find which lists this domain belongs to
			for i, listCfg := range ipSetLists {
				if h.isDomainInList(question, i) && listCfg.EnableIPv6 {
					ipv6Name := listCfg.Name + "6"
					effectiveTTL := normalizeTTL(r.Hdr.Ttl)
					err := h.ipSet.AddElement(ipv6Name, r.AAAA.String(), effectiveTTL)
					if err != nil {
						h.log.Error(fmt.Sprintf("Error %v when added address %s to ipset: %s", err.Error(), r.AAAA.String(), ipv6Name))
					}
					h.log.Debugf("Added IPv6 address %s with original TTL %d, effective TTL %d for domain: %s, to ipset: %s", r.AAAA.String(), r.Hdr.Ttl, effectiveTTL, question, ipv6Name)
				}
			}
		}
	}
}

// isDomainInList checks if a domain matches the rules for a specific ipset list.
func (h *Handler) isDomainInList(domain string, listIndex int) bool {
	listCache, ok := h.listDomainCaches[listIndex]
	if !ok {
		return false
	}

	domainWithoutDot := strings.TrimSuffix(domain, ".")

	if listCache.Contains(domainWithoutDot) {
		return true
	}

	parts := strings.Split(domainWithoutDot, ".")
	for i := 0; i <= len(parts)-2; i++ {
		suffix := "." + strings.Join(parts[i:], ".")
		if listCache.ContainsSuffix(suffix) {
			return true
		}
	}

	return false
}

func (h *Handler) resolver(domain string, qtype uint16, depth int) ([]dns.RR, int) {
	if depth > 10 {
		h.log.Warnf("CNAME loop detected for %s", domain)
		return nil, dns.RcodeServerFailure
	}

	cached := h.getFromCache(domain, qtype)
	if cached != nil {
		h.log.Tracef("Cache hit for %s (type %d), returning %d records", domain, qtype, len(cached))
		return cached, dns.RcodeSuccess
	}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(domain), qtype)
	m.RecursionDesired = true
	m.SetEdns0(1232, true) // Enable EDNS0 for upstream queries

	var response *dns.Msg
	var err error

	sortedServers := h.sortServers(h.config.DNS.UpstreamServers)

	for _, ns := range sortedServers {
		u, parseErr := url.Parse(ns)
		if parseErr != nil {
			if !strings.Contains(ns, "://") {
				u = &url.URL{Scheme: "udp", Host: ns}
			} else {
				h.log.Warnf("Failed to parse upstream %s: %v", ns, parseErr)
				continue
			}
		}

		host := u.Host
		if _, _, err := net.SplitHostPort(host); err != nil {
			// If host is a valid IP address, it doesn't have a port
			ip := net.ParseIP(host)
			if ip != nil {
				if ip.To4() == nil { // It's an IPv6 address
					host = fmt.Sprintf("[%s]", host)
				}
			}
			// Now, add the port based on the scheme
			switch u.Scheme {
			case "tls", "dot":
				host = fmt.Sprintf("%s:%d", host, 853)
			case "https", "doh":
				host = fmt.Sprintf("%s:%d", host, 443)
			case "tcp", "udp":
				host = fmt.Sprintf("%s:%d", host, 53)
			}
		}

		h.log.Debugf("Querying upstream %s using %s for %s", ns, u.Scheme, domain)

		switch u.Scheme {
		case "https", "doh":
			response, err = h.exchangeDoH(m, ns)
		case "tls", "dot":
			client := &dns.Client{Net: "tcp-tls", Timeout: h.timeout, TLSConfig: &tls.Config{ServerName: u.Hostname()}}
			response, _, err = client.Exchange(m, host)
		case "tcp":
			client := &dns.Client{Net: "tcp", Timeout: h.timeout}
			response, _, err = client.Exchange(m, host)
		default: // udp
			client := &dns.Client{Net: "udp", Timeout: h.timeout}
			response, _, err = client.Exchange(m, host)
		}

		if err == nil && response != nil {
			if response.Rcode == dns.RcodeSuccess || response.Rcode == dns.RcodeNameError {
				h.log.Debugf("Received a valid response for %s via %s with Rcode: %s", domain, ns, dns.RcodeToString[response.Rcode])
				break
			}
		}
		rcode := "N/A"
		if response != nil {
			rcode = dns.RcodeToString[response.Rcode]
		}
		h.log.Warnf("DNS error with %s (%s): %v, Rcode: %s", ns, u.Scheme, err, rcode)
		response = nil
	}

	if response == nil {
		h.log.Errorf("All DNS servers failed for %s", domain)
		return nil, dns.RcodeServerFailure
	}

	if response.Rcode == dns.RcodeNameError {
		ttl := uint32(h.config.DNS.Timeout) // Default TTL
		if ttl == 0 {
			ttl = 300
		}
		if len(response.Ns) > 0 {
			if soa, ok := response.Ns[0].(*dns.SOA); ok && soa != nil {
				ttl = soa.Minttl
			}
		}
		effectiveTTL := normalizeTTL(ttl)
		h.dnsCache.Set(fmt.Sprintf("%s|%d", domain, qtype), []dns.RR{}, effectiveTTL)
		h.log.Tracef("Negative cache set for %s (type %d) with TTL %d (effective %d)", domain, qtype, ttl, effectiveTTL)
		return nil, dns.RcodeNameError
	}

	finalAnswers := make([]dns.RR, 0)
	cnameChain := make([]dns.RR, 0)

	for _, answer := range response.Answer {
		if cname, ok := answer.(*dns.CNAME); ok {
			h.log.Debugf("Found CNAME for %s: %s", domain, cname.Target)
			cnameChain = append(cnameChain, answer)
			recursiveAnswers, rcode := h.resolver(cname.Target, qtype, depth+1)
			if rcode == dns.RcodeSuccess {
				finalAnswers = append(cnameChain, recursiveAnswers...)
				h.cacheResponse(domain, qtype, finalAnswers)
				return finalAnswers, dns.RcodeSuccess
			}
			// propagate error/NXDOMAIN but still return CNAMEs we found
			return append(cnameChain, recursiveAnswers...), rcode
		} else {
			finalAnswers = append(finalAnswers, answer)
		}
	}

	if len(finalAnswers) > 0 {
		h.cacheResponse(domain, qtype, finalAnswers)
		h.log.Tracef("Cache set for %s (type %d)", domain, qtype)
	}

	return finalAnswers, dns.RcodeSuccess
}

func (h *Handler) getFromCache(domain string, qtype uint16) []dns.RR {
	h.log.Tracef("Cache getFromCache for %s (type %d)", domain, qtype)
	result := h.dnsCache.Get(fmt.Sprintf("%s|%d", domain, qtype))
	if len(result) > 0 {
		h.log.Tracef("Cache hit for %s (type %d)", domain, qtype)
		for _, rr := range result {
			h.log.Tracef("Cached RR: %s", rr.String())
		}
	} else {
		h.log.Tracef("Cache miss for %s (type %d)", domain, qtype)
	}
	return result
}

func (h *Handler) cacheResponse(domain string, qtype uint16, answers []dns.RR) {
	h.log.Tracef("Cache set for %s (type %d)", domain, qtype)
	if len(answers) == 0 {
		return
	}

	ttl := answers[0].Header().Ttl
	for _, rr := range answers {
		if rr.Header().Ttl < ttl {
			ttl = rr.Header().Ttl
		}
	}

	effectiveTTL := normalizeTTL(ttl)
	h.dnsCache.Set(fmt.Sprintf("%s|%d", domain, qtype), answers, effectiveTTL)
	h.log.Tracef("Cache set for %s (type %d) with TTL %d (effective %d)", domain, qtype, ttl, effectiveTTL)
}

func (h *Handler) exchangeDoH(m *dns.Msg, endpoint string) (*dns.Msg, error) {
	pack, err := m.Pack()
	if err != nil {
		return nil, fmt.Errorf("failed to pack DNS message: %w", err)
	}

	// Ensure the endpoint is a valid URL
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = "https://" + endpoint
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid DoH endpoint URL: %w", err)
	}

	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(pack))
	if err != nil {
		return nil, fmt.Errorf("failed to create DoH request: %w", err)
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform DoH request: %w", err)
	}
	defer func() {
		if resp.Body != nil {
			resp.Body.Close()
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH request failed with status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read DoH response body: %w", err)
	}

	responseMsg := new(dns.Msg)
	err = responseMsg.Unpack(body)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack DoH response: %w", err)
	}

	return responseMsg, nil
}

func (h *Handler) sortServers(servers []string) []string {
	priority := map[string]int{
		"https": 0, "doh": 0,
		"tls": 1, "dot": 1,
		"tcp": 2,
		"udp": 3,
	}

	sorted := make([]string, len(servers))
	copy(sorted, servers)

	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			schemeI := h.getScheme(sorted[i])
			schemeJ := h.getScheme(sorted[j])

			priorityI, okI := priority[schemeI]
			if !okI {
				priorityI = 99 // low priority for unknown
			}
			priorityJ, okJ := priority[schemeJ]
			if !okJ {
				priorityJ = 99 // low priority for unknown
			}

			if priorityI > priorityJ {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	h.log.Debugf("Sorted upstream servers: %v", sorted)
	return sorted
}

func (h *Handler) getScheme(server string) string {
	if strings.HasPrefix(server, "https://") {
		return "https"
	}
	if strings.HasPrefix(server, "tls://") {
		return "tls"
	}
	if strings.HasPrefix(server, "tcp://") {
		return "tcp"
	}
	if strings.HasPrefix(server, "udp://") {
		return "udp"
	}
	// Default to udp if no scheme
	if !strings.Contains(server, "://") {
		return "udp"
	}
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	return u.Scheme
}
