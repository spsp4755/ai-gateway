package app

import (
	"testing"

	"github.com/spsp4755/ai-gateway/internal/config"
	"github.com/spsp4755/ai-gateway/internal/storage"
)

func TestBuildAttemptOrderPrefersHealthyActiveBackend(t *testing.T) {
	service := NewService(config.Config{}, nil)

	backends := []storage.Backend{
		{Name: "draining", Status: "draining", HealthStatus: "healthy", Priority: 1, Weight: 100},
		{Name: "healthy-active", Status: "active", HealthStatus: "healthy", Priority: 10, Weight: 100},
		{Name: "unhealthy-active", Status: "active", HealthStatus: "unhealthy", Priority: 1, Weight: 100},
	}

	order := service.buildAttemptOrder(backends)
	if len(order) != 3 {
		t.Fatalf("expected 3 backends in order, got %d", len(order))
	}
	if order[0].Name != "healthy-active" {
		t.Fatalf("expected healthy active backend first, got %s", order[0].Name)
	}
}

