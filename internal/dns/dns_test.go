package dns

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/crazytypewriter/dns-box/internal/cache"
	C "github.com/crazytypewriter/dns-box/internal/cache"
	"github.com/crazytypewriter/dns-box/internal/config"
	"github.com/crazytypewriter/dns-box/internal/ipsetstate"
	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(5) // 5 rps; burst = max(5, пол 10) = 10

	for i := 0; i < 10; i++ {
		require.True(t, rl.Allow("192.0.2.1:53"), "запросы в пределах burst должны проходить")
	}
	require.False(t, rl.Allow("192.0.2.1:53"), "запрос сверх burst должен быть отклонён")

	// Другой клиент не зависит от первого
	require.True(t, rl.Allow("192.0.2.2:53"))

	// Отключённый лимитер всё разрешает
	off := NewRateLimiter(0)
	for i := 0; i < 100; i++ {
		require.True(t, off.Allow("192.0.2.3:53"))
	}
}

func TestHostsResolver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	err := os.WriteFile(path, []byte("# comment\n127.0.0.1 localhost\n192.168.1.10 nas.lan other.lan\n::1 localhost\nbad.entry x\n"), 0600)
	require.NoError(t, err)

	h := NewHostsResolver(path, log.New())
	require.NoError(t, h.Reload())

	ip := h.Lookup("nas.lan.", dns.TypeA)
	require.NotNil(t, ip)
	require.Equal(t, "192.168.1.10", ip.String())

	// второй алиас из той же строки
	ip = h.Lookup("other.lan.", dns.TypeA)
	require.NotNil(t, ip)

	// IPv6
	ip = h.Lookup("localhost.", dns.TypeAAAA)
	require.NotNil(t, ip)
	require.Equal(t, "::1", ip.String())

	// Нет записи AAAA для nas.lan
	require.Nil(t, h.Lookup("nas.lan.", dns.TypeAAAA))

	// Неизвестный домен
	require.Nil(t, h.Lookup("unknown.lan.", dns.TypeA))

	// Пустой резолвер (файл не задан) всегда возвращает nil
	empty := NewHostsResolver("", log.New())
	require.Nil(t, empty.Lookup("any.", dns.TypeA))
}

func TestFilterPrivateAnswers(t *testing.T) {
	h := &Handler{log: log.New()}

	mkA := func(ip string) dns.RR {
		rr, _ := dns.NewRR("example.com. 300 IN A " + ip)
		return rr
	}
	mkCNAME := func() dns.RR {
		rr, _ := dns.NewRR("example.com. 300 IN CNAME target.example.com.")
		return rr
	}

	answers := []dns.RR{mkA("8.8.8.8"), mkA("192.168.1.1"), mkA("127.0.0.1"), mkA("10.0.0.5"), mkA("1.1.1.1"), mkCNAME()}
	filtered := h.filterPrivateAnswers(answers, dns.TypeA)

	require.Len(t, filtered, 3, "приватные IP должны быть отфильтрованы")
	require.Equal(t, "8.8.8.8", filtered[0].(*dns.A).A.String())
	require.Equal(t, "1.1.1.1", filtered[1].(*dns.A).A.String())
	require.IsType(t, &dns.CNAME{}, filtered[2], "CNAME не должен быть отфильтрован")
}

func TestIsUsableRcode(t *testing.T) {
	require.True(t, isUsableRcode(dns.RcodeSuccess))
	require.True(t, isUsableRcode(dns.RcodeNameError))
	require.False(t, isUsableRcode(dns.RcodeServerFailure))
	require.False(t, isUsableRcode(dns.RcodeRefused))
}

func TestShouldPrefetch(t *testing.T) {
	cases := []struct {
		name               string
		origTTL, remaining uint32
		want               bool
	}{
		{"свежая запись", 3600, 3600, false},
		{"ровно 10% — не срабатывает (строго меньше)", 3600, 360, false},
		{"чуть меньше 10% — срабатывает", 3600, 359, true},
		{"пол 30с: короткий TTL", 300, 25, true},
		{"пол 30с: 30 ровно — не срабатывает", 300, 30, false},
		{"минимальный нормализованный TTL 300: порог 30с", 300, 29, true},
		{"истекающая", 3600, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, shouldPrefetch(tc.origTTL, tc.remaining))
		})
	}
}

// fakeIPSet записывает вызовы для проверок в тестах.
type fakeIPSet struct {
	added    map[string]map[string]uint32 // set -> ip -> ttl
	created  map[string]bool
	maxElem  map[string]uint32
	listErr  error
	existing map[string][]string // что «уже лежит» в сете для ListElements
}

func newFakeIPSet() *fakeIPSet {
	return &fakeIPSet{
		added:    map[string]map[string]uint32{},
		created:  map[string]bool{},
		maxElem:  map[string]uint32{},
		existing: map[string][]string{},
	}
}

func (f *fakeIPSet) create(name string, maxElem uint32) error {
	f.created[name] = true
	f.maxElem[name] = maxElem
	return nil
}

