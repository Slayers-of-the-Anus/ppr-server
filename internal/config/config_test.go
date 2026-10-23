package config

import (
	"encoding/base64"
	"path/filepath"
	"testing"
)

func TestPublicModeRequiresSecretAndStableID(t *testing.T) {
	t.Setenv("PHONEPILOT_SERVER_ID_FILE", filepath.Join(t.TempDir(), "id"))
	t.Setenv("PHONEPILOT_PUBLIC_URL", "https://pilot.example.org")
	t.Setenv("PHONEPILOT_CAMERA_SECRET_B64URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("public mode accepted without camera secret")
	}
	t.Setenv("PHONEPILOT_CAMERA_SECRET_B64URL", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if a.ServerID != b.ServerID || len(a.ServerID) != 36 {
		t.Fatalf("serverId not stable: %q %q", a.ServerID, b.ServerID)
	}
	t.Setenv("PHONEPILOT_MAIN_BIND", "0.0.0.0")
	if _, err := Load(); err == nil {
		t.Fatal("public mode accepted a directly exposed main listener")
	}
}
