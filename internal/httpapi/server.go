package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"phonepilot/server/internal/config"
	"phonepilot/server/internal/realtime"
	"phonepilot/server/internal/webassets"
	"phonepilot/server/internal/ws"
)

type sessionRecord struct {
	Username  string
	ExpiresAt time.Time
}

type tokenRecord struct {
	Identity  realtime.AdminIdentity
	ExpiresAt time.Time
}

type Server struct {
	cfg      config.Config
	hub      *realtime.Hub
	mux      *http.ServeMux
	web      fs.FS
	mu       sync.Mutex
	sessions map[string]sessionRecord
	tokens   map[string]tokenRecord
}

type lanContextKey struct{}
type lanContext struct{ subnet *net.IPNet }

func New(cfg config.Config, hub *realtime.Hub) (*Server, error) {
	web, err := fs.Sub(webassets.FS, "web")
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, hub: hub, mux: http.NewServeMux(), web: web,
		sessions: map[string]sessionRecord{}, tokens: map[string]tokenRecord{},
	}
	s.routes()
	go s.cleanupLoop()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.securityHeaders(s.mux) }

func (s *Server) LANHandler(subnet *net.IPNet) http.Handler {
	base := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), lanContextKey{}, lanContext{subnet})))
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.handleRoot)
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("GET /admin", s.handleAdminPage)
	s.mux.HandleFunc("GET /camera-test", s.handleCameraTestPage)
	s.mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(s.web)))
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.requireAdmin(s.handleLogout))
	s.mux.HandleFunc("GET /api/me", s.requireAdmin(s.handleMe))
	s.mux.HandleFunc("GET /api/webrtc-config", s.requireAdmin(s.handleWebRTCConfig))
	s.mux.HandleFunc("POST /api/ws-token", s.requireAdmin(s.handleWSToken))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]any{"ok": true}) })
	s.mux.HandleFunc("GET /api/network/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"v": 2, "serverId": s.cfg.ServerID})
	})
	s.mux.HandleFunc("GET /api/network/lan-probe", s.handleLANProbe)
	s.mux.HandleFunc("GET /ws/admin", s.handleAdminWS)
	s.mux.HandleFunc("GET /ws/camera", s.handleCameraWS)
	s.mux.HandleFunc("GET /ws/robot", s.handleCameraWS)
}

func (s *Server) handleLANProbe(w http.ResponseWriter, r *http.Request) {
	if !verifiedLANPeer(r) {
		http.NotFound(w, r)
		return
	}
	nonce := r.URL.Query().Get("nonce")
	if len(nonce) < 16 || len(nonce) > 128 {
		http.Error(w, "invalid nonce", http.StatusBadRequest)
		return
	}
	if _, err := base64.RawURLEncoding.DecodeString(nonce); err != nil || strings.Contains(nonce, "=") {
		http.Error(w, "invalid nonce", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"v": 2, "serverId": s.cfg.ServerID, "nonce": nonce})
}

func verifiedLANPeer(r *http.Request) bool {
	lan, ok := r.Context().Value(lanContextKey{}).(lanContext)
	if !ok || lan.subnet == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() != nil && lan.subnet.Contains(ip)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/admin", http.StatusFound)
	} else {
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	s.serveWebFile(w, r, "login.html")
}
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.serveWebFile(w, r, "admin.html")
}
func (s *Server) handleCameraTestPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.EnableBrowserCameraTest {
		http.NotFound(w, r)
		return
	}
	s.serveWebFile(w, r, "camera-test.html")
}

