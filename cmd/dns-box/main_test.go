package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crazytypewriter/dns-box/internal/config"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

func TestBlocklistE2E(t *testing.T) {
	// 1. Подготовка
	// Используем io.Pipe для безопасного перехвата логов между горутинами
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	tmpDir := t.TempDir()

	// Создание временного файла конфигурации
	testPort := 53531
	cfg := config.Config{
		Server: config.ServerConfig{
			Address: []string{fmt.Sprintf("127.0.0.1:%d", testPort)},
			Log:     "info", // Добавляем уровень логирования
		},
		DNS: config.DNSConfig{
			UpstreamServers: []string{"8.8.8.8:53"},
		},
		BlockList: config.BlockListConfig{
			URLs: []string{"https://blocklistproject.github.io/Lists/tracking.txt"},
		},
	}
	configPath := filepath.Join(tmpDir, "test_config.json")
	configData, err := json.Marshal(cfg)
	require.NoError(t, err)
	err = os.WriteFile(configPath, configData, 0600)
	require.NoError(t, err)

	// 2. Запуск приложения
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- run(ctx, configPath, w)
	}()

	// Ожидаем сообщения об успешном обновлении списков блокировки
	blocklistUpdated := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Blocklists updated successfully") {
				close(blocklistUpdated)
			}
		}
	}()

	select {
	case <-blocklistUpdated:
		// Успешно, продолжаем тест
	case <-time.After(30 * time.Second): // Увеличенный таймаут на случай медленной сети
		t.Fatal("Таймаут ожидания обновления blocklist")
	}

	// 3. Проверка
	client := new(dns.Client)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", testPort)

	// Проверка заблокированного домена
	t.Run("blocked domain", func(t *testing.T) {
		msg := new(dns.Msg)
		msg.SetQuestion(dns.Fqdn("mc.yandex.ru"), dns.TypeA)
		r, _, err := client.Exchange(msg, serverAddr)
		require.NoError(t, err)
		require.NotEmpty(t, r.Answer, "должен быть ответ для заблокированного домена")
		require.IsType(t, &dns.A{}, r.Answer[0], "тип записи должен быть A")
		require.Equal(t, net.ParseIP("0.0.0.0").String(), r.Answer[0].(*dns.A).A.String(), "IP-адрес должен быть 0.0.0.0")
	})

	// 4. Очистка
	cancel()
	select {
	case err := <-runErrCh:
		require.ErrorIs(t, err, context.Canceled, "ожидалась ошибка отмены контекста при остановке")
	case <-time.After(10 * time.Second):
		t.Fatal("сервер не остановился вовремя")
	}
}
