package channels

// One-time migration from the old `tenants.facebook` / `tenants.line`
// sub-documents to the new `channel_connections` collection. Runs at
// startup after Mongo is connected; idempotent (safe to run on every boot)
// thanks to Upsert by (tenant_id, provider, external_id).
//
// We deliberately leave the old tenant fields in place so a quick rollback
// is possible — the new code paths just stop reading them.

import (
	"context"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/models"
)

// MigrateLegacyTenantConnections walks every tenant doc, copying any
// `facebook` and `line` sub-doc into the channel_connections collection.
//
// Returns counts (fb, line, errors) for log visibility.
func MigrateLegacyTenantConnections(ctx context.Context, m *db.Mongo) (fb, line, errCount int) {
	store := NewStore(m)

	cur, err := m.DB.Collection("tenants").Find(ctx, bson.M{})
	if err != nil {
		log.Printf("channels: migrate: list tenants: %v", err)
		return 0, 0, 1
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var t models.Tenant
		if err := cur.Decode(&t); err != nil {
			errCount++
			continue
		}
		if t.Facebook != nil && t.Facebook.PageID != "" {
			conn := &models.ChannelConnection{
				TenantID:    t.ID,
				Provider:    models.ProviderFacebook,
				ExternalID:  t.Facebook.PageID,
				DisplayName: t.Facebook.PageName,
				Credentials: map[string]string{
					"page_access_token": t.Facebook.PageAccessToken,
				},
				Status:    models.ChannelStatusActive,
				CreatedAt: zeroOr(t.Facebook.ConnectedAt),
			}
			if err := store.Upsert(ctx, conn); err != nil {
				log.Printf("channels: migrate fb tenant=%s: %v", t.ID, err)
				errCount++
			} else {
				fb++
			}
		}
		if t.Line != nil && t.Line.ChannelID != "" {
			conn := &models.ChannelConnection{
				TenantID:    t.ID,
				Provider:    models.ProviderLine,
				ExternalID:  t.Line.ChannelID,
				DisplayName: "LINE Official Account",
				Credentials: map[string]string{
					"channel_secret":       t.Line.ChannelSecret,
					"channel_access_token": t.Line.ChannelAccessToken,
				},
				Status:    models.ChannelStatusActive,
				CreatedAt: zeroOr(t.Line.ConnectedAt),
			}
			if err := store.Upsert(ctx, conn); err != nil {
				log.Printf("channels: migrate line tenant=%s: %v", t.ID, err)
				errCount++
			} else {
				line++
			}
		}
	}
	return fb, line, errCount
}

func zeroOr(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}
