package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"phonepilot/server/internal/config"
	"phonepilot/server/internal/realtime"
)

func TestLoginAndWSToken(t *testing.T) {
	cfg := config.Config{
		Users:                 []config.User{{Username: "admin", Password: "secret"}},
		MaxViewersPerCamera:   4,
		ServoMinAngle:         0,
		ServoMaxAngle:         180,
		ServoCenterAngle:      90,
		ServoDefaultStep:      5,
		SessionTTL:            time.Hour,
		NegotiationTimeout:    2 * time.Second,
		ViewerSignalingGrace:  2 * time.Second,
		CameraDisconnectGrace: 2 * time.Second,
	}
	hub := realtime.NewHub(cfg)
	defer hub.Close()
	api, err := New(cfg, hub)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Post(ts.URL+"/api/login", "application/json", bytes.NewBufferString(`{"username":"admin","password":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status=%d body=%s", resp.StatusCode, b)
	}

	resp, err = client.Post(ts.URL+"/api/ws-token", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if token, _ := body["token"].(string); len(token) < 20 {
		t.Fatalf("unexpected token: %#v", body["token"])
	}
	if viewer, _ := body["viewerId"].(string); len(viewer) < 12 {
		t.Fatalf("unexpected viewer id: %#v", body["viewerId"])
	}
}
