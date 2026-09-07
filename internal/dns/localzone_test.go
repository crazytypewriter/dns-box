package dns

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crazytypewriter/dns-box/internal/cache"
	C "github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func writeLocalFiles(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()

	leases := filepath.Join(dir, "dhcp.leases")
	require.NoError(t, os.WriteFile(leases, []byte(
		"1770000000 aa:bb:cc:dd:ee:01 192.168.1.10 nas *\n"+
			"1770000000 aa:bb:cc:dd:ee:02 192.168.1.11 printer hostid2\n"+
			"1770000000 aa:bb:cc:dd:ee:03 2a00:1370:1111:100::42 phone *\n"+ // глобальный v6 из leases тоже обслуживаем
			"1770000000 aa:bb:cc:dd:ee:04 192.168.1.12 * hostid3\n"), 0600))

	hosts := filepath.Join(dir, "hosts.odhcpd")
	require.NoError(t, os.WriteFile(hosts, []byte(
		"# odhcpd\n"+
			"fd00::10 nas\n"+
			"fd00::11 tv\n"+
			"fd00::12 nas\n"), 0600))

	return leases, hosts
}

func TestLocalZoneForwardAndReverse(t *testing.T) {
	leases, hosts := writeLocalFiles(t)
	z := NewLocalZone([]string{leases}, []string{hosts}, "lan", log.New())
	z.Reload()

	// Короткое имя и имя с поисковым доменом
	ip := z.LookupForward("nas.", dns.TypeA)
	require.NotNil(t, ip)
	require.Equal(t, "192.168.1.10", ip.String())
	ip = z.LookupForward("nas.lan.", dns.TypeAAAA)
	require.NotNil(t, ip)
	require.Equal(t, "fd00::10", ip.String())

	// odhcpd-запись
	ip = z.LookupForward("tv.lan.", dns.TypeAAAA)
	require.NotNil(t, ip)
	require.Equal(t, "fd00::11", ip.String())

	// Одно имя — несколько адресов разных версий
	ip = z.LookupForward("phone", dns.TypeAAAA)
	require.NotNil(t, ip)
	require.Equal(t, "2a00:1370:1111:100::42", ip.String())

	// Обратное разрешение
	require.Equal(t, "nas", z.LookupReverse(net.ParseIP("192.168.1.10")))
	require.Equal(t, "nas", z.LookupReverse(net.ParseIP("fd00::10")))

	// Безымянный lease не попадает в зону
	require.Nil(t, z.LookupForward("192.168.1.12.", dns.TypeA))

	// Незнакомое имя
	require.Nil(t, z.LookupForward("unknown.lan.", dns.TypeA))
}

func TestLocalZoneHotReload(t *testing.T) {
	dir := t.TempDir()
	leases := filepath.Join(dir, "dhcp.leases")
	require.NoError(t, os.WriteFile(leases, []byte("1770000000 aa:bb:cc:dd:ee:01 192.168.1.10 nas *\n"), 0600))

	z := NewLocalZone([]string{leases}, nil, "", log.New())
	z.Reload()
	require.NotNil(t, z.LookupForward("nas.", dns.TypeA))

	// Изменился файл — перечитали
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(leases, future, future))
	require.NoError(t, os.WriteFile(leases, []byte("1770000000 aa:bb:cc:dd:ee:01 192.168.1.99 nas2 *\n"), 0600))
	require.NoError(t, os.Chtimes(leases, future.Add(time.Second), future.Add(time.Second)))
	z.Reload()
	require.Nil(t, z.LookupForward("nas.", dns.TypeA), "старая запись должна уйти")
	require.NotNil(t, z.LookupForward("nas2.", dns.TypeA))
}

func TestParseReverse(t *testing.T) {
	ip, ok := ParseReverse("10.1.168.192.in-addr.arpa.")
	require.True(t, ok)
	require.Equal(t, "192.168.1.10", ip.String())

	// IPv6: 2001:db8::1 -> 1.0.0...0.8.b.d.0.1.0.0.2.ip6.arpa
	ip, ok = ParseReverse("1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.")
	require.True(t, ok)
	require.Equal(t, "2001:db8::1", ip.String())

	// Короткий v4-реверс (/<24) и мусор
	_, ok = ParseReverse("1.168.192.in-addr.arpa.")
	require.False(t, ok)
	_, ok = ParseReverse("example.com.")
	require.False(t, ok)
}

