package dns

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
)

// LocalZone обслуживает локальную зону: имена DHCP-клиентов из lease-файлов
// dnsmasq (/tmp/dhcp.leases) и hosts-файлов odhcpd (/tmp/hosts/odhcpd),
// включая PTR для локальных адресов. Позволяет убрать dnsmasq из
// DNS-цепочки (port=0) и закрыть двойной хоп.
type LocalZone struct {
	mu      sync.RWMutex
	forward map[string][]net.IP // lowercase fqdn -> IP (v4 и v6 вместе)
	reverse map[string]string   // IP -> первое встреченное имя
	domain  string              // поисковый домен без точки, lowercase
	leases  []string
	hosts   []string
	mtimes  map[string]int64
	log     *log.Logger
}

func NewLocalZone(leasesFiles, hostsFiles []string, domain string, l *log.Logger) *LocalZone {
	if l == nil {
		l = log.New()
	}
	return &LocalZone{
		forward: map[string][]net.IP{},
		reverse: map[string]string{},
		domain:  strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), "."),
		leases:  leasesFiles,
		hosts:   hostsFiles,
		mtimes:  map[string]int64{},
		log:     l,
	}
}

// Reload перечитывает файлы, изменившиеся по mtime. Повторный вызов без
// изменений ничего не стоит, поэтому его можно дёргать тикером.
func (z *LocalZone) Reload() {
	changed := false
	for _, path := range append(append([]string{}, z.leases...), z.hosts...) {
		if path == "" {
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			// Файл исчез. Если раньше он читался — зону нужно пересобрать
			// без него, иначе записи из него заморозятся навсегда
			// (например, dnsmasq в port=0 перестал писать leases).
			if _, was := z.mtimes[path]; was {
				delete(z.mtimes, path)
				changed = true
			}
			continue
		}
		mt := st.ModTime().Unix()
		if z.mtimes[path] != mt {
			z.mtimes[path] = mt
			changed = true
		}
	}
	if !changed {
		return
	}

	forward := map[string][]net.IP{}
	reverse := map[string]string{}

	for _, path := range z.leases {
		z.parseLeasesFile(path, forward, reverse)
	}
	for _, path := range z.hosts {
		z.parseHostsFile(path, forward, reverse)
	}

	z.mu.Lock()
	z.forward = forward
	z.reverse = reverse
	z.mu.Unlock()

	z.log.Infof("Local zone reloaded: %d names, %d addresses (from %d lease files, %d hosts files)",
		len(reverse), len(forward), len(z.leases), len(z.hosts))
}

// parseLeasesFile читает dnsmasq-формат: "<expires> <mac> <ip> <name> <clientid>".
func (z *LocalZone) parseLeasesFile(path string, forward map[string][]net.IP, reverse map[string]string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		ip := net.ParseIP(fields[2])
		if ip == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(fields[3], "."))
		if name == "" || name == "*" {
			continue
		}
		addLocalEntry(forward, reverse, name, ip, z.domain)
	}
}

// parseHostsFile читает hosts-формат odhcpd: "ip name [name...]".
func (z *LocalZone) parseHostsFile(path string, forward map[string][]net.IP, reverse map[string]string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil {
			continue
		}
		for _, name := range fields[1:] {
			name = strings.ToLower(strings.TrimSuffix(name, "."))
			if name == "" {
				continue
			}
			addLocalEntry(forward, reverse, name, ip, z.domain)
		}
	}
}

func addLocalEntry(forward map[string][]net.IP, reverse map[string]string, name string, ip net.IP, domain string) {
	fqdn := dns.Fqdn(name)
	forward[fqdn] = appendUniqueIP(forward[fqdn], ip)
	if domain != "" {
		fq := dns.Fqdn(name + "." + domain)
		forward[fq] = appendUniqueIP(forward[fq], ip)
	}
	if _, ok := reverse[ip.String()]; !ok {
		reverse[ip.String()] = name
	}
}

func appendUniqueIP(ips []net.IP, ip net.IP) []net.IP {
	for _, existing := range ips {
		if existing.Equal(ip) {
			return ips
		}
	}
	return append(ips, ip)
}

// LookupForward возвращает локальный IP для имени (A/AAAA) или nil.
func (z *LocalZone) LookupForward(name string, qtype uint16) net.IP {
	z.mu.RLock()
	defer z.mu.RUnlock()

	ips := z.forward[dns.Fqdn(strings.ToLower(name))]
	for _, ip := range ips {
		if qtype == dns.TypeA && ip.To4() != nil {
			return ip
		}
		if qtype == dns.TypeAAAA && ip.To4() == nil {
			return ip
		}
	}
	return nil
}

