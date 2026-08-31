package billing

import (
	"time"
)

// UsageRecord defines billing data for a single request
type UsageRecord struct {
	RequestID string `json:"request_id"`
	TraceID   string `json:"trace_id"`
	Model     string `json:"model"`
	User      string `json:"user"`       // Can be extended to UserID or API Key
	OrgID     string `json:"org_id"`     // Organization ID
	ProjectID string `json:"project_id"` // Project ID

	// PromptTokens / CompletionTokens are the split from vLLM usage when available.
	// TotalTokens equals PromptTokens + CompletionTokens and is the value deducted.
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// UsageSource is "official" when the value came from vLLM usage, or "estimated"
	// when it was derived from SSE chunk counting (an approximation, not exact tokens).
	UsageSource string `json:"usage_source"`

	// RequestStatus records how the upstream request finished:
	// "completed", "client_disconnected", "upstream_failed", or "timed_out".
	RequestStatus string `json:"request_status"`

	// State is the settlement state of this ledger entry:
	// "billed", "refunded". It is set by the billing backend.
	State     string    `json:"state"`
	Timestamp time.Time `json:"timestamp"`
}

// APIKeyInfo contains metadata for an authenticated API key
type APIKeyInfo struct {
	Fingerprint string    `json:"fingerprint"`
	OrgID       string    `json:"org_id"`
	ProjectID   string    `json:"project_id"`
	Status      string    `json:"status"` // "active", "suspended", "revoked"
	Name        string    `json:"name"`   // Human-readable name for the API key
	CreatedAt   time.Time `json:"created_at"`
}

// BillingService defines the interface for billing services
// Can be implemented by MemoryBillingService or RedisBillingService
type BillingService interface {
	// ReportUsage reports a billing record (async or sync depending on implementation)
	ReportUsage(record UsageRecord) error

	// Start starts background processing tasks (like workers)
	Start()

	// Stop gracefully stops the service
	Stop()
}

// QuotaService extends BillingService with quota management capabilities
// This interface is implemented by Redis-based billing for multi-tenant quota enforcement
type QuotaService interface {
	BillingService

	// AuthenticateAPIKey validates an API key and returns org/project metadata
	// Returns error if the key is invalid or inactive
	AuthenticateAPIKey(apiKey string) (*APIKeyInfo, error)

	// CheckQuota verifies if the org and project have sufficient quota
	// This is a read-only pre-check before processing the request
	// Returns error if quota is insufficient
	CheckQuota(orgID, projectID string, estimatedTokens int) error

	// CreateAPIKey creates a new API key in Redis with metadata
	// This is typically called by an admin API or setup script
	CreateAPIKey(apiKey, orgID, projectID, name string) error

	// SetQuota sets the quota for an organization or project
	// This is typically called by an admin API or billing system
	SetOrgQuota(orgID string, tokens int) error
	SetProjectQuota(projectID string, tokens int) error

	// GetQuota retrieves current quota balance
	GetOrgQuota(orgID string) (int, error)
	GetProjectQuota(projectID string) (int, error)
}
