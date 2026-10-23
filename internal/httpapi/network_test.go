package httpapi

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"phonepilot/server/internal/config"
	"phonepilot/server/internal/realtime"
)

type testSocket struct {
	net.Conn
	r *bufio.Reader
}

func dialTestSocket(t *testing.T, url string) *testSocket {
	t.Helper()
	host := strings.TrimPrefix(url, "http://")
	c, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	s := &testSocket{Conn: c, r: bufio.NewReader(c)}
	_, err = fmt.Fprintf(c, "GET /ws/camera?cameraId=phone_01 HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: MDEyMzQ1Njc4OWFiY2RlZg==\r\n\r\n", host)
	if err != nil {
		t.Fatal(err)
	}
	line, err := s.r.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatalf("upgrade: %s %v", line, err)
	}
	for {
		line, err = s.r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	return s
}

func (s *testSocket) read(t *testing.T) map[string]any {
	t.Helper()
	_ = s.SetReadDeadline(time.Now().Add(3 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(s.r, head[:]); err != nil {
		t.Fatal(err)
	}
	if head[0]&15 != 1 {
		t.Fatalf("unexpected WebSocket opcode %d", head[0]&15)
	}
	n := int(head[1] & 127)
	if n == 126 {
		var b [2]byte
		if _, err := io.ReadFull(s.r, b[:]); err != nil {
			t.Fatal(err)
		}
		n = int(binary.BigEndian.Uint16(b[:]))
	}
	if n == 127 {
		t.Fatal("oversized test frame")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(s.r, b); err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func (s *testSocket) write(t *testing.T, msg any) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte{0x81}
	if len(b) < 126 {
		frame = append(frame, byte(len(b))|0x80)
	} else {
		frame = append(frame, 0xfe, byte(len(b)>>8), byte(len(b)))
	}
	mask := []byte{1, 2, 3, 4}
	frame = append(frame, mask...)
	for i := range b {
		frame = append(frame, b[i]^mask[i%4])
	}
	if _, err := s.Conn.Write(frame); err != nil {
		t.Fatal(err)
	}
}

func proof(secret []byte, nonce, id string) string {
	m := hmac.New(sha256.New, secret)
	_, _ = m.Write([]byte(nonce + "\n" + id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func TestCameraAuthAndNetworkPath(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	cfg := config.Config{CameraSecret: secret, ServerID: "12345678-1234-4234-8234-123456789abc", CameraDisconnectGrace: time.Second, MaxViewersPerCamera: 4, ServoCenterAngle: 90, ServoMaxAngle: 180, ServoDefaultStep: 5}
	hub := realtime.NewHub(cfg)
	defer hub.Close()
	api, err := New(cfg, hub)
	if err != nil {
		t.Fatal(err)
	}
	main := httptest.NewServer(api.Handler())
	defer main.Close()
	_, subnet, _ := net.ParseCIDR("127.0.0.0/8")
	local := httptest.NewServer(api.LANHandler(subnet))
	defer local.Close()

	probe, err := http.Get(main.URL + "/api/network/lan-probe?nonce=MTIzNDU2Nzg5MDEyMzQ1Ng")
	if err != nil {
		t.Fatal(err)
	}
	probe.Body.Close()
	if probe.StatusCode != 404 {
		t.Fatalf("public probe status %d", probe.StatusCode)
	}
	probe, err = http.Get(local.URL + "/api/network/lan-probe?nonce=MTIzNDU2Nzg5MDEyMzQ1Ng")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(probe.Body).Decode(&body)
	if body["serverId"] != cfg.ServerID || body["nonce"] != "MTIzNDU2Nzg5MDEyMzQ1Ng" {
		t.Fatalf("probe=%v", body)
	}

	s := dialTestSocket(t, local.URL)
	challenge := s.read(t)
	if challenge["type"] != "camera_auth_challenge" || challenge["v"] != float64(2) {
		t.Fatalf("challenge=%v", challenge)
	}
	nonce := challenge["nonce"].(string)
	s.write(t, map[string]any{"type": "camera_auth", "v": 2, "cameraId": "phone_01", "proof": proof(secret, nonce, "phone_01")})
	if m := s.read(t); m["type"] != "camera_ready" {
		t.Fatalf("ready=%v", m)
	}
	_ = s.read(t) // legacy robot_ready
	_ = s.read(t) // viewer_list
	s.write(t, map[string]any{"type": "network_state", "v": 2, "transport": "wifi"})
	if m := s.read(t); m["type"] != "network_path" || m["path"] != "lan_wifi" || m["verified"] != true {
		t.Fatalf("path=%v", m)
	}
	if path := hub.CameraSnapshot("phone_01")["networkPath"].(realtime.NetworkPath); path.Path != "lan_wifi" {
		t.Fatalf("snapshot=%+v", path)
	}
	_ = s.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshot := hub.CameraSnapshot("phone_01")
		if snapshot != nil && snapshot["networkPath"].(realtime.NetworkPath).Path == "offline" {
			if snapshot["signaling"] != "grace" {
				t.Fatalf("signaling=%v", snapshot["signaling"])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("network path did not immediately become offline")
}

func TestCameraAuthRejectsWrongAndReplayedProof(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	cfg := config.Config{CameraSecret: secret, CameraDisconnectGrace: time.Second}
	hub := realtime.NewHub(cfg)
	defer hub.Close()
	api, _ := New(cfg, hub)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	first := dialTestSocket(t, srv.URL)
	nonce := first.read(t)["nonce"].(string)
	first.write(t, map[string]any{"type": "camera_auth", "v": 2, "cameraId": "phone_01", "proof": proof(secret, nonce, "wrong_id")})
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	var b [2]byte
	_, err := io.ReadFull(first.r, b[:])
	if err != nil || b[0]&15 != 8 {
		t.Fatalf("wrong proof not rejected: %v %v", b, err)
	}
	_ = first.Close()
	second := dialTestSocket(t, srv.URL)
	_ = second.read(t)
	second.write(t, map[string]any{"type": "camera_auth", "v": 2, "cameraId": "phone_01", "proof": proof(secret, nonce, "phone_01")})
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.ReadFull(second.r, b[:])
	if err != nil || b[0]&15 != 8 {
		t.Fatalf("replayed proof not rejected: %v %v", b, err)
	}
	_ = second.Close()
	if hub.CameraSnapshot("phone_01") != nil {
		t.Fatal("unauthenticated camera registered")
	}
}

func TestLANPeerUsesSocketAddress(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("192.168.1.0/24")
	r := httptest.NewRequest(http.MethodGet, "/api/network/lan-probe", nil)
	r = r.WithContext(context.WithValue(r.Context(), lanContextKey{}, lanContext{subnet}))
	r.RemoteAddr = "203.0.113.4:50000"
	r.Header.Set("X-Forwarded-For", "192.168.1.20")
	if verifiedLANPeer(r) {
		t.Fatal("trusted forwarded address instead of peer socket")
	}
	r.RemoteAddr = "192.168.1.20:50000"
	if !verifiedLANPeer(r) {
		t.Fatal("rejected actual LAN peer")
	}
}
