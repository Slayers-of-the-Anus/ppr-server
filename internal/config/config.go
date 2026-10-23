package config

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type User struct {
	Username string
	Password string
}

type IceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type Config struct {
	Port                    int
	MainBind                string
	LANBind                 string
	PublicURL               string
	CameraSecret            []byte
	ServerID                string
	Users                   []User
	IceServers              []IceServer
	EnableBrowserCameraTest bool
	CameraDisconnectGrace   time.Duration
	ViewerSignalingGrace    time.Duration
	NegotiationTimeout      time.Duration
	MaxViewersPerCamera     int
	ServoMinAngle           int
	ServoMaxAngle           int
	ServoCenterAngle        int
	ServoDefaultStep        int
	ServoCommandInterval    time.Duration
	CookieSecure            bool
	SessionTTL              time.Duration
}

func Load() (Config, error) {
	_ = loadDotEnv(".env")

	minAngle := clamp(intEnv("SERVO_MIN_ANGLE", 0), 0, 179)
	maxAngle := clamp(intEnv("SERVO_MAX_ANGLE", 180), minAngle+1, 360)

	cfg := Config{
		Port:      intEnv("PORT", 8080),
		MainBind:  env("PHONEPILOT_MAIN_BIND", "127.0.0.1"),
		LANBind:   env("PHONEPILOT_LAN_BIND", "auto:8081"),
		PublicURL: strings.TrimSpace(os.Getenv("PHONEPILOT_PUBLIC_URL")),
		Users: []User{
			{Username: env("ADMIN_LOGIN", "admin"), Password: env("ADMIN_PASSWORD", "ChangeMe123!")},
			{Username: "Ivan", Password: env("IVAN_PASSWORD", "Ivan123!")},
			{Username: "sasha", Password: env("SASHA_PASSWORD", "Sasha123!")},
			{Username: "yehor", Password: env("YEHOR_PASSWORD", "Yehor123!")},
		},
		EnableBrowserCameraTest: boolEnv("ENABLE_BROWSER_CAMERA_TEST", true),
		CameraDisconnectGrace:   durationMs("CAMERA_DISCONNECT_GRACE_MS", 15_000),
		ViewerSignalingGrace:    durationMs("VIEWER_SIGNALING_GRACE_MS", 15_000),
		NegotiationTimeout:      durationMs("WEBRTC_NEGOTIATION_TIMEOUT_MS", 20_000),
		MaxViewersPerCamera:     clamp(intEnv("MAX_VIEWERS_PER_CAMERA", 4), 1, 8),
		ServoMinAngle:           minAngle,
		ServoMaxAngle:           maxAngle,
		ServoCenterAngle:        (minAngle + maxAngle) / 2,
		ServoDefaultStep:        clamp(intEnv("SERVO_STEP", 5), 1, 45),
		ServoCommandInterval:    durationMsAllowZero("SERVO_COMMAND_INTERVAL_MS", 80),
		CookieSecure:            boolEnv("COOKIE_SECURE", false),
		SessionTTL:              8 * time.Hour,
	}

	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("PORT must be between 1 and 65535")
	}
	mainIP := net.ParseIP(cfg.MainBind)
	if mainIP == nil {
		return Config{}, fmt.Errorf("PHONEPILOT_MAIN_BIND must be an IPv4 or IPv6 address")
	}
	secretText := strings.TrimSpace(os.Getenv("PHONEPILOT_CAMERA_SECRET_B64URL"))
	if secretText != "" {
		if strings.Contains(secretText, "=") {
			return Config{}, fmt.Errorf("PHONEPILOT_CAMERA_SECRET_B64URL must be unpadded Base64URL")
		}
		var err error
		cfg.CameraSecret, err = base64.RawURLEncoding.DecodeString(secretText)
		if err != nil || len(cfg.CameraSecret) != 32 {
			return Config{}, fmt.Errorf("PHONEPILOT_CAMERA_SECRET_B64URL must encode exactly 32 bytes")
		}
	}
	if cfg.PublicURL != "" {
		if !mainIP.IsLoopback() {
			return Config{}, fmt.Errorf("public mode requires loopback PHONEPILOT_MAIN_BIND behind a restricted TLS reverse proxy")
		}
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || net.ParseIP(u.Hostname()) != nil || strings.EqualFold(u.Hostname(), "localhost") {
			return Config{}, fmt.Errorf("PHONEPILOT_PUBLIC_URL must be an external HTTPS origin")
		}
		if len(cfg.CameraSecret) != 32 {
			return Config{}, fmt.Errorf("PHONEPILOT_PUBLIC_URL requires PHONEPILOT_CAMERA_SECRET_B64URL")
		}
	}
	var err error
	cfg.ServerID, err = loadServerID(env("PHONEPILOT_SERVER_ID_FILE", filepath.Join("data", "server-id")))
	if err != nil {
		return Config{}, err
	}

	stun := csv(env("STUN_URLS", "stun:stun.l.google.com:19302"))
	if len(stun) > 0 {
		cfg.IceServers = append(cfg.IceServers, IceServer{URLs: stun})
	}
	turn := csv(os.Getenv("TURN_URLS"))
	if len(turn) > 0 {
		cfg.IceServers = append(cfg.IceServers, IceServer{
			URLs:       turn,
			Username:   os.Getenv("TURN_USERNAME"),
			Credential: os.Getenv("TURN_CREDENTIAL"),
		})
	}

	return cfg, nil
}

func loadServerID(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(b))
		if len(id) == 36 && id[8] == '-' && id[13] == '-' && id[18] == '-' && id[23] == '-' && id[14] == '4' {
			return id, nil
		}
		return "", fmt.Errorf("invalid serverId in %s", path)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return loadServerID(path)
	}
	if err != nil {
		return "", err
	}
	if _, err = f.WriteString(id + "\n"); err != nil {
		_ = f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return id, nil
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				_ = os.Setenv(key, value)
			}
		}
	}
	return s.Err()
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func boolEnv(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func durationMs(key string, fallback int) time.Duration {
	n := intEnv(key, fallback)
	if n <= 0 {
		n = fallback
	}
	return time.Duration(n) * time.Millisecond
}

func durationMsAllowZero(key string, fallback int) time.Duration {
	n := intEnv(key, fallback)
	if n < 0 {
		n = fallback
	}
	return time.Duration(n) * time.Millisecond
}

func csv(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, item := range parts {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
