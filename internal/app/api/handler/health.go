// Package handler holds the internal JSON API handlers (health, webhooks).
package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Health reports process and database liveness.
type Health struct {
	store *repository.Store
}

func NewHealth(store *repository.Store) *Health {
	return &Health{store: store}
}

type healthResponse struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

// Get responds 200 when the database answers a ping, 503 otherwise.
func (h *Health) Get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	resp := healthResponse{Status: "ok", DB: "ok"}
	status := http.StatusOK

	if err := h.store.Ping(ctx); err != nil {
		resp.Status = "degraded"
		resp.DB = "error"
		status = http.StatusServiceUnavailable
	}

	util.JSON(w, status, resp)
}
