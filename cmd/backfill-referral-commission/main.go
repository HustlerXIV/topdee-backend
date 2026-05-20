// cmd/backfill-referral-commission/main.go
//
// One-time backfill: credits the first-payment commission for any referral
// that was never paid out (commission_count == 0) but whose referred tenant
// already has an active paid subscription.
//
// Safe to run multiple times — it only processes referrals with
// commission_count == 0 and skips any tenant on the free plan.
//
// Usage (from backend/):
//
//	go run ./cmd/backfill-referral-commission
//	go run ./cmd/backfill-referral-commission --dry-run   # preview without writing
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "print what would be credited without writing to DB")
	flag.Parse()

	_ = godotenv.Load(".env")

	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}
	mongoDB := os.Getenv("MONGO_DB")
	if mongoDB == "" {
		mongoDB = "topdee"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer client.Disconnect(ctx)
	db := client.Database(mongoDB)

	// ── Load referral settings ────────────────────────────────────────────────
	var settings struct {
		Enabled               bool `bson:"enabled"`
		FirstCommissionAmount int  `bson:"first_commission_amount"`
	}
	err = db.Collection("referral_settings").
		FindOne(ctx, bson.M{"_id": "global"}).Decode(&settings)
	if err == mongo.ErrNoDocuments {
		// Default
		settings.Enabled = true
		settings.FirstCommissionAmount = 10000 // ฿100 in satang
	} else if err != nil {
		log.Fatalf("load settings: %v", err)
	}
	if !settings.Enabled {
		log.Fatal("referral programme is disabled — aborting")
	}
	if settings.FirstCommissionAmount <= 0 {
		log.Fatal("first_commission_amount is 0 — nothing to credit")
	}

	// ── Find referrals that were never paid a commission ──────────────────────
	cur, err := db.Collection("referrals").Find(ctx, bson.M{
		"status":           "active",
		"commission_count": 0,
	})
	if err != nil {
		log.Fatalf("find referrals: %v", err)
	}
	defer cur.Close(ctx)

	type referral struct {
		ID                 string    `bson:"_id"`
		ReferrerTenantID   string    `bson:"referrer_tenant_id"`
		ReferredTenantID   string    `bson:"referred_tenant_id"`
		ReferredTenantName string    `bson:"referred_tenant_name"`
		CommissionCount    int       `bson:"commission_count"`
		UpdatedAt          time.Time `bson:"updated_at"`
	}

	credited := 0
	skipped := 0

	for cur.Next(ctx) {
		var r referral
		if err := cur.Decode(&r); err != nil {
			log.Printf("decode: %v", err)
			continue
		}

		// Check that the referred tenant is on a paid plan (not free/trialing).
		var tenant struct {
			Plan         string `bson:"plan"`
			Subscription *struct {
				Status string `bson:"status"`
			} `bson:"subscription"`
		}
		err := db.Collection("tenants").
			FindOne(ctx, bson.M{"_id": r.ReferredTenantID}).Decode(&tenant)
		if err != nil {
			log.Printf("tenant %s not found, skipping referral %s", r.ReferredTenantID, r.ID)
			skipped++
			continue
		}
		if tenant.Plan == "free" || tenant.Plan == "" {
			fmt.Printf("  SKIP  referral %s — referred tenant %q is on free plan\n",
				r.ID, r.ReferredTenantName)
			skipped++
			continue
		}
		subStatus := ""
		if tenant.Subscription != nil {
			subStatus = tenant.Subscription.Status
		}
		if subStatus == "trialing" || subStatus == "canceled" || subStatus == "" {
			fmt.Printf("  SKIP  referral %s — referred tenant %q subscription status=%q\n",
				r.ID, r.ReferredTenantName, subStatus)
			skipped++
			continue
		}

		amount := settings.FirstCommissionAmount
		description := fmt.Sprintf("Referral commission from %s — ฿%.2f (backfill)",
			r.ReferredTenantName, float64(amount)/100)

		fmt.Printf("  CREDIT referral %s — referrer tenant %s ← ฿%.2f from %q\n",
			r.ID, r.ReferrerTenantID, float64(amount)/100, r.ReferredTenantName)

		if *dryRun {
			credited++
			continue
		}

		now := time.Now().UTC()

		// Upsert wallet.
		_, err = db.Collection("wallets").UpdateOne(
			ctx,
			bson.M{"_id": r.ReferrerTenantID},
			bson.M{
				"$inc": bson.M{"balance": amount},
				"$set": bson.M{"tenant_id": r.ReferrerTenantID, "updated_at": now},
				"$setOnInsert": bson.M{"payout_type": "manual"},
			},
			options.Update().SetUpsert(true),
		)
		if err != nil {
			log.Printf("  ERROR wallet upsert for referral %s: %v", r.ID, err)
			continue
		}

		// Insert wallet transaction.
		txn := bson.M{
			"_id":         uuid.NewString(),
			"tenant_id":   r.ReferrerTenantID,
			"type":        "commission",
			"amount":      amount,
			"referral_id": r.ID,
			"description": description,
			"created_at":  now,
		}
		if _, err := db.Collection("wallet_transactions").InsertOne(ctx, txn); err != nil {
			log.Printf("  ERROR insert txn for referral %s: %v", r.ID, err)
			continue
		}

		// Update referral record.
		_, err = db.Collection("referrals").UpdateOne(ctx,
			bson.M{"_id": r.ID},
			bson.M{
				"$inc": bson.M{"commission_count": 1, "total_earned": amount},
				"$set": bson.M{"updated_at": now},
			},
		)
		if err != nil {
			log.Printf("  ERROR update referral %s: %v", r.ID, err)
			continue
		}

		credited++
	}

	if *dryRun {
		fmt.Printf("\nDry run — would credit %d referral(s), skip %d.\n", credited, skipped)
	} else {
		fmt.Printf("\nDone — credited %d referral(s), skipped %d.\n", credited, skipped)
	}
}
