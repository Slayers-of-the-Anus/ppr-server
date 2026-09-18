package config

import (
	"bufio"
	"fmt"
	"os"
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
		Port: intEnv("PORT", 8080),
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
