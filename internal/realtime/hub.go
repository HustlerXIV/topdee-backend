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
//
// `send` is intentionally NEVER closed: closing it would race with the
// broadcaster (which holds a snapshot reference to this Client and sends
// outside the hub lock), and sending on a closed channel panics — taking
// down the whole process. Teardown is signalled purely via `closed`.
type Client struct {
	Conn     *ws.Conn
	TenantID string
	UserID   string
	send     chan []byte
	closed   chan struct{}
	once     sync.Once
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

// Remove unregisters a client and signals its writer goroutine to exit.
// It does NOT close `send` (see the Client doc comment) — the writer exits
// on the `closed` signal instead, which cannot race with Broadcast.
func (h *Hub) Remove(c *Client) {
	h.mu.Lock()
	if set, ok := h.clients[c.TenantID]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, c.TenantID)
		}
	}
	h.mu.Unlock()
	c.SignalClose()
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
		// `closed` takes priority: if the client is tearing down we skip it
		// entirely. Because `send` is never closed, the send case can never
		// panic — at worst it hits `default` and the message is dropped.
		select {
		case <-c.closed:
			// client is shutting down; skip
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

// SignalClose closes the `closed` channel so the writer goroutine exits.
// Called from both the reader goroutine (on disconnect) and Hub.Remove;
// sync.Once makes it safe to call concurrently and repeatedly.
func (c *Client) SignalClose() {
	c.once.Do(func() { close(c.closed) })
}
