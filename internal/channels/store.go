package channels

// Mongo-backed CRUD for ChannelConnection. Lives in this package (not in
// handlers) so providers and the migration helper can hit it directly.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/models"
)

const (
	connectionsColl = "channel_connections"
	oauthStatesColl = "channel_oauth_states"
	profilesColl    = "customer_profiles"
)

// ErrConnectionTaken is returned when the (provider, external_id) pair is
// already owned by a different tenant — surfaces as 409 to the caller.
var ErrConnectionTaken = errors.New("this account is already connected to another workspace")

type Store struct {
	mongo *db.Mongo
}

func NewStore(m *db.Mongo) *Store { return &Store{mongo: m} }

func (s *Store) coll() *mongo.Collection {
	return s.mongo.DB.Collection(connectionsColl)
}

// FindByExternal returns the connection for (provider, externalID), or nil
// when none exists. Used by the webhook router to map an inbound event to
// a tenant.
func (s *Store) FindByExternal(ctx context.Context, provider, externalID string) (*models.ChannelConnection, error) {
	var c models.ChannelConnection
	err := s.coll().FindOne(ctx, bson.M{
		"provider":    provider,
		"external_id": externalID,
	}).Decode(&c)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// FindForTenant returns one specific connection, scoped to the tenant —
// the scoping prevents tenant A from deleting tenant B's connection by
// guessing the id.
func (s *Store) FindForTenant(ctx context.Context, tenantID, id string) (*models.ChannelConnection, error) {
	var c models.ChannelConnection
	err := s.coll().FindOne(ctx, bson.M{
		"_id":       id,
		"tenant_id": tenantID,
	}).Decode(&c)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListByTenant returns every connection owned by a tenant, sorted oldest
// first so UIs render in stable order.
func (s *Store) ListByTenant(ctx context.Context, tenantID string) ([]models.ChannelConnection, error) {
	cur, err := s.coll().Find(
		ctx,
		bson.M{"tenant_id": tenantID},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []models.ChannelConnection{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CountByProvider returns how many connections of `provider` the tenant has —
// used to enforce per-provider plan limits.
func (s *Store) CountByProvider(ctx context.Context, tenantID, provider string) (int64, error) {
	return s.coll().CountDocuments(ctx, bson.M{
		"tenant_id": tenantID,
		"provider":  provider,
	})
}

// CountByTenant returns how many connections the tenant has total across
// every provider — used to enforce a plan's total-channel cap.
func (s *Store) CountByTenant(ctx context.Context, tenantID string) (int64, error) {
	return s.coll().CountDocuments(ctx, bson.M{"tenant_id": tenantID})
}

// Upsert inserts or updates a connection identified by (tenant_id, provider,
// external_id). Returns ErrConnectionTaken if the same external_id is already
// claimed by *another* tenant.
func (s *Store) Upsert(ctx context.Context, conn *models.ChannelConnection) error {
	if conn.ID == "" {
		conn.ID = uuid.NewString()
	}
	if conn.CreatedAt.IsZero() {
		conn.CreatedAt = time.Now().UTC()
	}
	conn.UpdatedAt = time.Now().UTC()
	if conn.Status == "" {
		conn.Status = models.ChannelStatusActive
	}

	// Pre-flight: is the external account already claimed elsewhere?
	existing, err := s.FindByExternal(ctx, conn.Provider, conn.ExternalID)
	if err != nil {
		return err
	}
	if existing != nil && existing.TenantID != conn.TenantID {
		return ErrConnectionTaken
	}

	_, err = s.coll().UpdateOne(
		ctx,
		bson.M{
			"tenant_id":   conn.TenantID,
			"provider":    conn.Provider,
			"external_id": conn.ExternalID,
		},
		bson.M{
			"$set": bson.M{
				"display_name": conn.DisplayName,
				"credentials":  conn.Credentials,
				"config":       conn.Config,
				"status":       conn.Status,
				"error":        conn.Error,
				"updated_at":   conn.UpdatedAt,
			},
			"$setOnInsert": bson.M{
				"_id":        conn.ID,
				"tenant_id":  conn.TenantID,
				"provider":   conn.Provider,
				"external_id": conn.ExternalID,
				"created_by": conn.CreatedBy,
				"created_at": conn.CreatedAt,
			},
		},
		options.Update().SetUpsert(true),
	)
	if mongo.IsDuplicateKeyError(err) {
		return ErrConnectionTaken
	}
	return err
}

// Delete removes a connection scoped to the tenant. Returns false if no
// matching connection existed (404 from caller's perspective).
func (s *Store) Delete(ctx context.Context, tenantID, id string) (bool, error) {
	res, err := s.coll().DeleteOne(ctx, bson.M{
		"_id":       id,
		"tenant_id": tenantID,
	})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

// UpdateCredentials replaces the credentials map on a connection. Used by
// CredentialRefresher implementations to persist rotated tokens. Doesn't
// touch any other fields.
func (s *Store) UpdateCredentials(ctx context.Context, id string, creds map[string]string) error {
	_, err := s.coll().UpdateOne(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{
			"credentials": creds,
			"updated_at":  time.Now().UTC(),
		}},
	)
	return err
}

// MarkError flips a connection to status="error" with a message. Used when
// outbound sends fail repeatedly so the dashboard can surface the issue.
func (s *Store) MarkError(ctx context.Context, id, msg string) error {
	_, err := s.coll().UpdateOne(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{
			"status":     models.ChannelStatusError,
			"error":      msg,
			"updated_at": time.Now().UTC(),
		}},
	)
	return err
}

// ── OAuth state helpers ─────────────────────────────────────────────────

// SaveOAuthState persists an in-flight OAuth handshake. Called twice: once
// when the user starts (only state, tenant_id, user_id are known) and again
// when Meta redirects back (we add the user_access_token + pages).
func (s *Store) SaveOAuthState(ctx context.Context, st *models.FacebookOAuthState) error {
	if st.CreatedAt.IsZero() {
		st.CreatedAt = time.Now().UTC()
	}
	if st.ExpiresAt.IsZero() {
		st.ExpiresAt = time.Now().Add(15 * time.Minute).UTC()
	}
	_, err := s.mongo.DB.Collection(oauthStatesColl).UpdateOne(
		ctx,
		bson.M{"_id": st.State},
		bson.M{"$set": st},
		options.Update().SetUpsert(true),
	)
	return err
}

// GetOAuthState fetches an in-flight OAuth handshake by its state token.
// Returns nil when missing or expired.
func (s *Store) GetOAuthState(ctx context.Context, state string) (*models.FacebookOAuthState, error) {
	var st models.FacebookOAuthState
	err := s.mongo.DB.Collection(oauthStatesColl).FindOne(ctx, bson.M{"_id": state}).Decode(&st)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.ExpiresAt.IsZero() && time.Now().After(st.ExpiresAt) {
		return nil, nil
	}
	return &st, nil
}

// DeleteOAuthState consumes a state record once we're done with it.
func (s *Store) DeleteOAuthState(ctx context.Context, state string) error {
	_, err := s.mongo.DB.Collection(oauthStatesColl).DeleteOne(ctx, bson.M{"_id": state})
	return err
}

// ── Customer profile cache ──────────────────────────────────────────────

// profileID is the deterministic _id we use for CustomerProfile docs:
// "<provider>:<external_user_id>". Globally unique per platform.
func profileID(provider, externalUserID string) string {
	return provider + ":" + externalUserID
}

// GetProfile returns the cached display name / picture for a customer,
// or nil when none has been fetched yet.
func (s *Store) GetProfile(ctx context.Context, provider, externalUserID string) (*models.CustomerProfile, error) {
	var p models.CustomerProfile
	err := s.mongo.DB.Collection(profilesColl).
		FindOne(ctx, bson.M{"_id": profileID(provider, externalUserID)}).
		Decode(&p)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpsertProfile saves (or refreshes) the cached profile for a customer.
// Best-effort callers can ignore the returned error.
func (s *Store) UpsertProfile(ctx context.Context, p *models.CustomerProfile) error {
	if p.Provider == "" || p.ExternalUserID == "" {
		return errors.New("profile: missing provider or external_user_id")
	}
	p.ID = profileID(p.Provider, p.ExternalUserID)
	p.UpdatedAt = time.Now().UTC()
	_, err := s.mongo.DB.Collection(profilesColl).UpdateOne(
		ctx,
		bson.M{"_id": p.ID},
		bson.M{"$set": p},
		options.Update().SetUpsert(true),
	)
	return err
}

// ── Instagram OAuth state helpers ───────────────────────────────────────
//
// Stored in a separate collection ("channel_ig_oauth_states") to avoid
// type conflicts with the Facebook OAuth state documents.

const igOAuthStatesColl = "channel_ig_oauth_states"

// SaveIGOAuthState persists an in-flight Instagram OAuth handshake.
func (s *Store) SaveIGOAuthState(ctx context.Context, st *models.InstagramOAuthState) error {
	if st.CreatedAt.IsZero() {
		st.CreatedAt = time.Now().UTC()
	}
	if st.ExpiresAt.IsZero() {
		st.ExpiresAt = time.Now().Add(15 * time.Minute).UTC()
	}
	_, err := s.mongo.DB.Collection(igOAuthStatesColl).UpdateOne(
		ctx,
		bson.M{"_id": st.State},
		bson.M{"$set": st},
		options.Update().SetUpsert(true),
	)
	return err
}

// GetIGOAuthState fetches a pending Instagram OAuth handshake.
// Returns nil when missing or expired.
func (s *Store) GetIGOAuthState(ctx context.Context, state string) (*models.InstagramOAuthState, error) {
	var st models.InstagramOAuthState
	err := s.mongo.DB.Collection(igOAuthStatesColl).FindOne(ctx, bson.M{"_id": state}).Decode(&st)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.ExpiresAt.IsZero() && time.Now().After(st.ExpiresAt) {
		return nil, nil
	}
	return &st, nil
}

// DeleteIGOAuthState removes the state record after use.
func (s *Store) DeleteIGOAuthState(ctx context.Context, state string) error {
	_, err := s.mongo.DB.Collection(igOAuthStatesColl).DeleteOne(ctx, bson.M{"_id": state})
	return err
}
