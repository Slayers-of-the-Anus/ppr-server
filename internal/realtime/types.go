package realtime

import (
	"sync"
	"time"

	"phonepilot/server/internal/config"
)

type Message map[string]any

type AdminIdentity struct {
	Username        string
	ViewerID        string
	ActiveSessionID string
}

type AdminClient struct {
	Conn        *WSConn
	Username    string
	ViewerID    string
	LastServoAt time.Time
}

type ServoState struct {
	Available     bool
	Angle         int
	TargetAngle   int
	Pending       bool
	Min           int
	Max           int
	Step          int
	LastCommandBy string
	LastCommandAt time.Time
}

type Camera struct {
	ID                    string
	Name                  string
	Conn                  *WSConn
	Available             bool
	Width                 int
	Height                int
	FPS                   int
	LastSeen              time.Time
	DisconnectTimer       *time.Timer
	IntentionalDisconnect bool
	SourceSessionID       string
	Servo                 ServoState
}

type ViewerSession struct {
	SessionID            string
	CameraID             string
	Admin                *AdminClient
	Username             string
	ViewerID             string
	Role                 string
	State                string
	ViewerState          string
	CameraState          string
	MediaState           string
	CreatedAt            time.Time
	LastSeen             time.Time
	NegotiationTimer     *time.Timer
	ViewerGraceTimer     *time.Timer
	PendingCritical      map[string]Message
	PendingCriticalOrder []string
	PendingIce           []Message
	RelaySourceSessionID string
}

type Hub struct {
	cfg      config.Config
	mu       sync.Mutex
	cameras  map[string]*Camera
	admins   map[*AdminClient]struct{}
	sessions map[string]*ViewerSession
	stop     chan struct{}
}