func (f *fakeIPSet) CreateIPv4Set(name string, timeout, maxElem uint32) error {
	return f.create(name, maxElem)
}
func (f *fakeIPSet) CreateIPv6Set(name string, timeout, maxElem uint32) error {
	return f.create(name, maxElem)
}
func (f *fakeIPSet) CreateIPv4NetSet(name string, timeout, maxElem uint32) error {
	return f.create(name, maxElem)
}
func (f *fakeIPSet) CreateIPv6NetSet(name string, timeout, maxElem uint32) error {
	return f.create(name, maxElem)
}
func (f *fakeIPSet) AddElement(set, ip string, ttl uint32) error {
	if f.added[set] == nil {
		f.added[set] = map[string]uint32{}
	}
	f.added[set][ip] = ttl
	return nil
}
func (f *fakeIPSet) RemoveElement(set, ip string) error {
	delete(f.added[set], ip)
	return nil
}
func (f *fakeIPSet) ListElements(set string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.existing[set], nil
}

func TestProcessAnswersPersistentAndStore(t *testing.T) {
	cfg := &config.Config{
		IPSet: config.IPSetConfig{
			Lists: []config.IPSetListConfig{
				{Name: "vpn", EnableIPv6: true, Rules: config.RulesConfig{
					Domains: []string{"example.com"},
				}},
				{Name: "sticky", Persistent: true, Rules: config.RulesConfig{
					DomainSuffix: []string{".sticky.com"},
				}},
			},
		},
	}

	l := log.New()
	listCaches := map[int]*cache.DomainCache{
		0: cache.NewDomainCache(1024),
		1: cache.NewDomainCache(1024),
	}
	listCaches[0].Add("example.com")
	listCaches[1].AddSuffix(".sticky.com")

	fake := newFakeIPSet()
	store := ipsetstate.NewStore(100, l)
	h := &Handler{
		config:           cfg,
		ipSet:            fake,
		stateStore:       store,
		listDomainCaches: listCaches,
		log:              l,
	}

	aRR, _ := dns.NewRR("example.com. 600 IN A 1.2.3.4")
	aaaaRR, _ := dns.NewRR("example.com. 600 IN AAAA 2001:db8::1")
	stickyRR, _ := dns.NewRR("foo.sticky.com. 600 IN A 9.9.9.9")
	h.processAnswers([]dns.RR{aRR, aaaaRR, stickyRR}, "example.com.")

	// Обычный список: нормализованный TTL (600 < 180? нет, 600 в [300,3600] → 600)
	require.Equal(t, uint32(600), fake.added["vpn"]["1.2.3.4"])
	require.Equal(t, uint32(600), fake.added["vpn6"]["2001:db8::1"])
	// Persistent список: TTL 0 (вечная запись)
	require.Equal(t, uint32(0), fake.added["sticky"]["9.9.9.9"])
	// Всё отражено в сторе состояния
	entries := store.Snapshot()
	require.Len(t, entries, 3)
}

func TestRateLimiterLowRateHasBurst(t *testing.T) {
	// rate_limit: 1 — burst отделён от rate, первый всплеск проходит
	rl := NewRateLimiter(1)
	for i := 0; i < 10; i++ {
		require.True(t, rl.Allow("192.0.2.10:53"), "burst floor 10 должен пропускать всплеск")
	}
	require.False(t, rl.Allow("192.0.2.10:53"), "после исчерпания burst — отказ")
}

func TestServeDNSNoAuthoritativeNoDO(t *testing.T) {
	// Форвардер не авторитетен и не проксирует DO-бит.
	// Пустой список апстримов → быстрый SERVFAIL без сети.
	h := &Handler{
		log:         log.New(),
		dnsCache:    C.NewDNSCache(1024, log.New()),
		domainCache: cache.NewDomainCache(1024),
		config:      &config.Config{},
	}
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn("example.com."), dns.TypeA)
	req.SetEdns0(4096, true) // клиент просит DNSSEC

	w := &captureWriter{}
	h.ServeDNS(w, req)

	require.False(t, w.msg.Authoritative, "AA должен быть 0 у форвардера")
	if edns := w.msg.IsEdns0(); edns != nil {
		require.False(t, edns.Do(), "DO должен быть 0 — DNSSEC не валидируется")
	}
}

type captureWriter struct {
	msg *dns.Msg
}

func (w *captureWriter) WriteMsg(m *dns.Msg) error {
	w.msg = m
	return nil
}
func (w *captureWriter) Write(b []byte) (int, error)  { return len(b), nil }
func (w *captureWriter) RemoteAddr() net.Addr         { return dummyAddr{} }
func (w *captureWriter) LocalAddr() net.Addr          { return dummyAddr{} }
func (w *captureWriter) Close() error                 { return nil }
func (w *captureWriter) TsigStatus() error            { return nil }
func (w *captureWriter) TsigTimersOnly(bool)          {}
func (w *captureWriter) Hijack()                      {}
func (w *captureWriter) TsigVariable() ([]byte, bool) { return nil, false }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "udp" }
func (dummyAddr) String() string  { return "192.0.2.1:12345" }
