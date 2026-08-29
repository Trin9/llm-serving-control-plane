package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gate-service/app/billing"

	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

type authTestBillingService struct{}

func (authTestBillingService) ReportUsage(billing.UsageRecord) error { return nil }
func (authTestBillingService) Start()                                 {}
func (authTestBillingService) Stop()                                  {}

func TestAuthMiddleware_RejectsJWTWhenSecretIsUnset(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(AuthMiddleware(authTestBillingService{}))
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"userID": "user-1"})
	signedToken, err := token.SignedString([]byte("dev-only-secret-change-me"))
	assert.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+signedToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	assert.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestAuthMiddleware_AcceptsConfiguredHMACJWT(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-secret")
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(AuthMiddleware(authTestBillingService{}))
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"userID": "user-1"})
	signedToken, err := token.SignedString([]byte("test-secret"))
	assert.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+signedToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	assert.Equal(t, http.StatusNoContent, response.Code)
}