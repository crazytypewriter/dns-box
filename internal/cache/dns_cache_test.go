package cache

import (
	"testing"
	"time"

	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestGetWithMetaRoundTrip(t *testing.T) {
	c := NewDNSCache(1024*1024, log.New())

	a, err := dns.NewRR("example.com. 600 IN A 1.2.3.4")
	require.NoError(t, err)
	c.Set("example.com.|1", []dns.RR{a}, 600)

	rrs, origTTL, remaining := c.GetWithMeta("example.com.|1")
	require.Len(t, rrs, 1)
	require.Equal(t, uint32(600), origTTL)
	require.InDelta(t, 600*time.Second, remaining, float64(3*time.Second))

	// Негативная запись: пустой срез, но не nil
	c.Set("nx.example.com.|1", []dns.RR{}, 300)
	rrs, origTTL, remaining = c.GetWithMeta("nx.example.com.|1")
	require.NotNil(t, rrs)
	require.Empty(t, rrs)
	require.Equal(t, uint32(300), origTTL)

	// Промах
	rrs, _, _ = c.GetWithMeta("miss.|1")
	require.Nil(t, rrs)

	// Истёкшая запись удаляется
	c.Set("gone.|1", []dns.RR{a}, 0)
	time.Sleep(1100 * time.Millisecond)
	rrs, _, _ = c.GetWithMeta("gone.|1")
	require.Nil(t, rrs)

	// Get (старый API) по-прежнему работает
	got := c.Get("example.com.|1")
	require.Len(t, got, 1)
}
