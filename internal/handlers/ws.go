package handlers

// WebSocket endpoint — /ws?token=<jwt>
//
// Browsers can't send Authorization headers on WS connections, so we accept
// the JWT as a query-string parameter instead. The connection is closed
// immediately if the token is missing or invalid.
//
// Once authed, the client is registered in the Hub. The Hub fans out
// inbox_update events to every connected dashboard tab in the same tenant.

import (
	"log"

	"github.com/gofiber/fiber/v2"
	ws "github.com/gofiber/websocket/v2"

	"github.com/topdee/backend/internal/auth"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/realtime"
)

// WSUpgrade is a middleware that rejects non-WebSocket requests before they
// reach the ws.New handler (Fiber requirement).
func WSUpgrade() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if ws.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	}
}

// WSHandler returns the WebSocket handler that authenticates and registers
// the client in the Hub.
func WSHandler(hub *realtime.Hub, cfg *config.Config) fiber.Handler {
	return ws.New(func(c *ws.Conn) {
		token := c.Query("token")
		claims, err := auth.Parse(cfg.JWTSecret, token)
		if err != nil {
			log.Printf("ws: auth failed: %v", err)
			_ = c.Close()
			return
		}

		client := realtime.NewClient(c, claims.TenantID, claims.UserID)
		hub.Add(client)
		defer hub.Remove(client)

		// Writer goroutine — drains the send channel onto the socket.
		go func() {
			for {
				select {
				case msg, ok := <-client.Send():
					if !ok {
						return
					}
					if err := c.WriteMessage(ws.TextMessage, msg); err != nil {
						return
					}
				case <-client.Closed():
					return
				}
			}
		}()

		// Reader — keeps the connection alive and detects client disconnect.
		// We don't expect clients to send anything; we just need the read
		// loop to detect the close frame.
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				client.SignalClose()
				return
			}
		}
	})
}
