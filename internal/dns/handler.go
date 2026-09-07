package dns

import (
	"bytes"
	"context"
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
	"github.com/crazytypewriter/dns-box/internal/ipsetstate"
	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

type Handler struct {
	config      *config.Config
	dnsCache    *C.DNSCache
	domainCache *cache.DomainCache
	ipSet       ipset.Manager
	blockList   *blocklist.BlockList
	log         *log.Logger
	httpClient  *http.Client // общий клиент для DoH с reuse соединений
	timeout     time.Duration

	// Rate limiting и локальные hosts-переопределения
	rateLimiter *RateLimiter
	hosts       *HostsResolver

	// Локальная зона (DHCP-имена, PTR) и ACL по подсетям
	local       *LocalZone
	allowedNets []*net.IPNet

	// Зеркало ipset-записей для восстановления после ребута (nil = выключено)
	stateStore *ipsetstate.Store

	// Префетч: дедупликация одновременных обновлений + семафор
	prefetchGroup singleflight.Group
	prefetchSem   chan struct{}

	// Per-list domain caches for routing IPs to correct ipsets
	listDomainCaches map[int]*cache.DomainCache
}

func NewDnsHandler(cfg *config.Config, dnsCache *C.DNSCache, domainCache *cache.DomainCache, ipSet ipset.Manager, blockList *blocklist.BlockList, listDomainCaches map[int]*cache.DomainCache, stateStore *ipsetstate.Store, local *LocalZone, l *log.Logger) *Handler {
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
		stateStore:       stateStore,
		prefetchSem:      make(chan struct{}, 8), // не устраиваем шторм на слабом роутере
	}

	h.local = local
	for _, subnet := range cfg.Server.AllowedSubnets {
		_, ipNet, err := net.ParseCIDR(subnet)
		if err != nil {
			l.Warnf("Invalid allowed subnet %q: %v", subnet, err)
			continue
		}
		h.allowedNets = append(h.allowedNets, ipNet)
	}
	if len(h.allowedNets) == 0 {
		for _, addr := range cfg.Server.Address {
			host, _, err := net.SplitHostPort(addr)
			if err == nil && (host == "" || host == "::" || host == "0.0.0.0") {
				l.Warnf("Wildcard bind %s with no server.allowed_subnets: DNS will answer queries from any source, including the Internet", addr)
			}
		}
	}

	h.rateLimiter = NewRateLimiter(cfg.DNS.RateLimit)
	if cfg.DNS.HostsFile != "" {
		h.hosts = NewHostsResolver(cfg.DNS.HostsFile, l)
		if err := h.hosts.Reload(); err != nil {
			l.Warnf("Failed to load hosts file %s: %v", cfg.DNS.HostsFile, err)
		}
	}

	return h
}