func TestIsLocalIP(t *testing.T) {
	require.True(t, IsLocalIP(net.ParseIP("192.168.1.1")))
	require.True(t, IsLocalIP(net.ParseIP("10.0.0.1")))
	require.True(t, IsLocalIP(net.ParseIP("fd00::1")))
	require.True(t, IsLocalIP(net.ParseIP("fe80::1")))
	require.True(t, IsLocalIP(net.ParseIP("127.0.0.1")))
	require.False(t, IsLocalIP(net.ParseIP("8.8.8.8")))
	require.False(t, IsLocalIP(net.ParseIP("2a00:1370::1")))
	require.False(t, IsLocalIP(nil))
}

// ServeDNS через локальную зону: A локального имени, PTR локального IP,
// и главное — PTR неизвестного локального адреса получает NXDOMAIN,
// а не уходит на апстрим (апстримов в конфиге нет: уход = SERVFAIL).
func TestServeDNSLocalZoneAndPTR(t *testing.T) {
	leases, hosts := writeLocalFiles(t)
	z := NewLocalZone([]string{leases}, []string{hosts}, "lan", log.New())
	z.Reload()

	h := &Handler{
		log:         log.New(),
		dnsCache:    C.NewDNSCache(1024, log.New()),
		domainCache: cache.NewDomainCache(1024),
		config:      &config.Config{}, // апстримов нет
		local:       z,
	}

	t.Run("local forward", func(t *testing.T) {
		w := queryDNS(h, "nas.lan.", dns.TypeA)
		require.Len(t, w.msg.Answer, 1)
		require.Equal(t, "192.168.1.10", w.msg.Answer[0].(*dns.A).A.String())
	})

	t.Run("local ptr known", func(t *testing.T) {
		w := queryDNS(h, "10.1.168.192.in-addr.arpa.", dns.TypePTR)
		require.Equal(t, dns.RcodeSuccess, w.msg.Rcode)
		require.Len(t, w.msg.Answer, 1)
		require.Equal(t, "nas.", w.msg.Answer[0].(*dns.PTR).Ptr)
	})

	t.Run("local ptr unknown stays local NXDOMAIN", func(t *testing.T) {
		// 192.168.1.99 нет в leases: NXDOMAIN, а не SERVFAIL
		// (SERVFAIL означал бы попытку форварда на пустой список апстримов)
		w := queryDNS(h, "99.1.168.192.in-addr.arpa.", dns.TypePTR)
		require.Equal(t, dns.RcodeNameError, w.msg.Rcode)
	})

	t.Run("public ptr goes upstream", func(t *testing.T) {
		// Публичный реверс не перехватывается: пустые апстримы → SERVFAIL
		w := queryDNS(h, "8.8.8.8.in-addr.arpa.", dns.TypePTR)
		require.Equal(t, dns.RcodeServerFailure, w.msg.Rcode)
	})
}

// ACL: клиент из неразрешённой подсети получает REFUSED.
func TestServeDNSACL(t *testing.T) {
	_, ipNet, err := net.ParseCIDR("192.168.0.0/16")
	require.NoError(t, err)

	h := &Handler{
		log:         log.New(),
		dnsCache:    C.NewDNSCache(1024, log.New()),
		domainCache: cache.NewDomainCache(1024),
		config:      &config.Config{},
		allowedNets: []*net.IPNet{ipNet},
	}

	w := queryDNS(h, "example.com.", dns.TypeA)
	require.Equal(t, dns.RcodeRefused, w.msg.Rcode, "клиент 192.0.2.1 (TEST-NET) должен быть отклонён")

	h.allowedNets = nil // пустой ACL — всем можно
	w = queryDNS(h, "example.com.", dns.TypeA)
	require.NotEqual(t, dns.RcodeRefused, w.msg.Rcode)
}

func queryDNS(h *Handler, name string, qtype uint16) *captureWriter {
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(name), qtype)
	w := &captureWriter{}
	h.ServeDNS(w, req)
	if w.msg == nil {
		panic(fmt.Sprintf("no response for %s", name))
	}
	return w
}

