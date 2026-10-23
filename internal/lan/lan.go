package lan

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

type Binding struct {
	Address   string
	IP        net.IP
	Subnet    *net.IPNet
	Interface *net.Interface
	Port      int
}

func Resolve(spec string) (*Binding, error) {
	if spec == "" || strings.EqualFold(spec, "off") {
		return nil, nil
	}
	host, portText, err := net.SplitHostPort(spec)
	if err != nil {
		return nil, fmt.Errorf("PHONEPILOT_LAN_BIND: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("PHONEPILOT_LAN_BIND: invalid port")
	}
	if host == "auto" {
		conn, err := net.Dial("udp4", "1.1.1.1:53")
		if err != nil {
			return nil, fmt.Errorf("PHONEPILOT_LAN_BIND auto: no primary IPv4 route: %w", err)
		}
		host = conn.LocalAddr().(*net.UDPAddr).IP.String()
		_ = conn.Close()
	}
	ip := net.ParseIP(host).To4()
	if ip == nil || !ip.IsPrivate() || ip.IsLoopback() {
		return nil, fmt.Errorf("PHONEPILOT_LAN_BIND: %q is not a private interface IPv4 address", host)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var matches []*Binding
	for i := range interfaces {
		iface := &interfaces[i]
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && ipNet.IP.To4() != nil && ipNet.IP.Equal(ip) {
				matches = append(matches, &Binding{Address: net.JoinHostPort(host, portText), IP: ip, Subnet: &net.IPNet{IP: ip.Mask(ipNet.Mask), Mask: ipNet.Mask}, Interface: iface, Port: port})
			}
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("PHONEPILOT_LAN_BIND: address %s matches %d active interfaces; LAN disabled", host, len(matches))
	}
	return matches[0], nil
}
