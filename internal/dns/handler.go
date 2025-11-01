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
	blockList   *blocklist.BlockList // Новое поле
	log         *log.Logger
}

func NewDnsHandler(cfg *config.Config, dnsCache *C.DNSCache, domainCache *cache.DomainCache, ipSet *ipset.IPSet, blockList *blocklist.BlockList, log *log.Logger) *Handler {
	return &Handler{
		config:      cfg,
		dnsCache:    dnsCache,
		domainCache: domainCache,
		ipSet:       ipSet,
		blockList:   blockList, // Инициализация нового поля
		log:         log,
	}
}

func (h *Handler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

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

		answers := h.resolver(question.Name, question.Qtype)
		if h.shouldProcess(question.Name) {
			h.log.Debugf("Processing question: %s", question.Name)
			h.processAnswers(answers, question.Name)
		}
		msg.Answer = append(msg.Answer, answers...)
	}

	if err := w.WriteMsg(msg); err != nil {
		h.log.Errorf("Failed to write response: %v", err)
	}
}

func (h *Handler) shouldProcess(domain string) bool {
	domainWithoutDot := strings.TrimSuffix(domain, ".")
	h.log.Debugf("Check if domain exists in config or suffix config for: %s", domainWithoutDot)

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

	h.log.Debugf("Domain not found in domain cache: %s", domainWithoutDot)
	return false
}

func (h *Handler) processAnswers(answers []dns.RR, question string) {
	for _, rr := range answers {
		switch r := rr.(type) {
		case *dns.A:
			if h.config.IPSet.IPv4Name != "" {
				err := h.ipSet.AddElement(h.config.IPSet.IPv4Name, r.A.String(), r.Hdr.Ttl)
				if err != nil {
					h.log.Error(fmt.Sprintf("Error %v added address %s to ipset: %s", err.Error(), r.A.String(), h.config.IPSet.IPv4Name))
				}
				h.log.Debugf("Added IPv4 address %s with timeout %d for domain: %s, to ipset: %s", r.A.String(), r.Hdr.Ttl, question, h.config.IPSet.IPv4Name)
			}
		case *dns.AAAA:
			if h.config.IPSet.IPv6Name != "" {
				err := h.ipSet.AddElement(h.config.IPSet.IPv6Name, r.AAAA.String(), r.Hdr.Ttl)
				if err != nil {
					h.log.Error(fmt.Sprintf("Error %v when added address %s to ipset: %s", err.Error(), r.AAAA.String(), h.config.IPSet.IPv4Name))
				}
				h.log.Debugf("Added IPv6 address %s with timeout %d for domain: %s, to ipset: %s", r.AAAA.String(), r.Hdr.Ttl, question, h.config.IPSet.IPv6Name)
			}
		}
	}
}

func (h *Handler) resolver(domain string, qtype uint16) []dns.RR {
	if cached := h.getFromCache(domain, qtype); len(cached) > 0 {
		h.log.Tracef("Cache hit for %s (type %d)", domain, qtype)
		return cached
	}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(domain), qtype)
	m.RecursionDesired = true

	var response *dns.Msg
	var err error

	// Sort servers by protocol priority: DoH/DoT > TCP > UDP
	sortedServers := h.sortServers(h.config.DNS.UpstreamServers)

	for _, ns := range sortedServers {
		u, parseErr := url.Parse(ns)
		if parseErr != nil {
			// Fallback for simple IP addresses like 8.8.8.8:53
			if !strings.Contains(ns, "://") {
				u = &url.URL{Scheme: "udp", Host: ns}
				parseErr = nil
			} else {
				h.log.Warnf("Failed to parse upstream %s: %v", ns, parseErr)
				continue
			}
		}

		// Add port if missing for specific schemes
		host := u.Host
		// Add port if missing
		if _, _, err := net.SplitHostPort(host); err != nil {
			// Check if the host is a bare IPv6 address
			if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
				host = fmt.Sprintf("[%s]", host)
			}

			// Now add the port
			switch u.Scheme {
			case "tls", "dot":
				host = fmt.Sprintf("%s:%d", host, 853)
			case "https", "doh":
				host = fmt.Sprintf("%s:%d", host, 443)
			case "tcp", "udp":
				host = fmt.Sprintf("%s:%d", host, 53)
			}
		}

		h.log.Debugf("Querying upstream %s using %s", ns, u.Scheme)

		switch u.Scheme {
		case "https", "doh":
			response, err = h.exchangeDoH(m, ns)
		case "tls", "dot":
			client := &dns.Client{
				Net:     "tcp-tls",
				Timeout: time.Duration(h.config.DNS.Timeout) * time.Second,
				TLSConfig: &tls.Config{
					ServerName: u.Hostname(),
				},
			}
			response, _, err = client.Exchange(m, host)
		case "tcp":
			client := &dns.Client{
				Net:     "tcp",
				Timeout: time.Duration(h.config.DNS.Timeout) * time.Second,
			}
			response, _, err = client.Exchange(m, host)
		case "udp":
			client := &dns.Client{
				Net:     "udp",
				Timeout: time.Duration(h.config.DNS.Timeout) * time.Second,
			}
			response, _, err = client.Exchange(m, host)
		default: // Defaults to UDP
			client := &dns.Client{
				Net:     "udp",
				Timeout: time.Duration(h.config.DNS.Timeout) * time.Second,
			}
			response, _, err = client.Exchange(m, host)
		}

		if err == nil && response != nil && response.Rcode == dns.RcodeSuccess {
			h.log.Debugf("Successfully resolved %s via %s", domain, ns)
			break
		}
		h.log.Warnf("DNS error with %s (%s): %v", ns, u.Scheme, err)
		response = nil // Reset response to ensure we try the next server
	}

	if response == nil {
		h.log.Errorf("All DNS servers failed for %s", domain)
		return nil
	}

	if len(response.Answer) > 0 {
		h.cacheResponse(domain, qtype, response.Answer)
		h.log.Tracef("Cache set for %s (type %d)", domain, qtype)
	}

	return response.Answer
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
	var filtered []dns.RR
	for _, rr := range answers {
		if rr.Header().Rrtype == qtype {
			filtered = append(filtered, rr)
		}
	}
	if len(filtered) == 0 {
		return
	}

	ttl := filtered[0].Header().Ttl
	for _, rr := range filtered {
		if rr.Header().Ttl < ttl {
			ttl = rr.Header().Ttl
		}
	}

	h.dnsCache.Set(fmt.Sprintf("%s|%d", domain, qtype), filtered, ttl)
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

	client := &http.Client{
		Timeout: time.Duration(h.config.DNS.Timeout) * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform DoH request: %w", err)
	}
	defer resp.Body.Close()

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
