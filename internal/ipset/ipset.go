//go:build linux

package ipset

import (
	"errors"
	"fmt"

	I "github.com/crazytypewriter/ipset"
)

type IPSet struct{}

func New() (*IPSet, error) {
	if err := I.Init(); err != nil {
		return nil, fmt.Errorf("ipset Init failed: %w", err)
	}
	return &IPSet{}, nil
}

// create создаёт сет, считая «уже существует» успехом: сеты переживают
// перезапуск процесса, и на роутере это нормальный путь, а не ошибка.
func create(name string, opts ...I.Option) error {
	err := I.Create(name, opts...)
	if err != nil && errors.Is(err, I.ErrExist) {
		return nil
	}
	return err
}

// createOpts собирает опции создания: тип, семейство, timeout по умолчанию
// и maxelem (0 — оставить дефолт ядра, 65536).
func createOpts(typ string, ipv6 bool, timeout, maxElem uint32) []I.Option {
	opts := []I.Option{I.OptTimeout(timeout)}
	if typ != "" {
		opts = append(opts, I.OptType(typ))
	}
	if ipv6 {
		opts = append(opts, I.OptIPv6())
	}
	if maxElem != 0 {
		opts = append(opts, I.OptMaxElem(maxElem))
	}
	return opts
}

func (i *IPSet) CreateIPv4Set(name string, timeout, maxElem uint32) error {
	return create(name, createOpts("", false, timeout, maxElem)...)
}

func (i *IPSet) CreateIPv6Set(name string, timeout, maxElem uint32) error {
	return create(name, createOpts("", true, timeout, maxElem)...)
}

func (i *IPSet) CreateIPv4NetSet(name string, timeout, maxElem uint32) error {
	return create(name, createOpts("hash:net", false, timeout, maxElem)...)
}

func (i *IPSet) CreateIPv6NetSet(name string, timeout, maxElem uint32) error {
	return create(name, createOpts("hash:net", true, timeout, maxElem)...)
}

func (i *IPSet) AddElement(setName, ip string, ttl uint32) error {
	return I.Add(setName, ip, I.OptTimeout(ttl))
}

func (i *IPSet) RemoveElement(setName, ip string) error {
	return I.Del(setName, ip)
}

func (i *IPSet) ListElements(setName string) ([]string, error) {
	entries, err := I.List(setName)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Prefix.IsValid() {
			out = append(out, e.Prefix.String())
			continue
		}
		out = append(out, e.IP.String())
	}
	return out, nil
}
