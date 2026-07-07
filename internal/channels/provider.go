// Package channels is the platform-agnostic webhook + outbound layer.
//
// Adding a new social platform is a matter of writing a `Provider`
// implementation and registering it in main.go. Everything else — webhook
// routing, signature verification, tenant lookup, plan-limit enforcement,
// connection CRUD — is generic.
//
//	┌────────────┐   POST /webhooks/<name>   ┌───────────────────┐
//	│  Platform  │ ────────────────────────▶ │ generic webhook   │
//	│  (LINE,    │                            │   handler         │
//	│   FB, …)   │                            │                   │
//	└────────────┘                            │ 1. Provider.Parse │
//	                                          │ 2. lookup conn    │
//	                                          │ 3. Provider.Verify│
//	                                          │ 4. Orchestrator   │
//	                                          │ 5. Provider.Send  │
//	                                          └───────────────────┘
package channels

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/models"
)

// ParsedEvent is the platform-neutral form of one inbound message.
//
// Providers translate their wire formats (FB Messenger entries, LINE events,
// future: Telegram updates, WhatsApp messages…) into this shape so the
// orchestrator never has to care about the source.
type ParsedEvent struct {
	// ExternalChannelID identifies which connection this event is for —
	// FB page id, LINE channel/destination id, etc. Used to look up the
	// owning tenant via (provider, external_id).
	ExternalChannelID string
	// ExternalUserID is the sender's id within the platform.
	ExternalUserID string
	// Text is the user's message. Media-only events can leave this empty and
	// carry Attachments instead.
	Text string
	// Attachments are media payloads sent with the message. Public providers
	// can include URL directly; private providers can include an ID for the
	// backend to proxy later.
	Attachments []models.Attachment
	// Timestamp is when the platform reports the message was sent.
	Timestamp time.Time
	// ReplyToken — LINE's free reply mechanism. Empty for FB.
	ReplyToken string
	// IsAgentEcho marks a message the business itself sent that we learned
	// about via a platform echo webhook — e.g. an admin replying by hand in
	// the Facebook Page inbox. These are recorded to the transcript as a
	// human turn; the orchestrator must NOT run an AI turn or push a reply
	// for them. ExternalUserID still points at the customer side of the chat.
	IsAgentEcho bool
	// EchoAppID is the `app_id` carried on a Facebook message_echoes event, if
	// any. It identifies which app sent the message: our own topdee app (bot
	// replies + dashboard human replies dispatched via the Send API) vs. a
	// reply typed natively in the Page inbox / Business Suite (which is either
	// absent or Meta's own Pages app id). The webhook router compares this to
	// our configured FB app id to decide whether the echo is our own (skip,
	// already stored) or an external human reply (record). Zero when absent.
	EchoAppID int64
	// Raw is the original event JSON, kept for logging / debugging.
	Raw map[string]any
}

// Provider is the interface every platform implements.
//
// All methods must be safe for concurrent use — the registry caches one
// instance per provider name and reuses it across requests.
type Provider interface {
	// Name is the lowercase identifier used in routes (/webhooks/<name>),
	// in `provider` columns, and in plan-limit lookups.
	Name() string

	// HandshakeVerify handles GET /webhooks/<name> challenge handshakes
	// (e.g. Facebook's hub.challenge dance). Providers that don't use a
	// GET handshake just return false.
	HandshakeVerify(query map[string]string, cfg *config.Config) (ok bool, body string)

	// ParseEvents extracts inbound events from a raw POST body. Pure — no
	// network or DB access — so the router can call it before signature
	// verification (which is fine: the signature covers the body bytes,
	// not anything we derived from them).
	ParseEvents(body []byte) ([]ParsedEvent, error)

	// VerifySignature validates the request signature. Some providers
	// verify with an app-level secret (Facebook), others with the
	// connection's own secret (LINE). The router calls this once per
	// (request, connection) pair after parsing.
	VerifySignature(headers map[string]string, body []byte, cfg *config.Config, conn *models.ChannelConnection) bool

	// Send dispatches an outbound reply for one event. `evt` is the inbound
	// message we're replying to (some providers — LINE — need its ReplyToken
	// to use the free reply API).
	Send(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, reply string) error
}

// Registry is a thread-safe lookup from provider name → Provider. Built
// once at startup, read everywhere.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Provider
}

func NewRegistry() *Registry { return &Registry{m: map[string]Provider{}} }

// Register adds (or replaces) a provider. Last-writer-wins.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[p.Name()] = p
}

// Get returns the provider with the given name, plus ok=false if missing.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.m[name]
	return p, ok
}

// Names returns all registered provider names. Order is unspecified.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for n := range r.m {
		out = append(out, n)
	}
	return out
}

// ErrUnknownProvider is returned when /webhooks/:provider doesn't match.
var ErrUnknownProvider = errors.New("unknown provider")

// CredentialRefresher is an *optional* extension to Provider for platforms
// whose credentials rotate (e.g. LINE's 30-day access tokens that we mint
// from channel id + secret). The generic webhook router type-asserts into
// this interface before each Send and persists conn back to Mongo when
// EnsureCredentials reports refreshed=true.
//
// Providers that don't need refresh just don't implement it.
type CredentialRefresher interface {
	EnsureCredentials(ctx context.Context, conn *models.ChannelConnection) (refreshed bool, err error)
}

// ImageSender is an *optional* extension to Provider for platforms that
// support sending image messages. Providers that don't support images
// just don't implement this interface — the inbox handler will fall back
// to a "not supported" error in that case.
//
// imageURL must be a publicly accessible HTTPS URL (already uploaded to R2
// or another CDN); the provider SDK/API will fetch it.
type ImageSender interface {
	SendImage(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, imageURL string) error
}
