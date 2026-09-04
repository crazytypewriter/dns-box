package cache

import (
	"encoding/binary"
	"github.com/VictoriaMetrics/fastcache"
	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"time"
)

type DNSCache struct {
	cache *fastcache.Cache
	log   *log.Logger
}

func NewDNSCache(size int, l *log.Logger) *DNSCache {
	return &DNSCache{
		cache: fastcache.New(size),
		log:   l,
	}
}

// Get возвращает записи из кеша по ключу. Для негативных записей
// возвращается пустой (не nil) срез.
func (c *DNSCache) Get(key string) []dns.RR {
	rrs, _, _ := c.GetWithMeta(key)
	return rrs
}

// GetWithMeta помимо записей возвращает исходный (нормализованный) TTL
// записи и остаток времени до истечения — нужно для решения о префетче.
// Формат записи: [0:8] expire uint64 unix, [8:12] origTTL uint32, [12:] RR.
func (c *DNSCache) GetWithMeta(key string) (rrs []dns.RR, origTTL uint32, remaining time.Duration) {
	val := c.cache.Get(nil, []byte(key))
	if len(val) < 12 {
		return nil, 0, 0 // отсутствует или запись старого/битого формата
	}

	expire := int64(binary.BigEndian.Uint64(val[:8]))
	origTTL = binary.BigEndian.Uint32(val[8:12])
	now := time.Now().Unix()
	if now > expire {
		c.cache.Del([]byte(key))
		log.Tracef("Cache entry expired for key: %s", key)
		return nil, 0, 0 // Expired
	}
	remaining = time.Duration(expire-now) * time.Second

	buf := val[12:]
	if len(buf) == 0 {
		log.Tracef("Negative cache hit for key: %s", key)
		return []dns.RR{}, origTTL, remaining // Negative cache hit
	}

	offset := 0
	for offset < len(buf) {
		if offset+2 > len(buf) {
			break
		}
		packedLen := binary.BigEndian.Uint16(buf[offset : offset+2])
		offset += 2

		if offset+int(packedLen) > len(buf) {
			break
		}

		rr, _, err := dns.UnpackRR(buf, offset)
		if err != nil {
			c.log.Debugf("Error unpacking RR at offset %d, length %d: %v\n", offset, packedLen, err)
			offset += int(packedLen)
			continue
		}
		rrs = append(rrs, rr)
		offset += int(packedLen)
	}

	return rrs, origTTL, remaining
}

func (c *DNSCache) Set(key string, rrs []dns.RR, ttl uint32) {
	expire := time.Now().Add(time.Duration(ttl) * time.Second).Unix()
	buf := make([]byte, 12)
	binary.BigEndian.PutUint64(buf, uint64(expire))
	binary.BigEndian.PutUint32(buf[8:12], ttl)

	if len(rrs) > 0 {
		for _, rr := range rrs {
			packed := make([]byte, dns.MaxMsgSize)
			packedLen, err := dns.PackRR(rr, packed, 0, nil, false)
			if err != nil {
				c.log.Debugf("Error packing RR: %v\n", err)
				continue
			}
			lenBuf := make([]byte, 2)
			binary.BigEndian.PutUint16(lenBuf, uint16(packedLen))
			buf = append(buf, lenBuf...)
			buf = append(buf, packed[:packedLen]...)
		}
	}

	log.Tracef("Set cache with key, %s and ttl %d. Record count: %d", key, ttl, len(rrs))
	c.cache.Set([]byte(key), buf)
}
