package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/spsp4755/ai-gateway/internal/app"
	"github.com/spsp4755/ai-gateway/internal/config"
	"github.com/spsp4755/ai-gateway/internal/storage"
)

func TestListModelsReturnsRegisteredLogicalModels(t *testing.T) {
	tempDir := t.TempDir()
	cfg := config.Config{
		BindAddr:            ":8080",
		DataDir:             tempDir,
		DatabasePath:        filepath.Join(tempDir, "gateway.db"),
		AllowAnonymous:      true,
		RequestTimeout:      2 * time.Second,
		HealthcheckInterval: 30 * time.Second,
		Title:               "Test Gateway",
	}

	store, err := storage.New(cfg.DatabasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.Initialize(); err != nil {
		t.Fatalf("initialize store: %v", err)
	}
	if err := store.CreateLogicalModel(context.Background(), "qwen-chat-prod", "test model", "active"); err != nil {
		t.Fatalf("create model: %v", err)
	}

	service := app.NewService(cfg, store)
	server := NewServer(cfg, store, service)

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", response.Code)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "qwen-chat-prod" {
		t.Fatalf("unexpected payload: %s", response.Body.String())
	}
}
