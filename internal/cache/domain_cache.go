package cache

import (
	"github.com/VictoriaMetrics/fastcache"
	"strings"
)

type DomainCache struct {
	cache *fastcache.Cache
}

func NewDomainCache(size int) *DomainCache {
	return &DomainCache{
		cache: fastcache.New(size),
	}
}

func (c *DomainCache) Add(domain string) {
	c.cache.Set([]byte(domain), nil)
}

func (c *DomainCache) AddSuffix(suffix string) {
	if !strings.HasPrefix(suffix, ".") {
		suffix = "." + suffix
	}
	c.cache.Set([]byte(suffix), nil)
}

func (c *DomainCache) Remove(domain string) {
	c.cache.Del([]byte(domain))
}

func (c *DomainCache) Contains(domain string) bool {
	return c.cache.Has([]byte(domain))
}

// ContainsSuffix проверяет точное совпадение суффикса. Обрезка до двух
// меток здесь запрещена: вызывающий код (shouldProcess/isDomainInList)
// сам перебирает суффиксы от полного имени к короткому, повторная обрезка
// ломала суффиксы длиннее двух меток (.scontent.cdninstagram.com) и,
// наоборот, матчила лишнее (.foo.googlevideo.com -> .googlevideo.com).
func (c *DomainCache) ContainsSuffix(domain string) bool {
	if !strings.HasPrefix(domain, ".") {
		domain = "." + domain
	}
	return c.cache.Has([]byte(domain))
}
