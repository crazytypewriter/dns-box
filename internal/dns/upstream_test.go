package dns

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// quietLogger — логгер без вывода: queryUpstreams пишет Warnf на каждый
// упавший upstream, и в тестах это только шум.
func quietLogger() *log.Logger {
	l := log.New()
	l.SetOutput(io.Discard)
	return l
}

// startUDPServer поднимает локальный DNS-сервер на 127.0.0.1, отвечающий
// одним A-record, и считает полученные запросы.
func startUDPServer(t *testing.T, ip string) (addr string, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)

	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		hits.Add(1)
		m := new(dns.Msg)
		m.SetReply(r)
		rr, _ := dns.NewRR(r.Question[0].Name + " 300 IN A " + ip)
		m.Answer = []dns.RR{rr}
		_ = w.WriteMsg(m)
	})

	srv := &dns.Server{PacketConn: pc, Handler: mux}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	return "udp://" + pc.LocalAddr().String(), hits
}

// startDoHServer поднимает DoH-эндпоинт (POST application/dns-message).
func startDoHServer(t *testing.T, ip string, delay time.Duration) (endpoint string, client *http.Client) {
	t.Helper()

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		req := new(dns.Msg)
		if err := req.Unpack(body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m := new(dns.Msg)
		m.SetReply(req)
		rr, _ := dns.NewRR(req.Question[0].Name + " 300 IN A " + ip)
		m.Answer = []dns.RR{rr}
		packed, err := m.Pack()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(packed)
	}))
	t.Cleanup(ts.Close)

	return ts.URL + "/dns-query", ts.Client()
}

// closedPort возвращает адрес, на котором заведомо никто не слушает.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func query(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return m
}

func answerIP(t *testing.T, resp *dns.Msg) string {
	t.Helper()
	require.NotNil(t, resp, "ожидался ответ от upstream")
	require.Len(t, resp.Answer, 1)
	a, ok := resp.Answer[0].(*dns.A)
	require.True(t, ok)
	return a.A.String()
}

// Открытый upstream не должен получить запрос, пока шифрованный отвечает:
// иначе имя домена утекает в сеть открытым текстом на каждом запросе,
// даже когда выигрывает ответ DoH.
func TestQueryUpstreamsKeepsPlaintextUnusedWhenDoHAnswers(t *testing.T) {
	plainAddr, plainHits := startUDPServer(t, "203.0.113.2")
	// Задержка больше stagger-сдвига (300 мс): в старой общей гонке
	// открытый upstream к этому моменту уже был бы запрошен.
	dohURL, client := startDoHServer(t, "203.0.113.1", 500*time.Millisecond)

	h := &Handler{log: quietLogger(), timeout: 5 * time.Second, httpClient: client}

	resp := h.queryUpstreams(query("example.com"), "example.com", []string{plainAddr, dohURL})

	require.Equal(t, "203.0.113.1", answerIP(t, resp), "ответить должен DoH")
	require.Zero(t, plainHits.Load(), "открытый upstream не должен быть запрошен")
}

// Когда шифрованные upstream'ы отвалились, открытый обязан подхватить —
// вторая волна стартует сразу по исчерпании первой, без ожидания таймаута.
func TestQueryUpstreamsFallsBackToPlaintextAfterEncryptedFail(t *testing.T) {
	plainAddr, plainHits := startUDPServer(t, "203.0.113.2")
	dead := "tls://" + closedPort(t) // connection refused, ошибка сразу

	h := &Handler{log: quietLogger(), timeout: 5 * time.Second}

	start := time.Now()
	resp := h.queryUpstreams(query("example.com"), "example.com", []string{plainAddr, dead})
	elapsed := time.Since(start)

	require.Equal(t, "203.0.113.2", answerIP(t, resp))
	require.EqualValues(t, 1, plainHits.Load())
	require.Less(t, elapsed, 2*time.Second, "fallback не должен ждать dns.timeout")
}

// Зависший шифрованный upstream не должен блокировать резолв навсегда:
// вторая волна стартует по dns.timeout.
func TestQueryUpstreamsFallsBackWhenEncryptedHangs(t *testing.T) {
	plainAddr, _ := startUDPServer(t, "203.0.113.2")

	// TLS-соединение принимаем, но handshake не завершаем никогда.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	h := &Handler{log: quietLogger(), timeout: 500 * time.Millisecond}

	start := time.Now()
	resp := h.queryUpstreams(query("example.com"), "example.com", []string{plainAddr, "tls://" + ln.Addr().String()})
	elapsed := time.Since(start)

	require.Equal(t, "203.0.113.2", answerIP(t, resp))
	require.GreaterOrEqual(t, elapsed, 400*time.Millisecond, "открытый upstream не должен стартовать раньше таймаута")
}

// Без шифрованных upstream'ов ждать нечего — открытые стартуют сразу.
func TestQueryUpstreamsPlaintextOnlyStartsImmediately(t *testing.T) {
	plainAddr, plainHits := startUDPServer(t, "203.0.113.2")

	h := &Handler{log: quietLogger(), timeout: 10 * time.Second}

	start := time.Now()
	resp := h.queryUpstreams(query("example.com"), "example.com", []string{plainAddr})

	require.Equal(t, "203.0.113.2", answerIP(t, resp))
	require.EqualValues(t, 1, plainHits.Load())
	require.Less(t, time.Since(start), time.Second)
}

// Все upstream'ы мертвы — nil, а не зависание (клиенту уйдёт SERVFAIL).
func TestQueryUpstreamsAllDeadReturnsNil(t *testing.T) {
	h := &Handler{log: quietLogger(), timeout: 500 * time.Millisecond}

	resp := h.queryUpstreams(query("example.com"), "example.com",
		[]string{"tls://" + closedPort(t), "tcp://" + closedPort(t)})

	require.Nil(t, resp)
}
