package ipset

// Manager — абстракция над ipset для тестируемости: *IPSet (linux и stub)
// реализует её, в тестах подменяется фейком, записывающим вызовы.
type Manager interface {
	CreateIPv4Set(name string, timeout, maxElem uint32) error
	CreateIPv6Set(name string, timeout, maxElem uint32) error
	CreateIPv4NetSet(name string, timeout, maxElem uint32) error
	CreateIPv6NetSet(name string, timeout, maxElem uint32) error
	AddElement(setName, ip string, ttl uint32) error
	RemoveElement(setName, ip string) error
	// ListElements возвращает записи, реально лежащие в сете. Нужен при
	// восстановлении после рестарта: сет мог пережить процесс, и его
	// таймеры (в том числе продлённые правилом -j SET --exist) точнее,
	// чем файл состояния.
	ListElements(setName string) ([]string, error)
}
