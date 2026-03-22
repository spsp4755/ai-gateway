package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spsp4755/ai-gateway/internal/app"
	"github.com/spsp4755/ai-gateway/internal/config"
	"github.com/spsp4755/ai-gateway/internal/storage"
	"github.com/spsp4755/ai-gateway/internal/version"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	cfg       config.Config
	store     *storage.Store
	service   *app.Service
	templates *template.Template
	static    http.Handler
}

type apiKeyView struct {
	ID            int64
	Name          string
	AllowedModels string
	Enabled       bool
	LastUsedAt    string
	CreatedAt     string
}

type dashboardData struct {
	Title          string
	Version        string
	Stats          storage.Stats
	Models         []storage.LogicalModel
	Backends       []storage.Backend
	APIKeys        []apiKeyView
	Logs           []storage.RequestLog
	Notice         string
	CreatedKey     string
	AllowAnonymous bool
	AdminProtected bool
}

func NewServer(cfg config.Config, store *storage.Store, service *app.Service) *Server {
	templates := template.Must(template.New("").Funcs(template.FuncMap{
		"statusClass": statusClass,
		"shortTime":   shortTime,
	}).ParseFS(assets, "templates/*.html"))

	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}

	return &Server{
		cfg:       cfg,
		store:     store,
		service:   service,
		templates: templates,
		static:    http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", s.static)
	mux.HandleFunc("GET /", s.redirectHome)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /v1/models", s.api(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.api(s.handleChatCompletions))
	mux.HandleFunc("GET /admin", s.admin(s.handleDashboard))
	mux.HandleFunc("POST /admin/models", s.admin(s.handleCreateModel))
	mux.HandleFunc("POST /admin/models/", s.admin(s.handleModelAction))
	mux.HandleFunc("POST /admin/backends", s.admin(s.handleCreateBackend))
	mux.HandleFunc("POST /admin/backends/", s.admin(s.handleBackendAction))
	mux.HandleFunc("POST /admin/api-keys", s.admin(s.handleCreateAPIKey))
	mux.HandleFunc("POST /admin/api-keys/", s.admin(s.handleAPIKeyAction))
	mux.HandleFunc("GET /admin/export", s.admin(s.handleExport))
	mux.HandleFunc("POST /admin/import", s.admin(s.handleImport))
	return s.logging(mux)
}

func (s *Server) redirectHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, fmt.Sprintf(`{"status":"ok","version":%q}`, version.Value))
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request, auth *storage.APIKeyAuth) {
	models, err := s.service.SelectAccessibleModels(r.Context(), auth)
	if err != nil {
		s.writeServerError(w, err)
		return
	}
	type modelObject struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	response := struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}{
		Object: "list",
		Data:   make([]modelObject, 0, len(models)),
	}
	for _, model := range models {
		response.Data = append(response.Data, modelObject{
			ID:      model.Name,
			Object:  "model",
			OwnedBy: "ai-gateway",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request, auth *storage.APIKeyAuth) {
	if err := s.service.ProxyChatCompletion(w, r, auth); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.writeServerError(w, err)
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.renderDashboard(w, r)
}

func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNotice(w, r, "Could not read the model form.")
		return
	}
	if err := s.store.CreateLogicalModel(r.Context(), r.FormValue("name"), r.FormValue("description"), r.FormValue("status")); err != nil {
		s.redirectWithNotice(w, r, "Failed to create model: "+err.Error())
		return
	}
	s.redirectWithNotice(w, r, "Logical model added.")
}

