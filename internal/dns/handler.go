package dns

import (
	"dns-box/internal/cache"
	C "dns-box/internal/cache"
	"dns-box/internal/config"
	"dns-box/internal/ipset"
	"fmt"
	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"strings"
	"time"
)

type Handler struct {
	config      *config.Config
	dnsCache    *C.DNSCache
	domainCache *cache.DomainCache
	ipSet       *ipset.IPSet
	log         *log.Logger
}

func NewDnsHandler(cfg *config.Config, dnsCache *C.DNSCache, domainCache *cache.DomainCache, ipSet *ipset.IPSet, log *log.Logger) *Handler {
	return &Handler{
		config:      cfg,
		dnsCache:    dnsCache,
		domainCache: domainCache,
		ipSet:       ipSet,
		log:         log,
	}
}

func (h *Handler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	for _, question := range r.Question {
		answers := h.resolver(question.Name, question.Qtype)
		if h.shouldProcess(question.Name) {
			h.processAnswers(answers)
		}
		msg.Answer = append(msg.Answer, answers...)
	}

	if err := w.WriteMsg(msg); err != nil {
		h.log.Errorf("Failed to write response: %v", err)
	}
}

func (h *Handler) shouldProcess(domain string) bool {
	domainWithoutDot := strings.TrimSuffix(domain, ".")
	h.log.Debugf("Should process domain: %s", domainWithoutDot)

	if h.domainCache.Contains(domainWithoutDot) {
		return true
	}

	parts := strings.Split(domainWithoutDot, ".")
	if len(parts) > 2 {
		suffix := "." + strings.Join(parts[len(parts)-2:], ".")
		if h.domainCache.ContainsSuffix(suffix) {
			return true
		}
	}

	return false
}

func (h *Handler) processAnswers(answers []dns.RR) {
	for _, rr := range answers {
		switch r := rr.(type) {
		case *dns.A:
			if h.config.IPSet.IPv4Name != "" {
				h.ipSet.AddElement(h.config.IPSet.IPv4Name, r.A.String(), r.Hdr.Ttl)
			}
		case *dns.AAAA:
			if h.config.IPSet.IPv6Name != "" {
				h.ipSet.AddElement(h.config.IPSet.IPv6Name, r.AAAA.String(), r.Hdr.Ttl)
			}
		}
	}
}

func (h *Handler) resolver(domain string, qtype uint16) []dns.RR {
	if cached := h.getFromCache(domain, qtype); len(cached) > 0 {
		h.log.Debugf("Cache hit for %s (type %d)", domain, qtype)
		return cached
	}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(domain), qtype)
	m.RecursionDesired = true

	// 3. Настраиваем клиент с таймаутом из конфига
	client := &dns.Client{
		Timeout: time.Duration(h.config.DNS.Timeout) * time.Second,
	}

	var response *dns.Msg
	var err error

	for _, ns := range h.config.DNS.Nameservers {
		response, _, err = client.Exchange(m, ns)
		if err == nil && response != nil {
			break
		}
		h.log.Warnf("DNS error with %s: %v", ns, err)
	}

	if err != nil || response == nil {
		h.log.Errorf("All DNS servers failed for %s: %v", domain, err)
		return nil
	}

	if len(response.Answer) > 0 {
		h.cacheResponse(domain, qtype, response.Answer)
		h.log.Debugf("Cache set for %s (type %d)", domain, qtype)
	}

	return response.Answer
}

func (h *Handler) getFromCache(domain string, qtype uint16) []dns.RR {
	h.log.Debugf("Cache getFromCache for %s (type %d)", domain, qtype)
	result := h.dnsCache.Get(fmt.Sprintf("%s|%d", domain, qtype))
	if len(result) > 0 {
		h.log.Debugf("Cache hit for %s (type %d)", domain, qtype)
		for _, rr := range result {
			h.log.Debugf("Cached RR: %s", rr.String())
		}
	} else {
		h.log.Debugf("Cache miss for %s (type %d)", domain, qtype)
	}
	return result
}

func (h *Handler) cacheResponse(domain string, qtype uint16, answers []dns.RR) {
	h.log.Debugf("Cache set for %s (type %d)", domain, qtype)
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
