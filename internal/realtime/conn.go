package realtime

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"phonepilot/server/internal/ws"
)

type WSConn struct {
	conn   *ws.Conn
	write  sync.Mutex
	closed atomic.Bool
	alive  atomic.Bool
}

func NewWSConn(conn *ws.Conn) *WSConn {
	c := &WSConn{conn: conn}
	c.alive.Store(true)
	conn.SetPongHandler(func(string) error {
		c.alive.Store(true)
		return nil
	})
	return c
}

func (c *WSConn) ReadJSON(v any) error {
	return c.conn.ReadJSON(v)
}

func (c *WSConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

func (c *WSConn) WriteJSON(v any) bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.write.Lock()
	defer c.write.Unlock()
	if c.closed.Load() {
		return false
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.conn.WriteJSON(v); err != nil {
		return false
	}
	return true
}

func (c *WSConn) WriteRawJSON(raw []byte) bool {
	if c == nil || c.closed.Load() || !json.Valid(raw) {
		return false
	}
	c.write.Lock()
	defer c.write.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteMessage(ws.TextMessage, raw) == nil
}

func (c *WSConn) Ping() bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.write.Lock()
	defer c.write.Unlock()
	return c.conn.WriteControl(ws.PingMessage, nil, time.Now().Add(3*time.Second)) == nil
}

func (c *WSConn) MarkForPing() bool {
	return c.alive.Swap(false)
}

func (c *WSConn) MarkAlive()     { c.alive.Store(true) }
func (c *WSConn) IsClosed() bool { return c == nil || c.closed.Load() }

func (c *WSConn) Close(code int, reason string) {
	if c == nil || !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.write.Lock()
	defer c.write.Unlock()
	_ = c.conn.WriteControl(ws.CloseMessage, ws.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = c.conn.Close()
}

func (c *WSConn) Underlying() *ws.Conn { return c.conn }