func (h *Handler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	// ACL: запросы из неразрешённых подсетей отклоняются сразу.
	// Пустой список подсетей = разрешено всем (с оговорками, см. варнинг выше).
	if !h.clientAllowed(w.RemoteAddr()) {
		h.log.Debugf("DNS query refused by ACL from %s", w.RemoteAddr().String())
		msg := new(dns.Msg)
		msg.SetReply(r)
		msg.Rcode = dns.RcodeRefused
		_ = w.WriteMsg(msg)
		return
	}

	// Rate limiting: при превышении лимита отвечаем REFUSED
	if !h.rateLimiter.Allow(w.RemoteAddr().String()) {
		h.log.Debugf("Rate limit exceeded for %s", w.RemoteAddr().String())
		msg := new(dns.Msg)
		msg.SetReply(r)
		msg.Rcode = dns.RcodeRefused
		_ = w.WriteMsg(msg)
		return
	}

	msg := new(dns.Msg)
	msg.SetReply(r)
	// AA не ставим: форвардер не авторитетен за зону.
	// EDNS echoed для совместимости буфера, но DO всегда 0 — DNSSEC
	// не валидируется, проксирование DO только раздувает ответы.
	if r.IsEdns0() != nil {
		msg.SetEdns0(4096, false)
	}

	for _, question := range r.Question {
		// Локальные переопределения из hosts-файла имеют наивысший приоритет
		if ip := h.hosts.Lookup(question.Name, question.Qtype); ip != nil {
			h.log.Debugf("Hosts file hit for %s: %s", question.Name, ip)
			var rr dns.RR
			if ip.To4() == nil {
				aaaa := new(dns.AAAA)
				aaaa.Hdr = dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600}
				aaaa.AAAA = ip
				rr = aaaa
			} else {
				a := new(dns.A)
				a.Hdr = dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600}
				a.A = ip
				rr = a
			}
			msg.Answer = append(msg.Answer, rr)
			continue
		}

		// Локальная зона: DHCP-имена и PTR локальных адресов
		if h.local != nil {
			if ip := h.local.LookupForward(question.Name, question.Qtype); ip != nil {
				h.log.Debugf("Local zone hit for %s: %s", question.Name, ip)
				var rr dns.RR
				if ip.To4() == nil {
					aaaa := new(dns.AAAA)
					aaaa.Hdr = dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60}
					aaaa.AAAA = ip
					rr = aaaa
				} else {
					a := new(dns.A)
					a.Hdr = dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}
					a.A = ip
					rr = a
				}
				msg.Answer = append(msg.Answer, rr)
				continue
			}
			// PTR локальных диапазонов обслуживаем сами и наверх не отдаём:
			// публичный резолвер про 192.168.x.x всё равно не знает,
			// а LeakDNS-стиль утечки внутренних имён нам не нужен
			if question.Qtype == dns.TypePTR {
				if ip, ok := ParseReverse(question.Name); ok && IsLocalIP(ip) {
					if name := h.local.LookupReverse(ip); name != "" {
						if rr, err := ptrRR(question.Name, name); err == nil {
							msg.Answer = append(msg.Answer, rr)
						}
					} else {
						msg.Rcode = dns.RcodeNameError
					}
					continue
				}
			}

			// Локальная зона авторитетна и в forward-направлении
			// (аналог local=/lan/ в dnsmasq): незнакомое имя зоны и
			// не-A/AAAA типы обслуживаются локально и не утекают
			// на апстрим вместе с внутренними именами.
			if h.local.HasName(question.Name) {
				// Имя есть, записей запрошенного типа нет: NODATA
				// (NOERROR с пустым answer). NXDOMAIN здесь означал бы
				// "имени не существует" и у резолверов, кэширующих
				// негатив по имени, убил бы заодно и A — см. HasName.
				h.log.Debugf("Local zone NODATA for %s (type %d)", question.Name, question.Qtype)
				continue
			}
			if h.local.IsLocalName(question.Name) {
				h.log.Debugf("Local zone authoritative NXDOMAIN for %s (type %d)", question.Name, question.Qtype)
				msg.Rcode = dns.RcodeNameError
				continue
			}
		}

		domain := strings.TrimSuffix(question.Name, ".")
		if h.blockList != nil && h.blockList.IsBlocked(domain) {
			h.log.Debugf("Blocked domain: %s", domain)
			rr, err := dns.NewRR(fmt.Sprintf("%s A 0.0.0.0", question.Name))
			if err == nil {
				msg.Answer = append(msg.Answer, rr)
			}
			continue
		}

		answers, rcode := h.resolver(question.Name, question.Qtype, 0, false)
		if h.config.DNS.BlockPrivate {
			answers = h.filterPrivateAnswers(answers, question.Qtype)
		}
		if h.shouldProcess(question.Name) {
			h.log.Debugf("Processing question: %s", question.Name)
			h.processAnswers(answers, question.Name)
		}
		msg.Answer = append(msg.Answer, answers...)
		if rcode != dns.RcodeSuccess {
			msg.Rcode = rcode
		}
	}

	// Для UDP ответ не должен превышать буфер клиента (EDNS или 512 байт):
	// при переполнении обрезаем и ставим TC=1, клиент переспросит по TCP.
	if _, isUDP := w.RemoteAddr().(*net.UDPAddr); isUDP {
		size := 512
		if edns := r.IsEdns0(); edns != nil && int(edns.UDPSize()) > size {
			size = int(edns.UDPSize())
		}
		msg.Truncate(size)
	}

	if err := w.WriteMsg(msg); err != nil {
		h.log.Errorf("Failed to write response: %v", err)
	}
}

