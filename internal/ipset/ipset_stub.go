//go:build !linux

package ipset

type IPSet struct{}

func New() *IPSet {
	return &IPSet{}
}

func (i *IPSet) CreateIPv4Set(name string, timeout uint32) error {
	return nil
}

func (i *IPSet) CreateIPv6Set(name string, timeout uint32) error {
	return nil
}

func (i *IPSet) CreateIPv4NetSet(name string, timeout uint32) error {
	return nil
}

func (i *IPSet) CreateIPv6NetSet(name string, timeout uint32) error {
	return nil
}

func (i *IPSet) RemoveElement(setName, ip string) error {
	return nil
}

func (i *IPSet) AddElement(setName, ip string, ttl uint32) error {
	return nil
}
