package dns

import (
	"bufio"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
)

// HostsResolver хранит записи из файла в формате /etc/hosts и отвечает
// на A/AAAA-запросы локальными адресами, минуя upstream и кэш.
type HostsResolver struct {
	mu      sync.RWMutex
	ipv4    map[string]string // lowercase fqdn -> IPv4
	ipv6    map[string]string // lowercase fqdn -> IPv6
	path    string
	modTime int64
	log     *log.Logger
}

func NewHostsResolver(path string, l *log.Logger) *HostsResolver {
	if l == nil {
		l = log.New()
	}
	return &HostsResolver{
		ipv4: map[string]string{},
		ipv6: map[string]string{},
		path: path,
		log:  l,
	}
}

// Reload перечитывает hosts-файл. Файл перечитывается только если
// его mtime изменился, поэтому вызов можно делать на каждый запрос.
func (h *HostsResolver) Reload() error {
	if h.path == "" {
		return nil
	}
	st, err := os.Stat(h.path)
	if err != nil {
		return err
	}
	h.mu.RLock()
	unchanged := st.ModTime().Unix() == h.modTime
	h.mu.RUnlock()
	if unchanged {
		return nil
	}

	f, err := os.Open(h.path)
	if err != nil {
		return err
	}
	defer f.Close()

	ipv4 := map[string]string{}
	ipv6 := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.IndexAny(line, "#;"); i >= 0 {
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
			name = strings.ToLower(dns.Fqdn(name))
			if ip.To4() != nil {
				ipv4[name] = ip.String()
			} else {
				ipv6[name] = ip.String()
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	h.mu.Lock()
	h.ipv4 = ipv4
	h.ipv6 = ipv6
	h.modTime = st.ModTime().Unix()
	h.mu.Unlock()

	h.log.Infof("Hosts file %s loaded: %d IPv4 records, %d IPv6 records", h.path, len(ipv4), len(ipv6))
	return nil
}

// Lookup возвращает локальный IP для домена или nil.
func (h *HostsResolver) Lookup(domain string, qtype uint16) net.IP {
	if h == nil || h.path == "" {
		return nil
	}
	name := strings.ToLower(dns.Fqdn(domain))
	h.mu.RLock()
	defer h.mu.RUnlock()
	switch qtype {
	case dns.TypeA:
		if s, ok := h.ipv4[name]; ok {
			return net.ParseIP(s)
		}
	case dns.TypeAAAA:
		if s, ok := h.ipv6[name]; ok {
			return net.ParseIP(s)
		}
	}
	return nil
}
