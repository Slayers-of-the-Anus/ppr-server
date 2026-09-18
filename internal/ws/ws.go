package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	TextMessage   = 1
	BinaryMessage = 2
	CloseMessage  = 8
	PingMessage   = 9
	PongMessage   = 10

	CloseNormalClosure = 1000
	CloseGoingAway     = 1001
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type Conn struct {
	conn        net.Conn
	reader      *bufio.Reader
	writer      *bufio.Writer
	readLimit   int64
	pongHandler func(string) error
	writeMu     sync.Mutex
}

func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if r.Method != http.MethodGet {
		http.Error(w, "websocket requires GET", http.StatusMethodNotAllowed)
		return nil, errors.New("websocket: method must be GET")
	}
	if !headerContainsToken(r.Header, "Connection", "upgrade") || !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return nil, errors.New("websocket: missing upgrade headers")
	}
	if strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("websocket: unsupported version")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decoded) != 16 {
		http.Error(w, "bad websocket key", http.StatusBadRequest)
		return nil, errors.New("websocket: invalid key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusInternalServerError)
		return nil, errors.New("websocket: response writer does not support hijacking")
	}
	nc, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	acceptHash := sha1.Sum([]byte(key + websocketGUID))
	accept := base64.StdEncoding.EncodeToString(acceptHash[:])
	if _, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
		_ = nc.Close()
		return nil, err
	}
	if err = rw.Flush(); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return &Conn{conn: nc, reader: rw.Reader, writer: rw.Writer, readLimit: 256 * 1024}, nil
}

func (c *Conn) SetReadLimit(n int64)                 { c.readLimit = n }
func (c *Conn) SetPongHandler(fn func(string) error) { c.pongHandler = fn }
func (c *Conn) SetWriteDeadline(t time.Time) error   { return c.conn.SetWriteDeadline(t) }
func (c *Conn) Close() error                         { return c.conn.Close() }

func (c *Conn) ReadJSON(v any) error {
	typ, payload, err := c.ReadMessage()
	if err != nil {
		return err
	}
	if typ != TextMessage {
		return errors.New("websocket: expected text message")
	}
	return json.Unmarshal(payload, v)
}

func (c *Conn) WriteJSON(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteMessage(TextMessage, payload)
}

func (c *Conn) ReadMessage() (int, []byte, error) {
	var messageType int
	var message []byte
	fragmented := false

	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch opcode {
		case PingMessage:
			_ = c.WriteControl(PongMessage, payload, time.Now().Add(3*time.Second))
			continue
		case PongMessage:
			if c.pongHandler != nil {
				if err := c.pongHandler(string(payload)); err != nil {
					return 0, nil, err
				}
			}
			continue
		case CloseMessage:
			return 0, nil, io.EOF
		case 0:
			if !fragmented {
				return 0, nil, errors.New("websocket: unexpected continuation frame")
			}
			message = append(message, payload...)
		case TextMessage, BinaryMessage:
			if fragmented {
				return 0, nil, errors.New("websocket: new data frame during fragmented message")
			}
			messageType = opcode
			message = append(message, payload...)
			fragmented = !fin
		default:
			return 0, nil, errors.New("websocket: unsupported opcode")
		}

		if c.readLimit > 0 && int64(len(message)) > c.readLimit {
			return 0, nil, errors.New("websocket: message exceeds read limit")
		}
		if (opcode == 0 && fin) || ((opcode == TextMessage || opcode == BinaryMessage) && fin) {
			return messageType, message, nil
		}
		if opcode == 0 && !fin {
			fragmented = true
		}
	}
}

func (c *Conn) readFrame() (bool, int, []byte, error) {
	h, err := c.reader.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}
	h2, err := c.reader.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}

	fin := h&0x80 != 0
	opcode := int(h & 0x0F)
	if h&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: RSV bits set")
	}
	masked := h2&0x80 != 0
	if !masked {
		return false, 0, nil, errors.New("websocket: client frame must be masked")
	}

	length := uint64(h2 & 0x7F)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.reader, b[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.reader, b[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(b[:])
		if length&(1<<63) != 0 {
			return false, 0, nil, errors.New("websocket: invalid frame length")
		}
	}
	if opcode >= 8 && (!fin || length > 125) {
		return false, 0, nil, errors.New("websocket: invalid control frame")
	}
	if c.readLimit > 0 && int64(length) > c.readLimit {
		return false, 0, nil, errors.New("websocket: frame exceeds read limit")
	}

	var mask [4]byte
	if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, opcode, payload, nil
}

func (c *Conn) WriteMessage(messageType int, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeFrame(true, messageType, payload)
}

func (c *Conn) WriteControl(messageType int, payload []byte, deadline time.Time) error {
	if len(payload) > 125 {
		return errors.New("websocket: control payload too large")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return c.writeFrame(true, messageType, payload)
}

func (c *Conn) writeFrame(fin bool, opcode int, payload []byte) error {
	first := byte(opcode)
	if fin {
		first |= 0x80
	}
	if err := c.writer.WriteByte(first); err != nil {
		return err
	}
	length := len(payload)
	switch {
	case length <= 125:
		if err := c.writer.WriteByte(byte(length)); err != nil {
			return err
		}
	case length <= 65535:
		if err := c.writer.WriteByte(126); err != nil {
			return err
		}
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(length))
		if _, err := c.writer.Write(b[:]); err != nil {
			return err
		}
	default:
		if err := c.writer.WriteByte(127); err != nil {
			return err
		}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(length))
		if _, err := c.writer.Write(b[:]); err != nil {
			return err
		}
	}
	if _, err := c.writer.Write(payload); err != nil {
		return err
	}
	return c.writer.Flush()
}

func FormatCloseMessage(code int, text string) []byte {
	if code == 0 {
		return nil
	}
	if len(text) > 123 {
		text = text[:123]
	}
	payload := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], text)
	return payload
}

func headerContainsToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
