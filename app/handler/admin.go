package handler

import (
	"net/http"

	"gate-service/app/billing"

	"github.com/gin-gonic/gin"
)

// SettleRequest is the body of POST /admin/billing/settle (Phase 6, T-F3).
type SettleRequest struct {
	RequestID string `json:"request_id"`
	Action    string `json:"action"` // "billed" | "cancelled"
}

// SettleUsageHandlerFactory returns a handler that transitions a pending usage
// ledger entry to a terminal state via the billing backend's QuotaService.
func SettleUsageHandlerFactory(billingSvc billing.BillingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req SettleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}

		quotaSvc, ok := billingSvc.(billing.QuotaService)
		if !ok {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "settlement not supported by billing backend"})
			return
		}

		if err := quotaSvc.SettleUsage(req.RequestID, req.Action); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "ok", "request_id": req.RequestID, "action": req.Action})
	}
}
