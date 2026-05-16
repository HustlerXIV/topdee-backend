// cmd/seed-admin/main.go
// Promotes an existing user to platform admin by email.
//
// Usage (from backend/):
//
//	go run ./cmd/seed-admin --email tangnapat14@gmail.com
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	email := flag.String("email", "", "email of the user to promote")
	flag.Parse()

	if *email == "" {
		log.Fatal("--email is required")
	}
	*email = strings.ToLower(strings.TrimSpace(*email))

	// Load .env if present (same dir the backend uses).
	_ = godotenv.Load(".env")

	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}
	mongoDB := os.Getenv("MONGO_DB")
	if mongoDB == "" {
		mongoDB = "topdee"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer client.Disconnect(ctx)

	coll := client.Database(mongoDB).Collection("users")

	// Find first so we can print what we're touching.
	var user bson.M
	if err := coll.FindOne(ctx, bson.M{"email": *email}).Decode(&user); err != nil {
		log.Fatalf("user not found for email %q: %v", *email, err)
	}

	res, err := coll.UpdateOne(ctx,
		bson.M{"email": *email},
		bson.M{"$set": bson.M{"is_platform_admin": true}},
	)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	if res.MatchedCount == 0 {
		log.Fatalf("no user matched email %q", *email)
	}

	fmt.Printf("✓ Promoted %s (id: %v) to platform admin.\n", *email, user["_id"])
	fmt.Println("  Restart the backend and log in again to get a fresh JWT with is_admin=true.")
}
