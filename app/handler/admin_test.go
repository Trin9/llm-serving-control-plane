package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gate-service/app/billing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func performSettleRequest(t *testing.T, svc billing.BillingService, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/admin/billing/settle", SettleUsageHandlerFactory(svc))

	req := httptest.NewRequest(http.MethodPost, "/admin/billing/settle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSettleUsageHandler_OK(t *testing.T) {
	mockQuota := new(MockQuotaService)
	mockQuota.On("SettleUsage", "req-1", "billed").Return(nil)

	body, _ := json.Marshal(SettleRequest{RequestID: "req-1", Action: "billed"})
	w := performSettleRequest(t, mockQuota, string(body))

	assert.Equal(t, http.StatusOK, w.Code)
	mockQuota.AssertExpectations(t)
}

func TestSettleUsageHandler_SettleError(t *testing.T) {
	mockQuota := new(MockQuotaService)
	mockQuota.On("SettleUsage", "req-1", "billed").Return(errors.New("request req-1 is not in pending state"))

	body, _ := json.Marshal(SettleRequest{RequestID: "req-1", Action: "billed"})
	w := performSettleRequest(t, mockQuota, string(body))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	mockQuota.AssertExpectations(t)
}

func TestSettleUsageHandler_InvalidBody(t *testing.T) {
	mockQuota := new(MockQuotaService)
	w := performSettleRequest(t, mockQuota, "{not-json")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSettleUsageHandler_UnsupportedBackend(t *testing.T) {
	// MemoryBillingService implements BillingService but NOT QuotaService.
	mem := billing.NewMemoryBillingService(10)
	body, _ := json.Marshal(SettleRequest{RequestID: "req-1", Action: "billed"})
	w := performSettleRequest(t, mem, string(body))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
