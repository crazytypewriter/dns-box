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

func (c *DNSCache) Get(key string) []dns.RR {
	val := c.cache.Get(nil, []byte(key))
	if len(val) == 0 {
		return nil // Not in cache
	}

	expire := binary.BigEndian.Uint64(val[:8])
	if time.Now().Unix() > int64(expire) {
		c.cache.Del([]byte(key))
		log.Tracef("Cache entry expired for key: %s", key)
		return nil // Expired
	}

	buf := val[8:]
	if len(buf) == 0 {
		log.Tracef("Negative cache hit for key: %s", key)
		return []dns.RR{} // Negative cache hit
	}

	var rrs []dns.RR
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

	return rrs
}

func (c *DNSCache) Set(key string, rrs []dns.RR, ttl uint32) {
	expire := time.Now().Add(time.Duration(ttl) * time.Second).Unix()
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(expire))

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
