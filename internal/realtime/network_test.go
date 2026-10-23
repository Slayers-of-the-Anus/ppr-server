package realtime

import "testing"

func TestClassifyPath(t *testing.T) {
	for _, tc := range []struct {
		lan             bool
		transport, want string
	}{
		{true, "wifi", "lan_wifi"}, {true, "cellular", "unknown"},
		{false, "cellular", "mobile"}, {false, "wifi", "remote_wifi"},
		{false, "other", "unknown"}, {false, "", "unknown"},
	} {
		if got := classifyPath(tc.lan, tc.transport); got != tc.want {
			t.Errorf("lan=%v transport=%s: %s, want %s", tc.lan, tc.transport, got, tc.want)
		}
	}
}
