package netif

import "net"

func interfaceNames() (map[int]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make(map[int]string, len(interfaces))
	for _, iface := range interfaces {
		result[iface.Index] = iface.Name
	}
	return result, nil
}
