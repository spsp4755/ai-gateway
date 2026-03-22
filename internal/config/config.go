package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	BindAddr            string
	DataDir             string
	DatabasePath        string
	AdminUsername       string
	AdminPassword       string
	AllowAnonymous      bool
	UpstreamInsecureSkipVerify bool
	RequestTimeout      time.Duration
	HealthcheckInterval time.Duration
	Title               string
}

func Load() (Config, error) {
	cfg := Config{
		BindAddr:            envOrDefault("AI_GATEWAY_BIND_ADDR", ":8080"),
		DataDir:             envOrDefault("AI_GATEWAY_DATA_DIR", "./data"),
		AdminUsername:       os.Getenv("AI_GATEWAY_ADMIN_USERNAME"),
		AdminPassword:       os.Getenv("AI_GATEWAY_ADMIN_PASSWORD"),
		AllowAnonymous:      boolEnv("AI_GATEWAY_ALLOW_ANONYMOUS", false),
		UpstreamInsecureSkipVerify: boolEnv("AI_GATEWAY_UPSTREAM_INSECURE_SKIP_VERIFY", false),
		RequestTimeout:      durationEnv("AI_GATEWAY_REQUEST_TIMEOUT", 120*time.Second),
		HealthcheckInterval: durationEnv("AI_GATEWAY_HEALTHCHECK_INTERVAL", 30*time.Second),
		Title:               envOrDefault("AI_GATEWAY_TITLE", "AI Gateway"),
	}

	cfg.DatabasePath = envOrDefault("AI_GATEWAY_DATABASE_PATH", filepath.Join(cfg.DataDir, "ai-gateway.db"))
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o755); err != nil {
		return Config{}, err
	}
	if cfg.BindAddr == "" {
		return Config{}, errors.New("AI_GATEWAY_BIND_ADDR cannot be empty")
	}
	return cfg, nil
}

func (c Config) AdminAuthEnabled() bool {
	return c.AdminUsername != "" && c.AdminPassword != ""
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func boolEnv(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(value); err == nil {
		return parsed
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}