func (s *Server) handleModelAction(w http.ResponseWriter, r *http.Request) {
	id, action, ok := parseIDAction(r.URL.Path, "/admin/models/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "update":
		if err := r.ParseForm(); err != nil {
			s.redirectWithNotice(w, r, "Could not read the model update form.")
			return
		}
		err := s.store.UpdateLogicalModel(r.Context(), id, r.FormValue("name"), r.FormValue("description"), r.FormValue("status"))
		if err != nil {
			s.redirectWithNotice(w, r, "Failed to update model: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, "Model changes saved.")
	case "delete":
		if err := s.store.DeleteLogicalModel(r.Context(), id); err != nil {
			s.redirectWithNotice(w, r, "Failed to delete model: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, "Model deleted.")
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleCreateBackend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNotice(w, r, "Could not read the backend form.")
		return
	}
	modelID, err := parseInt64(r.FormValue("logical_model_id"))
	if err != nil {
		s.redirectWithNotice(w, r, "Select a logical model first.")
		return
	}
	backend := storage.Backend{
		LogicalModelID:    modelID,
		Name:              r.FormValue("name"),
		BaseURL:           r.FormValue("base_url"),
		UpstreamModelName: r.FormValue("upstream_model_name"),
		APIKey:            r.FormValue("api_key"),
		HealthcheckPath:   r.FormValue("healthcheck_path"),
		Priority:          parseInt(r.FormValue("priority"), 100),
		Weight:            parseInt(r.FormValue("weight"), 100),
		TimeoutMs:         parseInt(r.FormValue("timeout_ms"), 120000),
		Status:            r.FormValue("status"),
	}
	if err := s.store.CreateBackend(r.Context(), backend); err != nil {
		s.redirectWithNotice(w, r, "Failed to create backend: "+err.Error())
		return
	}
	s.redirectWithNotice(w, r, "Backend added.")
}

func (s *Server) handleBackendAction(w http.ResponseWriter, r *http.Request) {
	id, action, ok := parseIDAction(r.URL.Path, "/admin/backends/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "update":
		if err := r.ParseForm(); err != nil {
			s.redirectWithNotice(w, r, "Could not read the backend update form.")
			return
		}
		modelID, err := parseInt64(r.FormValue("logical_model_id"))
		if err != nil {
			s.redirectWithNotice(w, r, "A logical model is required for this backend.")
			return
		}
		backend := storage.Backend{
			ID:                id,
			LogicalModelID:    modelID,
			Name:              r.FormValue("name"),
			BaseURL:           r.FormValue("base_url"),
			UpstreamModelName: r.FormValue("upstream_model_name"),
			APIKey:            r.FormValue("api_key"),
			HealthcheckPath:   r.FormValue("healthcheck_path"),
			Priority:          parseInt(r.FormValue("priority"), 100),
			Weight:            parseInt(r.FormValue("weight"), 100),
			TimeoutMs:         parseInt(r.FormValue("timeout_ms"), 120000),
			Status:            r.FormValue("status"),
		}
		if err := s.store.UpdateBackend(r.Context(), backend); err != nil {
			s.redirectWithNotice(w, r, "Failed to update backend: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, "Backend changes saved.")
	case "delete":
		if err := s.store.DeleteBackend(r.Context(), id); err != nil {
			s.redirectWithNotice(w, r, "Failed to delete backend: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, "Backend deleted.")
	case "probe":
		if err := s.service.ProbeBackend(r.Context(), id); err != nil {
			s.redirectWithNotice(w, r, "Health probe failed: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, "Health probe updated.")
	case "status":
		if err := r.ParseForm(); err != nil {
			s.redirectWithNotice(w, r, "Could not read the status update request.")
			return
		}
		if err := s.store.SetBackendStatus(r.Context(), id, r.FormValue("status")); err != nil {
			s.redirectWithNotice(w, r, "Failed to update backend status: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, "Backend status updated.")
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNotice(w, r, "Could not read the API key form.")
		return
	}
	allowed := splitCSV(r.FormValue("allowed_models"))
	rawKey, err := s.store.CreateAPIKey(r.Context(), r.FormValue("name"), allowed, r.FormValue("enabled") != "")
	if err != nil {
		s.redirectWithNotice(w, r, "Failed to create API key: "+err.Error())
		return
	}
	s.setFlash(w, "notice", "API key created.")
	s.setFlash(w, "created_key", rawKey)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAPIKeyAction(w http.ResponseWriter, r *http.Request) {
	id, action, ok := parseIDAction(r.URL.Path, "/admin/api-keys/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "toggle":
		if err := r.ParseForm(); err != nil {
			s.redirectWithNotice(w, r, "Could not read the API key update request.")
			return
		}
		enabled := r.FormValue("enabled") == "1"
		if err := s.store.SetAPIKeyEnabled(r.Context(), id, enabled); err != nil {
			s.redirectWithNotice(w, r, "Failed to update API key: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, "API key status updated.")
	case "delete":
		if err := s.store.DeleteAPIKey(r.Context(), id); err != nil {
			s.redirectWithNotice(w, r, "Failed to delete API key: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, "API key deleted.")
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	exportDoc, err := s.store.ExportConfiguration(r.Context())
	if err != nil {
		s.writeServerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="ai-gateway-config.json"`)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(exportDoc)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		s.redirectWithNotice(w, r, "Could not read the import file.")
		return
	}
	file, _, err := r.FormFile("config_file")
	if err != nil {
		s.redirectWithNotice(w, r, "Choose a JSON file to import.")
		return
	}
	defer file.Close()

	var document storage.ExportDocument
	if err := json.NewDecoder(file).Decode(&document); err != nil {
		s.redirectWithNotice(w, r, "Import file is not valid JSON.")
		return
	}
	if err := s.importConfiguration(r.Context(), document); err != nil {
		s.redirectWithNotice(w, r, "Failed to import configuration: "+err.Error())
		return
	}
	s.redirectWithNotice(w, r, "Configuration imported.")
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		s.writeServerError(w, err)
		return
	}
	models, err := s.store.ListLogicalModels(r.Context())
	if err != nil {
		s.writeServerError(w, err)
		return
	}
	backends, err := s.store.ListBackends(r.Context())
	if err != nil {
		s.writeServerError(w, err)
		return
	}
	apiKeys, err := s.store.ListAPIKeys(r.Context())
	if err != nil {
		s.writeServerError(w, err)
		return
	}
	logs, err := s.store.RecentLogs(r.Context(), 50)
	if err != nil {
		s.writeServerError(w, err)
		return
	}

	views := make([]apiKeyView, 0, len(apiKeys))
	for _, item := range apiKeys {
		views = append(views, apiKeyView{
			ID:            item.ID,
			Name:          item.Name,
			AllowedModels: strings.Join(parseAllowedModels(item.AllowedModels), ", "),
			Enabled:       item.Enabled,
			LastUsedAt:    item.LastUsedAt,
			CreatedAt:     item.CreatedAt,
		})
	}

	data := dashboardData{
		Title:          s.cfg.Title,
		Version:        version.Value,
		Stats:          stats,
		Models:         models,
		Backends:       backends,
		APIKeys:        views,
		Logs:           logs,
		Notice:         s.popFlash(w, r, "notice"),
		CreatedKey:     s.popFlash(w, r, "created_key"),
		AllowAnonymous: s.cfg.AllowAnonymous,
		AdminProtected: s.cfg.AdminAuthEnabled(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		s.writeServerError(w, err)
	}
}

func (s *Server) importConfiguration(ctx context.Context, document storage.ExportDocument) error {
	modelMap := map[string]int64{}
	for _, model := range document.LogicalModels {
		modelID, err := s.store.UpsertLogicalModel(ctx, model.Name, model.Description, model.Status)
		if err != nil {
			return err
		}
		modelMap[model.Name] = modelID
	}

	for _, backend := range document.Backends {
		logicalModelID, ok := modelMap[backend.LogicalModelName]
		if !ok {
			if backend.LogicalModelName == "" {
				return fmt.Errorf("backend %s is missing logical_model_name", backend.Name)
			}
			modelID, err := s.store.UpsertLogicalModel(ctx, backend.LogicalModelName, "", "active")
			if err != nil {
				return err
			}
			modelMap[backend.LogicalModelName] = modelID
			logicalModelID = modelID
		}
		backend.LogicalModelID = logicalModelID
		if err := s.store.UpsertBackend(ctx, backend); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) api(next func(http.ResponseWriter, *http.Request, *storage.APIKeyAuth)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, err := s.authenticateAPIKey(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-gateway"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"Bearer API key is required"}}`)
			return
		}
		if auth != nil {
			_ = s.store.TouchAPIKey(r.Context(), auth.ID)
		}
		next(w, r, auth)
	}
}

func (s *Server) authenticateAPIKey(r *http.Request) (*storage.APIKeyAuth, error) {
	headerValue := strings.TrimSpace(r.Header.Get("Authorization"))
	bearerToken := ""
	if strings.HasPrefix(headerValue, "Bearer ") {
		bearerToken = strings.TrimSpace(strings.TrimPrefix(headerValue, "Bearer "))
	}
	if bearerToken == "" {
		if s.cfg.AllowAnonymous {
			return nil, nil
		}
		return nil, errors.New("missing token")
	}
	auth, err := s.store.AuthenticateAPIKey(r.Context(), bearerToken)
	if err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, errors.New("invalid token")
	}
	return auth, nil
}

func (s *Server) admin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.AdminAuthEnabled() {
			next(w, r)
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != s.cfg.AdminUsername || password != s.cfg.AdminPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="ai-gateway-admin"`)
			http.Error(w, "admin authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) redirectWithNotice(w http.ResponseWriter, r *http.Request, message string) {
	s.setFlash(w, "notice", message)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) setFlash(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "agw_" + name,
		Value:    url.QueryEscape(value),
		Path:     "/",
		HttpOnly: true,
		MaxAge:   300,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) popFlash(w http.ResponseWriter, r *http.Request, name string) string {
	cookie, err := r.Cookie("agw_" + name)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookie.Name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		SameSite: http.SameSiteLaxMode,
	})
	value, _ := url.QueryUnescape(cookie.Value)
	return value
}

func (s *Server) writeServerError(w http.ResponseWriter, err error) {
	log.Printf("server error: %v", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}

func parseInt64(value string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseAllowedModels(raw string) []string {
	if raw == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	return values
}

func parseIDAction(path, prefix string) (int64, string, bool) {
	trimmed := strings.TrimPrefix(path, prefix)
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return id, parts[1], true
}

func statusClass(value string) string {
	switch value {
	case "active", "healthy", "enabled":
		return "status-good"
	case "draining", "unknown":
		return "status-warn"
	case "inactive", "disabled", "unhealthy":
		return "status-bad"
	default:
		return "status-neutral"
	}
}

func shortTime(value string) string {
	if value == "" {
		return "-"
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.Local().Format("2006-01-02 15:04:05")
}
