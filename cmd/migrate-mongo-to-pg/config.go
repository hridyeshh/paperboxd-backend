package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Config holds both database connections and migration settings.
type Config struct {
	MongoURI string
	MongoDB  string // database name
	PgDSN    string

	// Migration behaviour
	DryRun   bool // log inserts but don't execute
	BatchSize int
}

func configFromEnv() Config {
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		slog.Error("MONGO_URI not set")
		os.Exit(1)
	}
	mongoDB := os.Getenv("MONGO_DB")
	if mongoDB == "" {
		mongoDB = "paperboxd"
	}
	pgDSN := os.Getenv("POSTGRES_URL")
	if pgDSN == "" {
		slog.Error("POSTGRES_URL not set")
		os.Exit(1)
	}
	return Config{
		MongoURI:  mongoURI,
		MongoDB:   mongoDB,
		PgDSN:     pgDSN,
		DryRun:    os.Getenv("DRY_RUN") == "true",
		BatchSize: 500,
	}
}

// Connections bundles open database handles.
type Connections struct {
	Mongo  *mongo.Database
	PG     *pgxpool.Pool
	mongoc *mongo.Client
}

func connect(ctx context.Context, cfg Config) (*Connections, error) {
	// MongoDB
	mongoClient, err := mongo.Connect(options.Client().ApplyURI(cfg.MongoURI).
		SetTimeout(30 * time.Second))
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	if err := mongoClient.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("mongo ping: %w", err)
	}
	slog.Info("MongoDB connected")

	// PostgreSQL
	pgPool, err := pgxpool.New(ctx, cfg.PgDSN)
	if err != nil {
		return nil, fmt.Errorf("pg connect: %w", err)
	}
	if err := pgPool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pg ping: %w", err)
	}
	slog.Info("PostgreSQL connected")

	return &Connections{
		Mongo:  mongoClient.Database(cfg.MongoDB),
		PG:     pgPool,
		mongoc: mongoClient,
	}, nil
}

func (c *Connections) Close(ctx context.Context) {
	if err := c.mongoc.Disconnect(ctx); err != nil {
		slog.Error("mongo disconnect", "error", err)
	}
	c.PG.Close()
}
