package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// HealthHandler holds dependencies for the health endpoint.
type HealthHandler struct {
	db    *pgxpool.Pool
	redis *redis.Client
}

// NewHealthHandler creates a HealthHandler.
func NewHealthHandler(db *pgxpool.Pool, redis *redis.Client) *HealthHandler {
	return &HealthHandler{db: db, redis: redis}
}

type healthResponse struct {
	Status   string            `json:"status"`
	Services map[string]string `json:"services"`
	Time     time.Time         `json:"time"`
}

// Health handles GET /health
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	services := make(map[string]string)
	overallOK := true

	// Check Postgres
	if err := h.db.Ping(ctx); err != nil {
		services["postgres"] = "unhealthy"
		overallOK = false
	} else {
		services["postgres"] = "healthy"
	}

	// Check Redis
	if err := h.redis.Ping(ctx).Err(); err != nil {
		services["redis"] = "unhealthy"
		overallOK = false
	} else {
		services["redis"] = "healthy"
	}

	status := "ok"
	statusCode := http.StatusOK
	if !overallOK {
		status = "degraded"
		statusCode = http.StatusServiceUnavailable
	}

	types.WriteJSON(w, statusCode, healthResponse{
		Status:   status,
		Services: services,
		Time:     time.Now().UTC(),
	})
}
