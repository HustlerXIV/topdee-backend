package db

import (
	"context"
	"log"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Mongo struct {
	Client *mongo.Client
	DB     *mongo.Database
}

func Connect(ctx context.Context, uri, dbName string) (*Mongo, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}
	m := &Mongo{Client: client, DB: client.Database(dbName)}
	if err := m.ensureIndexes(ctx); err != nil {
		log.Printf("warning: index ensure: %v", err)
	}
	log.Printf("mongo connected: %s", dbName)
	return m, nil
}

func (m *Mongo) ensureIndexes(ctx context.Context) error {
	// users.email unique per tenant
	if _, err := m.DB.Collection("users").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "email", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("uniq_tenant_email"),
	}); err != nil {
		return err
	}
	// knowledge_bases.tenant_id
	if _, err := m.DB.Collection("knowledge_bases").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "tenant_id", Value: 1}},
	}); err != nil {
		return err
	}
	// tenants.facebook.page_id — sparse + unique so each FB page maps to ≤ 1 tenant
	if _, err := m.DB.Collection("tenants").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "facebook.page_id", Value: 1}},
		Options: options.Index().
			SetUnique(true).
			SetSparse(true).
			SetName("uniq_fb_page"),
	}); err != nil {
		return err
	}
	// messages indexes
	if _, err := m.DB.Collection("messages").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "conversation_id", Value: 1}, {Key: "created_at", Value: 1}}},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}},
	}); err != nil {
		return err
	}
	// channel_connections — generic per-tenant external account bindings.
	// The (provider, external_id) pair is globally unique so the same FB
	// page or LINE channel can't be claimed by two tenants. Tenant-scoped
	// listing/counting is the other hot path.
	if _, err := m.DB.Collection("channel_connections").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys: bson.D{{Key: "provider", Value: 1}, {Key: "external_id", Value: 1}},
			Options: options.Index().
				SetUnique(true).
				SetName("uniq_provider_external"),
		},
		{
			Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "provider", Value: 1}},
			Options: options.Index().SetName("tenant_provider"),
		},
	}); err != nil {
		return err
	}
	// channel_oauth_states — short-lived OAuth handshakes. TTL on
	// expires_at means Mongo cleans them up automatically; we set
	// expireAfterSeconds=0 so the *value* of expires_at is the deadline.
	if _, err := m.DB.Collection("channel_oauth_states").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0).SetName("ttl_expires_at"),
	}); err != nil {
		return err
	}
	return nil
}