func (s *Server) serveWebFile(w http.ResponseWriter, r *http.Request, name string) {
	b, err := fs.ReadFile(s.web, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, w, &req); err != nil {
		return
	}
	var canonical string
	for _, u := range s.cfg.Users {
		if strings.EqualFold(strings.TrimSpace(req.Username), u.Username) && constantEqual(req.Password, u.Password) {
			canonical = u.Username
			break
		}
	}
	if canonical == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "INVALID_CREDENTIALS"})
		return
	}
	sid, err := randomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(s.cfg.SessionTTL)
	s.mu.Lock()
	s.sessions[sid] = sessionRecord{Username: canonical, ExpiresAt: expires}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "phonepilot_session", Value: sid, Path: "/", HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: int(s.cfg.SessionTTL.Seconds())})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": canonical})
}
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("phonepilot_session"); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "phonepilot_session", Path: "/", Value: "", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	writeJSON(w, http.StatusOK, map[string]any{"username": u})
}
func (s *Server) handleWebRTCConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"iceServers": s.cfg.IceServers, "maxViewersPerCamera": s.cfg.MaxViewersPerCamera, "topology": "browser-relay"})
}
func (s *Server) handleWSToken(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	var req struct {
		ViewerID        string `json:"viewerId"`
		ActiveSessionID string `json:"activeSessionId"`
	}
	if err := decodeJSON(r, w, &req); err != nil {
		return
	}
	viewer := req.ViewerID
	if !validOpaqueID(viewer) {
		var err error
		viewer, err = randomToken(16)
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
	}
	active := ""
	if validOpaqueID(req.ActiveSessionID) {
		active = req.ActiveSessionID
	}
	token, err := randomToken(28)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	s.mu.Lock()
	s.tokens[token] = tokenRecord{Identity: realtime.AdminIdentity{Username: u, ViewerID: viewer, ActiveSessionID: active}, ExpiresAt: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "viewerId": viewer})
}

func (s *Server) handleAdminWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	s.mu.Lock()
	record, ok := s.tokens[token]
	if ok {
		delete(s.tokens, token)
	}
	s.mu.Unlock()
	if !ok || time.Now().After(record.ExpiresAt) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		return
	}
	s.hub.HandleAdmin(realtime.NewWSConn(conn), record.Identity)
}
func (s *Server) handleCameraWS(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.CameraSecret) == 0 && !verifiedLANPeer(r) && !loopbackPeer(r) {
		http.Error(w, "camera authentication required outside local network", http.StatusForbidden)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("cameraId"))
	if id == "" {
		id = strings.TrimSpace(r.URL.Query().Get("robotId"))
	}
	if !validCameraID(id) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = id
	}
	name = truncateRunes(name, 80)
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		return
	}
	wrapped := realtime.NewWSConn(conn)
	if len(s.cfg.CameraSecret) == 32 && !authenticateCamera(wrapped, id, s.cfg.CameraSecret) {
		wrapped.Close(4003, "camera-auth-failed")
		return
	}
	s.hub.HandleCamera(wrapped, id, name, verifiedLANPeer(r))
}

func loopbackPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authenticateCamera(conn *realtime.WSConn, id string, secret []byte) bool {
	nonce, err := randomToken(16)
	if err != nil || !conn.WriteJSON(map[string]any{"type": "camera_auth_challenge", "v": 2, "nonce": nonce}) {
		return false
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	var msg struct {
		Type     string `json:"type"`
		V        int    `json:"v"`
		CameraID string `json:"cameraId"`
		Proof    string `json:"proof"`
	}
	if err := conn.ReadJSON(&msg); err != nil || msg.Type != "camera_auth" || msg.V != 2 || msg.CameraID != id || strings.Contains(msg.Proof, "=") {
		return false
	}
	proof, err := base64.RawURLEncoding.DecodeString(msg.Proof)
	if err != nil || len(proof) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(nonce + "\n" + id))
	return hmac.Equal(proof, mac.Sum(nil))
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentUser(r); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "AUTH_REQUIRED"})
			return
		}
		next(w, r)
	}
}
func (s *Server) currentUser(r *http.Request) (string, bool) {
	c, err := r.Cookie("phonepilot_session")
	if err != nil || c.Value == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sessions[c.Value]
	if !ok || time.Now().After(rec.ExpiresAt) {
		if ok {
			delete(s.sessions, c.Value)
		}
		return "", false
	}
	return rec.Username, true
}

func (s *Server) cleanupLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		s.mu.Lock()
		for k, v := range s.sessions {
			if now.After(v.ExpiresAt) {
				delete(s.sessions, k)
			}
		}
		for k, v := range s.tokens {
			if now.After(v.ExpiresAt) {
				delete(s.tokens, k)
			}
		}
		s.mu.Unlock()
	}
}
func decodeJSON(r *http.Request, w http.ResponseWriter, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "INVALID_JSON"})
		return err
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func constantEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func validOpaqueID(v string) bool {
	if len(v) < 12 || len(v) > 96 {
		return false
	}
	for _, r := range v {
		if !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}
func validCameraID(v string) bool {
	if len(v) < 2 || len(v) > 40 {
		return false
	}
	return validIdentifierChars(v)
}
func validIdentifierChars(v string) bool {
	for _, r := range v {
		if !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