// StartHostsReloader периодически перечитывает hosts-файл (по mtime),
// если он задан в конфигурации.
func (h *Handler) StartHostsReloader(ctx context.Context) {
	if h.hosts == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := h.hosts.Reload(); err != nil {
					h.log.Warnf("Failed to reload hosts file: %v", err)
				}
			}
		}
	}()
}

// clientAllowed проверяет адрес клиента против ACL-подсетей.
// Пустой ACL разрешает всех.
func (h *Handler) clientAllowed(addr net.Addr) bool {
	if len(h.allowedNets) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, ipNet := range h.allowedNets {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

// filterPrivateAnswers удаляет приватные IP (RFC 1918/4193, loopback,
// link-local, unique-local) из ответов — защита от DNS Rebinding.
func (h *Handler) filterPrivateAnswers(answers []dns.RR, qtype uint16) []dns.RR {
	filtered := make([]dns.RR, 0, len(answers))
	for _, rr := range answers {
		var ip net.IP
		switch v := rr.(type) {
		case *dns.A:
			ip = v.A
		case *dns.AAAA:
			ip = v.AAAA
		default:
			filtered = append(filtered, rr)
			continue
		}
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			h.log.Debugf("Rebinding protection: filtered private IP %s", ip)
			continue
		}
		filtered = append(filtered, rr)
	}
	return filtered
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
					if listCfg.Persistent {
						effectiveTTL = 0 // без срока жизни
					}
					if err := h.ipSet.AddElement(ipv4Name, r.A.String(), effectiveTTL); err != nil {
						h.log.Errorf("Error adding address %s to ipset %s: %v", r.A.String(), ipv4Name, err)
						continue
					}
					if h.stateStore != nil {
						h.stateStore.Record(ipv4Name, r.A.String(), effectiveTTL)
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
					if listCfg.Persistent {
						effectiveTTL = 0 // без срока жизни
					}
					if err := h.ipSet.AddElement(ipv6Name, r.AAAA.String(), effectiveTTL); err != nil {
						h.log.Errorf("Error adding address %s to ipset %s: %v", r.AAAA.String(), ipv6Name, err)
						continue
					}
					if h.stateStore != nil {
						h.stateStore.Record(ipv6Name, r.AAAA.String(), effectiveTTL)
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

// resolver разрешает домен. skipCache=true используется префетчем: чтение
// кеша пропускается, чтобы обновить запись, а не вернуть снова устаревшую.
func (h *Handler) resolver(domain string, qtype uint16, depth int, skipCache bool) ([]dns.RR, int) {
	if depth > 10 {
		h.log.Warnf("CNAME loop detected for %s", domain)
		return nil, dns.RcodeServerFailure
	}

	if !skipCache {
		cached := h.getFromCache(domain, qtype)
		if cached != nil {
			h.log.Tracef("Cache hit for %s (type %d), returning %d records", domain, qtype, len(cached))
			return cached, dns.RcodeSuccess
		}
	}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(domain), qtype)
	m.RecursionDesired = true
	m.SetEdns0(1232, false) // EDNS0 для больших ответов; DO=0 — DNSSEC не валидируем

	var response *dns.Msg

	// Условная пересылка: если домен попадает в настроенную зону,
	// используем её upstream-серверы вместо глобальных.
	servers := h.config.FindForwardZone(domain)
	if servers == nil {
		servers = h.config.DNS.UpstreamServers
	}

	response = h.queryUpstreams(m, domain, servers)

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
			recursiveAnswers, rcode := h.resolver(cname.Target, qtype, depth+1, skipCache)
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

// isUsableRcode — коды ответов, которые считаем итоговым ответом upstream'а.
// SERVFAIL/REFUSED и прочие ошибки трактуются как сбой сервера и приводят
// к переключению на следующий upstream (failover).
func isUsableRcode(rcode int) bool {
	return rcode == dns.RcodeSuccess || rcode == dns.RcodeNameError
}

// isEncryptedScheme сообщает, шифруется ли трафик до upstream. Открытые
// схемы (tcp/udp, а также любые неизвестные) видны провайдеру и DPI.
func isEncryptedScheme(scheme string) bool {
	switch scheme {
	case "https", "doh", "tls", "dot":
		return true
	}
	return false
}

// queryUpstreams отправляет запрос upstream-серверам с failover и параллельной
// гонкой: запросы стартуют со сдвигом (stagger), первый валидный ответ
// выигрывает, остальные отменяются. Если все серверы не дали валидного
// ответа — возвращает nil (клиенту будет отдан SERVFAIL).
//
// Открытые upstream'ы (tcp/udp) идут отдельной, второй волной. В общей гонке
// имя запрашиваемого домена уходило бы в сеть открытым текстом на каждом
// запросе — даже когда DoH отвечает первым и его ответ выигрывает: гонку
// выигрывает ответ, но утечка к этому моменту уже произошла. Вторая волна
// стартует, только когда шифрованные upstream'ы кончились (все вернули
// ошибку) или когда истёк dns.timeout — то есть шифрованный upstream завис.
func (h *Handler) queryUpstreams(m *dns.Msg, domain string, servers []string) *dns.Msg {
	sorted := h.sortServers(servers)
	if len(sorted) == 0 {
		return nil
	}

	var encrypted, plain []string
	for _, ns := range sorted {
		if isEncryptedScheme(h.getScheme(ns)) {
			encrypted = append(encrypted, ns)
		} else {
			plain = append(plain, ns)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resCh := make(chan *dns.Msg, len(sorted))
	failCh := make(chan struct{}, len(sorted))

	launch := func(ns string) {
		go func() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			resp, err := h.exchangeWith(m, ns)
			if err == nil && resp != nil && isUsableRcode(resp.Rcode) {
				h.log.Debugf("Received a valid response for %s via %s with Rcode: %s", domain, ns, dns.RcodeToString[resp.Rcode])
				select {
				case resCh <- resp:
				case <-ctx.Done():
				}
				return
			}
			rcode := "N/A"
			if resp != nil {
				rcode = dns.RcodeToString[resp.Rcode]
			}
			h.log.Warnf("DNS error with %s for %s: %v, Rcode: %s", ns, domain, err, rcode)
			select {
			case failCh <- struct{}{}:
			case <-ctx.Done():
			}
		}()
	}

	// Таймеры отложенных запусков гасим на выходе, чтобы не будить
	// планировщик уже после того, как ответ отдан.
	var timers []*time.Timer
	defer func() {
		for _, t := range timers {
			t.Stop()
		}
	}()

	// launchWave: первый upstream волны опрашиваем сразу, остальные
	// подключаются с задержкой, чтобы не грузить все серверы при живом
	// приоритетном. Вызывается только из главной горутины — иначе гонка
	// на timers.
	launchWave := func(group []string) {
		for i, ns := range group {
			if i == 0 {
				launch(ns)
				continue
			}
			delay := time.Duration(i) * 300 * time.Millisecond
			if delay > 900*time.Millisecond {
				delay = 900 * time.Millisecond
			}
			timers = append(timers, time.AfterFunc(delay, func() { launch(ns) }))
		}
	}

	launchWave(encrypted)

	// pending — сколько upstream'ов реально запущено: пока вторая волна не
	// стартовала, её серверы в счётчике отказов не участвуют.
	pending := len(encrypted)
	plainStarted := false
	startPlain := func() {
		if plainStarted || len(plain) == 0 {
			return
		}
		if len(encrypted) > 0 {
			h.log.Debugf("Encrypted upstreams exhausted for %s, falling back to plaintext: %v", domain, plain)
		}
		launchWave(plain)
		plainStarted = true
		pending = len(sorted)
	}

	// Нет шифрованных upstream'ов — открытые стартуют сразу, ждать нечего.
	if len(encrypted) == 0 {
		startPlain()
	}

	var fallback <-chan time.Time
	if !plainStarted && len(plain) > 0 {
		t := time.NewTimer(h.timeout)
		defer t.Stop()
		fallback = t.C
	}

	failed := 0
	for failed < pending {
		select {
		case resp := <-resCh:
			return resp
		case <-failCh:
			failed++
			if !plainStarted && failed == len(encrypted) {
				startPlain()
				fallback = nil
			}
		case <-fallback:
			h.log.Debugf("Encrypted upstreams timed out for %s, falling back to plaintext: %v", domain, plain)
			startPlain()
			fallback = nil
		}
	}
	return nil
}

// exchangeWith выполняет обмен с одним upstream-сервером, разбирая схему,
// порт и протокол (udp/tcp/tls/https).
func (h *Handler) exchangeWith(m *dns.Msg, ns string) (*dns.Msg, error) {
	u, parseErr := url.Parse(ns)
	if parseErr != nil {
		if !strings.Contains(ns, "://") {
			u = &url.URL{Scheme: "udp", Host: ns}
		} else {
			return nil, fmt.Errorf("failed to parse upstream %s: %w", ns, parseErr)
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

	h.log.Debugf("Querying upstream %s using %s", ns, u.Scheme)

	switch u.Scheme {
	case "https", "doh":
		return h.exchangeDoH(m, ns)
	case "tls", "dot":
		client := &dns.Client{Net: "tcp-tls", Timeout: h.timeout, TLSConfig: &tls.Config{ServerName: u.Hostname()}}
		resp, _, err := client.Exchange(m, host)
		return resp, err
	case "tcp":
		client := &dns.Client{Net: "tcp", Timeout: h.timeout}
		resp, _, err := client.Exchange(m, host)
		return resp, err
	default: // udp
		client := &dns.Client{Net: "udp", Timeout: h.timeout}
		resp, _, err := client.Exchange(m, host)
		return resp, err
	}
}

func (h *Handler) getFromCache(domain string, qtype uint16) []dns.RR {
	h.log.Tracef("Cache getFromCache for %s (type %d)", domain, qtype)
	result, origTTL, remaining := h.dnsCache.GetWithMeta(fmt.Sprintf("%s|%d", domain, qtype))
	if result == nil {
		h.log.Tracef("Cache miss for %s (type %d)", domain, qtype)
		return nil
	}
	h.log.Tracef("Cache hit for %s (type %d)", domain, qtype)
	for _, rr := range result {
		h.log.Tracef("Cached RR: %s", rr.String())
	}

	// Демандный префетч: запись близка к истечению и домен входит в список
	// с prefetch:true — отдаём клиенту кеш немедленно, обновляем в фоне.
	if len(result) > 0 &&
		shouldPrefetch(origTTL, uint32(remaining/time.Second)) &&
		h.domainHasPrefetch(domain) {
		h.schedulePrefetch(domain, qtype)
	}
	return result
}

// shouldPrefetch — чистая функция условия префетча: срабатывает, когда
// остаток TTL меньше 10% исходного (пол 30 секунд).
func shouldPrefetch(origTTL, remaining uint32) bool {
	threshold := origTTL / 10
	if threshold < 30 {
		threshold = 30
	}
	return remaining < threshold
}

// domainHasPrefetch — домен матчится хотя бы в один ipset-список с prefetch.
func (h *Handler) domainHasPrefetch(domain string) bool {
	for i, listCfg := range h.config.GetIPSetLists() {
		if listCfg.Prefetch && h.isDomainInList(domain, i) {
			return true
		}
	}
	return false
}

// schedulePrefetch запускает фоновое обновление записи кеша. Дедупликация
// через singleflight: повторные обращения к граничному домену не порождают
// параллельных запросов к апстриму.
func (h *Handler) schedulePrefetch(domain string, qtype uint16) {
	key := fmt.Sprintf("%s|%d", domain, qtype)
	go func() {
		_, _, _ = h.prefetchGroup.Do(key, func() (interface{}, error) {
			select {
			case h.prefetchSem <- struct{}{}:
				defer func() { <-h.prefetchSem }()
			case <-time.After(5 * time.Second):
				return nil, nil // семафор перегружен, префетч не критичен
			}
			h.log.Debugf("Prefetching %s (type %d)", domain, qtype)
			rrs, rcode := h.resolver(domain, qtype, 0, true)
			if rcode == dns.RcodeSuccess && h.shouldProcess(domain) {
				// Обновит ipset новыми IP; старые записи не трогаем.
				h.processAnswers(rrs, domain)
			}
			return nil, nil
		})
	}()
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