// HasName — имя присутствует в зоне хотя бы с одной записью (в любой
// форме: короткой или с поисковым доменом). Отличать это от IsLocalName
// обязательно: если имя есть, но записей запрошенного типа нет, по
// RFC 2308 полагается NODATA (NOERROR с пустым answer), а не NXDOMAIN.
// NXDOMAIN относится к имени целиком, и резолверы, кэширующие
// отрицательный ответ по имени, а не по паре имя+тип (Windows DNS
// Client, systemd-resolved), после NXDOMAIN на AAAA перестанут
// резолвить и A.
func (z *LocalZone) HasName(name string) bool {
	z.mu.RLock()
	defer z.mu.RUnlock()

	_, ok := z.forward[dns.Fqdn(strings.ToLower(name))]
	return ok
}

// IsLocalName — имя, за которое локальная зона отвечает авторитетно:
// либо оно есть в lease/hosts-данных (в любой форме — короткой или с
// поисковым доменом), либо попадает под поисковый домен. Для таких имён
// ответ формируется локально и не уходит на апстрим (аналог local=/lan/
// в dnsmasq): несуществующее имя получает NXDOMAIN, существующее без
// записей нужного типа — NODATA, см. HasName.
func (z *LocalZone) IsLocalName(name string) bool {
	z.mu.RLock()
	defer z.mu.RUnlock()

	name = dns.Fqdn(strings.ToLower(name))
	if _, ok := z.forward[name]; ok {
		return true
	}
	if z.domain != "" && strings.HasSuffix(name, "."+z.domain+".") {
		return true
	}
	return false
}

// LookupReverse возвращает имя для IP или "".
func (z *LocalZone) LookupReverse(ip net.IP) string {
	z.mu.RLock()
	defer z.mu.RUnlock()
	return z.reverse[ip.String()]
}

// ParseReverse разбирает *.in-addr.arpa / *.ip6.arpa в IP.
func ParseReverse(name string) (net.IP, bool) {
	name = strings.ToLower(dns.Fqdn(name))
	switch {
	case strings.HasSuffix(name, ".in-addr.arpa."):
		labels := strings.Split(strings.TrimSuffix(name, ".in-addr.arpa."), ".")
		// 4 метки октетов в обратном порядке
		if len(labels) != 4 {
			return nil, false
		}
		for i := 0; i < len(labels)/2; i++ {
			labels[i], labels[len(labels)-1-i] = labels[len(labels)-1-i], labels[i]
		}
		ip := net.ParseIP(strings.Join(labels, "."))
		if ip == nil || ip.To4() == nil {
			return nil, false
		}
		return ip, true
	case strings.HasSuffix(name, ".ip6.arpa."):
		digits := strings.TrimSuffix(name, ".ip6.arpa.")
		labels := strings.Split(digits, ".")
		if len(labels) != 32 {
			return nil, false
		}
		// 32 nibble в обратном порядке -> 16 байт
		buf := make([]byte, 16)
		for i := 0; i < 32; i++ {
			// labels[31-i] — прямой порядок
			d, ok := nibble(labels[31-i])
			if !ok {
				return nil, false
			}
			if i%2 == 0 {
				buf[i/2] |= d << 4
			} else {
				buf[i/2] |= d
			}
		}
		return net.IP(buf), true
	}
	return nil, false
}

func nibble(c string) (byte, bool) {
	if len(c) != 1 {
		return 0, false
	}
	switch {
	case c[0] >= '0' && c[0] <= '9':
		return c[0] - '0', true
	case c[0] >= 'a' && c[0] <= 'f':
		return c[0] - 'a' + 10, true
	}
	return 0, false
}

// cgnat — 100.64.0.0/10 (RFC 6598): net.IP.IsPrivate про него не знает,
// но это локальный диапазон (Tailscale/CGNAT), PTR не должен уходить наверх.
var cgnat = func() *net.IPNet {
	_, ipNet, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic(err)
	}
	return ipNet
}()

// IsLocalIP — адрес из приватных/локальных диапазонов (RFC 1918/4193,
// RFC 6598 CGNAT, loopback, link-local). PTR для таких адресов не должен
// уходить наверх.
func IsLocalIP(ip net.IP) bool {
	return ip != nil && (ip.IsPrivate() || cgnat.Contains(ip) || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified())
}

// StartReloader перечитывает файлы зоны по тикеру.
func (z *LocalZone) StartReloader(ctx context.Context) {
	if z == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				z.Reload()
			}
		}
	}()
}

// ptrRR собирает PTR-запись с фиксированным локальным TTL.
func ptrRR(reverseName, target string) (dns.RR, error) {
	return dns.NewRR(fmt.Sprintf("%s 60 IN PTR %s", reverseName, dns.Fqdn(target)))
}
