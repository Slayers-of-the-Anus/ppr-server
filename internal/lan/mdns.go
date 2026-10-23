package lan

import (
	"encoding/binary"
	"log"
	"net"
	"strings"
	"time"
)

// Advertise answers DNS-SD queries on the selected private interface.
func Advertise(binding *Binding, serverID string, stop <-chan struct{}) error {
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	conn, err := net.ListenMulticastUDP("udp4", binding.Interface, group)
	if err != nil {
		return err
	}
	_ = conn.SetReadBuffer(8192)
	name := "phonepilot-" + serverID
	service := "_phonepilot._tcp.local."
	instance := name + "." + service
	host := name + ".local."
	answer := dnsAnswer(service, instance, host, binding)
	if _, err := conn.WriteToUDP(answer, group); err != nil {
		_ = conn.Close()
		return err
	}
	go func() {
		defer conn.Close()
		// Announce immediately and periodically; respond to service/instance/host queries.
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		buf := make([]byte, 4096)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			n, _, err := conn.ReadFromUDP(buf)
			if err == nil && dnsQuestionMatches(buf[:n], service, instance, host) {
				_, _ = conn.WriteToUDP(answer, group)
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, _ = conn.WriteToUDP(answer, group)
			default:
			}
		}
	}()
	log.Printf("DNS-SD advertising %s on %s", instance, binding.Interface.Name)
	return nil
}

func dnsAnswer(service, instance, host string, binding *Binding) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[2:4], 0x8400)
	binary.BigEndian.PutUint16(b[6:8], 4)
	ptr := dnsName(instance)
	b = dnsRecord(b, service, 12, 1, ptr)
	srv := make([]byte, 6)
	binary.BigEndian.PutUint16(srv[4:], uint16(binding.Port))
	srv = append(srv, dnsName(host)...)
	b = dnsRecord(b, instance, 33, 0x8001, srv)
	txt := []byte("v=2")
	txt2 := []byte("serverId=" + strings.TrimSuffix(strings.TrimPrefix(instance, "phonepilot-"), "."+service))
	data := append([]byte{byte(len(txt))}, txt...)
	data = append(data, byte(len(txt2)))
	data = append(data, txt2...)
	b = dnsRecord(b, instance, 16, 0x8001, data)
	b = dnsRecord(b, host, 1, 0x8001, []byte(binding.IP.To4()))
	return b
}

func dnsName(name string) []byte {
	var out []byte
	for _, part := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		out = append(out, byte(len(part)))
		out = append(out, part...)
	}
	return append(out, 0)
}

func dnsRecord(b []byte, name string, typ, class uint16, data []byte) []byte {
	b = append(b, dnsName(name)...)
	var fixed [10]byte
	binary.BigEndian.PutUint16(fixed[0:2], typ)
	binary.BigEndian.PutUint16(fixed[2:4], class)
	binary.BigEndian.PutUint32(fixed[4:8], 120)
	binary.BigEndian.PutUint16(fixed[8:10], uint16(len(data)))
	b = append(b, fixed[:]...)
	return append(b, data...)
}

func dnsQuestionMatches(packet []byte, names ...string) bool {
	if len(packet) < 12 || packet[2]&0x80 != 0 {
		return false
	}
	n := int(binary.BigEndian.Uint16(packet[4:6]))
	off := 12
	for i := 0; i < n && i < 64; i++ {
		name, next, ok := readDNSName(packet, off)
		if !ok || next+4 > len(packet) {
			return false
		}
		off = next + 4
		for _, candidate := range names {
			if strings.EqualFold(name, candidate) {
				return true
			}
		}
	}
	return false
}

func readDNSName(packet []byte, off int) (string, int, bool) {
	var labels []string
	next := -1
	for depth := 0; depth < 32; depth++ {
		if off >= len(packet) {
			return "", 0, false
		}
		length := int(packet[off])
		off++
		if length == 0 {
			if next < 0 {
				next = off
			}
			return strings.Join(labels, ".") + ".", next, true
		}
		if length&0xc0 == 0xc0 {
			if off >= len(packet) {
				return "", 0, false
			}
			if next < 0 {
				next = off + 1
			}
			off = (length&0x3f)<<8 | int(packet[off])
			continue
		}
		if length > 63 || off+length > len(packet) {
			return "", 0, false
		}
		labels = append(labels, string(packet[off:off+length]))
		off += length
	}
	return "", 0, false
}
