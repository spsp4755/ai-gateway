package storage

type LogicalModel struct {
	ID          int64
	Name        string
	Description string
	Status      string
	CreatedAt   string
	UpdatedAt   string
}

type Backend struct {
	ID                int64
	LogicalModelID    int64
	LogicalModelName  string
	Name              string
	BaseURL           string
	UpstreamModelName string
	APIKey            string
	HealthcheckPath   string
	Priority          int
	Weight            int
	TimeoutMs         int
	Status            string
	HealthStatus      string
	LastError         string
	LastCheckedAt     string
	CreatedAt         string
	UpdatedAt         string
}

type APIKeyRecord struct {
	ID            int64
	Name          string
	KeyHash       string
	AllowedModels string
	Enabled       bool
	LastUsedAt    string
	CreatedAt     string
}

type APIKeyAuth struct {
	ID            int64
	Name          string
	AllowedModels []string
}

type RequestLog struct {
	ID               int64
	RequestID        string
	APIKeyName       string
	LogicalModelName string
	BackendName      string
	BackendURL       string
	Streamed         bool
	StatusCode       int
	LatencyMs        int64
	PromptTokens     *int
	CompletionTokens *int
	TotalTokens      *int
	Success          bool
	ErrorMessage     string
	CreatedAt        string
}

type Stats struct {
	ModelsCount    int
	BackendsCount  int
	HealthyBackends int
	RequestCount   int
}