// Утечка 1: неизвестные .lan-имена и не-A/AAAA типы не должны уходить
// на апстрим — зона авторитетна в обе стороны (local=/lan/ у dnsmasq).
func TestServeDNSLocalZoneAuthoritative(t *testing.T) {
	leases, hosts := writeLocalFiles(t)
	z := NewLocalZone([]string{leases}, []string{hosts}, "lan", log.New())
	z.Reload()

	h := &Handler{
		log:         log.New(),
		dnsCache:    C.NewDNSCache(1024, log.New()),
		domainCache: cache.NewDomainCache(1024),
		config:      &config.Config{}, // апстримов нет: уход наверх = SERVFAIL
		local:       z,
	}

	// Неизвестное имя зоны → локальный NXDOMAIN, не SERVFAIL
	w := queryDNS(h, "ghost.lan.", dns.TypeA)
	require.Equal(t, dns.RcodeNameError, w.msg.Rcode, "unknown .lan name must get local NXDOMAIN")

	// Известное имя, но MX → NODATA локально (NOERROR + пустой answer),
	// не форвард и не NXDOMAIN: имя-то существует
	w = queryDNS(h, "nas.lan.", dns.TypeMX)
	require.Equal(t, dns.RcodeSuccess, w.msg.Rcode, "MX for existing local name must be NODATA, not NXDOMAIN")
	require.Empty(t, w.msg.Answer, "NODATA — ответ без записей")

	// AAAA у имени, которое есть только с A: тоже NODATA. NXDOMAIN здесь
	// у резолверов с негативным кэшем по имени убил бы и A-запрос.
	w = queryDNS(h, "printer.lan.", dns.TypeAAAA)
	require.Equal(t, dns.RcodeSuccess, w.msg.Rcode, "AAAA for v4-only local name must be NODATA")
	require.Empty(t, w.msg.Answer)

	// A того же имени после этого продолжает резолвиться
	w = queryDNS(h, "printer.lan.", dns.TypeA)
	require.Equal(t, dns.RcodeSuccess, w.msg.Rcode)
	require.Len(t, w.msg.Answer, 1)
	require.Equal(t, "192.168.1.11", w.msg.Answer[0].(*dns.A).A.String())

	// Публичное имя по-прежнему форвардится (SERVFAIL при пустых апстримах)
	w = queryDNS(h, "example.com.", dns.TypeMX)
	require.Equal(t, dns.RcodeServerFailure, w.msg.Rcode)

	// Без поискового домена авторитетны только известные имена из leases
	z2 := NewLocalZone([]string{leases}, nil, "", log.New())
	z2.Reload()
	h2 := &Handler{
		log:         log.New(),
		dnsCache:    C.NewDNSCache(1024, log.New()),
		domainCache: cache.NewDomainCache(1024),
		config:      &config.Config{},
		local:       z2,
	}
	w = queryDNS(h2, "ghost.lan.", dns.TypeA)
	require.Equal(t, dns.RcodeServerFailure, w.msg.Rcode, "without search domain, unknown .lan must forward")
}

// Утечка 2: CGNAT 100.64.0.0/10 — локальный (Tailscale), PTR не форвардится.
func TestIsLocalIPCGNAT(t *testing.T) {
	require.True(t, IsLocalIP(net.ParseIP("100.117.136.113")), "Tailscale CGNAT address must be local")
	require.True(t, IsLocalIP(net.ParseIP("100.64.0.0")))
	require.True(t, IsLocalIP(net.ParseIP("100.127.255.255")))
	require.False(t, IsLocalIP(net.ParseIP("100.128.0.0")), "адрес за пределами /10 — публичный")
}

// Заморозка 3: исчезнувший lease-файл опустошает зону, а не консервирует её.
func TestLocalZoneRemovedFileThaws(t *testing.T) {
	dir := t.TempDir()
	leases := filepath.Join(dir, "dhcp.leases")
	require.NoError(t, os.WriteFile(leases, []byte("1770000000 aa:bb:cc:dd:ee:01 192.168.1.10 nas *\n"), 0600))

	z := NewLocalZone([]string{leases}, nil, "", log.New())
	z.Reload()
	require.NotNil(t, z.LookupForward("nas.", dns.TypeA))

	// Файл снесли (dnsmasq в port=0 перестал писать) — зона должна опустеть
	require.NoError(t, os.Remove(leases))
	z.Reload()
	require.Nil(t, z.LookupForward("nas.", dns.TypeA), "записи из удалённого файла должны исчезнуть")

	// Файл вернулся — зона снова наполнилась
	require.NoError(t, os.WriteFile(leases, []byte("1770000000 aa:bb:cc:dd:ee:01 192.168.1.10 nas *\n"), 0600))
	z.Reload()
	require.NotNil(t, z.LookupForward("nas.", dns.TypeA))
}
