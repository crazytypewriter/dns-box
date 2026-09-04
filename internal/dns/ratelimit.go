package dns

import (
	"net"
	"sync"
	"time"
)

// clientBucket — простой token bucket для одного клиента.
type clientBucket struct {
	tokens   float64
	lastSeen time.Time
}

// RateLimiter ограничивает частоту запросов с одного клиентского IP.
// При limit <= 0 ограничения отключены.
//
// burst отделён от rate: пол в 10 токенов, чтобы низкий rate_limit
// (например 1 rps) не убивал нормальные всплески — первый «залп» из
// нескольких запросов проходит, дальше жёстко 1/сек.
type RateLimiter struct {
	mu      sync.Mutex
	limit   float64 // tokens per second
	burst   float64
	buckets map[string]*clientBucket
}

const minBurst = 10

func NewRateLimiter(perSecond int) *RateLimiter {
	burst := float64(perSecond)
	if burst < minBurst {
		burst = minBurst
	}
	return &RateLimiter{
		limit:   float64(perSecond),
		burst:   burst,
		buckets: map[string]*clientBucket{},
	}
}

// Allow сообщает, разрешён ли запрос с адреса addr, и пополняет корзину.
func (r *RateLimiter) Allow(addr string) bool {
	if r == nil || r.limit <= 0 {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	b, ok := r.buckets[host]
	if !ok {
		// Периодическая чистка неактивных клиентов
		if len(r.buckets) > 10000 {
			for k, v := range r.buckets {
				if now.Sub(v.lastSeen) > time.Hour {
					delete(r.buckets, k)
				}
			}
		}
		b = &clientBucket{tokens: r.burst, lastSeen: now}
		r.buckets[host] = b
	}

	// Пополняем по прошедшему времени
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.lastSeen = now
	b.tokens += elapsed * r.limit
	if b.tokens > r.burst {
		b.tokens = r.burst
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
