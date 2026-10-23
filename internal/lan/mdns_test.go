package lan

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

func TestDNSSDAdvertisement(t *testing.T) {
	id := "12345678-1234-4234-8234-123456789abc"
	service := "_phonepilot._tcp.local."
	instance := "phonepilot-" + id + "." + service
	host := "phonepilot-" + id + ".local."
	b := dnsAnswer(service, instance, host, &Binding{IP: net.IPv4(192, 168, 1, 10), Port: 8081})
	if binary.BigEndian.Uint16(b[6:8]) != 4 || !bytes.Contains(b, dnsName(service)) || !bytes.Contains(b, dnsName(instance)) || !bytes.Contains(b, []byte("v=2")) || !bytes.Contains(b, []byte("serverId="+id)) || !bytes.Contains(b, []byte{0x1f, 0x91}) || !bytes.Contains(b, []byte{192, 168, 1, 10}) {
		t.Fatalf("incomplete DNS-SD answer: %x", b)
	}
	query := make([]byte, 12)
	binary.BigEndian.PutUint16(query[4:6], 1)
	query = append(query, dnsName(service)...)
	query = append(query, 0, 12, 0, 1)
	if !dnsQuestionMatches(query, service, instance, host) {
		t.Fatal("service query not recognized")
	}
}
