// Package realtime is the WebSocket fan-out for the inbox.
//
// One Hub holds every connected dashboard client, indexed by tenant id, so
// when a new message lands in Mongo we can push it to exactly the people
// who care (and not strangers in another workspace).
//
// Connections come in via /ws on the public router; auth is JWT-in-query
// because browsers can't set headers on the WebSocket constructor.
package realtime

import (
	"encoding/json"
	"log"
	"sync"

	ws "github.com/gofiber/websocket/v2"
)

// Client is one connected dashboard tab. Outbound writes go through `send`
// so the broadcast loop never blocks on a slow socket.
type Client struct {
	Conn     *ws.Conn
	TenantID string
	UserID   string
	send     chan []byte
	closed   chan struct{}
}

// Hub fans out messages to every Client subscribed to a given tenant id.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]map[*Client]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: map[string]map[*Client]struct{}{}}
}

// Add registers a client. Idempotent.
func (h *Hub) Add(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[c.TenantID] == nil {
		h.clients[c.TenantID] = map[*Client]struct{}{}
	}
	h.clients[c.TenantID][c] = struct{}{}
}

// Remove unregisters a client and closes its send channel so the writer
// goroutine exits cleanly.
func (h *Hub) Remove(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.clients[c.TenantID]; ok {
		if _, has := set[c]; has {
			delete(set, c)
			close(c.send)
		}
		if len(set) == 0 {
			delete(h.clients, c.TenantID)
		}
	}
}

// Broadcast marshals `payload` once and pushes it onto every subscriber's
// send channel for the given tenant. If a client's channel is full (slow
// reader), we drop the message rather than block the broadcaster — the
// inbox is forgiving of occasional misses; polling via REST is the safety
// net.
func (h *Hub) Broadcast(tenantID string, payload any) {
	msg, err := json.Marshal(payload)
	if err != nil {
		log.Printf("realtime: marshal: %v", err)
		return
	}
	h.mu.RLock()
	set := h.clients[tenantID]
	targets := make([]*Client, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	for _, c := range targets {
		select {
		case c.send <- msg:
		default:
			log.Printf("realtime: drop slow client tenant=%s", c.TenantID)
		}
	}
}

// NewClient is the constructor used by the WebSocket handler.
func NewClient(conn *ws.Conn, tenantID, userID string) *Client {
	return &Client{
		Conn:     conn,
		TenantID: tenantID,
		UserID:   userID,
		send:     make(chan []byte, 32),
		closed:   make(chan struct{}),
	}
}

// Send returns the outbound channel — the writer goroutine reads from this.
func (c *Client) Send() <-chan []byte { return c.send }

// Closed signals the writer goroutine to exit when the reader detects a
// dropped connection.
func (c *Client) Closed() <-chan struct{} { return c.closed }

// SignalClose is called once by the reader goroutine when the connection
// drops, so the writer can exit. Safe to call multiple times because we
// guard with a sync.Once via the Hub's Remove path closing `send`.
func (c *Client) SignalClose() {
	select {
	case <-c.closed:
		// already closed
	default:
		close(c.closed)
	}
}
