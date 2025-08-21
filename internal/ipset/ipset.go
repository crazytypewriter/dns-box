//go:build linux

package ipset

import (
	"fmt"
	I "github.com/crazytypewriter/ipset"
)

type IPSet struct{}

func New() *IPSet {
	if err := I.Init(); err != nil {
		panic(fmt.Sprintf("ipset Init failed: %v", err))
	}
	return &IPSet{}
}

func (i *IPSet) CreateIPv4Set(name string, timeout uint32) error {
	return I.Create(name, I.OptTimeout(timeout))
}

func (i *IPSet) CreateIPv6Set(name string, timeout uint32) error {
	return I.Create(name, I.OptIPv6(), I.OptTimeout(timeout))
}

func (i *IPSet) AddElement(setName, ip string, ttl uint32) error {
	return I.Add(setName, ip, I.OptTimeout(ttl))
}
