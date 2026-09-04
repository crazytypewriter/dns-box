package cache

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContainsSuffix(t *testing.T) {
	c := NewDomainCache(1024)
	c.AddSuffix(".googlevideo.com")
	c.AddSuffix(".scontent.cdninstagram.com")
	c.AddSuffix(".youtubei.googleapis.com")
	c.AddSuffix(".e6858.dsce9.akamaiedge.net")

	cases := []struct {
		suffix string
		want   bool
	}{
		// Регистрируется и находится — суффиксы длиннее двух меток
		{".googlevideo.com", true},
		{".scontent.cdninstagram.com", true},
		{".youtubei.googleapis.com", true},
		{".e6858.dsce9.akamaiedge.net", true},

		// Поддомен НЕ схлопывается в двухметочный суффикс
		{".foo.googlevideo.com", false},
		{".cdninstagram.com", false},
		{".akamaiedge.net", false},

		// Посторонние
		{".example.com", false},
		{".googleapis.com", false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, c.ContainsSuffix(tc.suffix), "ContainsSuffix(%q)", tc.suffix)
	}

	// Тот же кеш: ContainsSuffix не должен портить последующие проверки
	require.True(t, c.ContainsSuffix(".googlevideo.com"))
}

func TestAddSuffixNormalizes(t *testing.T) {
	c := NewDomainCache(1024)
	c.AddSuffix("youtube.com") // без точки — нормализуется в .youtube.com
	require.True(t, c.ContainsSuffix(".youtube.com"))
	require.True(t, c.ContainsSuffix("youtube.com"))
}
