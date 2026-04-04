package middleware

import (
	"gate-service/app/billing"
	"log"
	"net/http"
	"strings"

	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
)

// AuthMiddleware handles both API Key and JWT authentication
func AuthMiddleware(billingSvc billing.BillingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "No token"})
			return
		}

		// Extract the token from the "Bearer" scheme
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			log.Println("Invalid auth format, len(parts):", len(parts))
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token format"})
			return
		}
		tokenString := parts[1]

		// 1. Try API Key Authentication if QuotaService is available
		if quotaSvc, ok := billingSvc.(billing.QuotaService); ok {
			// Note: We don't check for "sk-" prefix to be flexible,
			// but we could if we wanted to avoid unnecessary Redis calls for JWTs.
			apiKeyInfo, err := quotaSvc.AuthenticateAPIKey(tokenString)
			if err == nil {
				// API Key valid
				c.Set("orgID", apiKeyInfo.OrgID)
				c.Set("projectID", apiKeyInfo.ProjectID)
				c.Set("authType", "api_key")

				// Optional: Check quota in middleware to fail fast
				if err := quotaSvc.CheckQuota(apiKeyInfo.OrgID, apiKeyInfo.ProjectID, 0); err != nil {
					c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
						"error":   "Quota exceeded",
						"message": err.Error(),
					})
					return
				}

				c.Next()
				return
			}
		}

		// 2. Fallback to JWT Authentication
		claims := jwt.MapClaims{}
		token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
			// Replace with actual secret management in production
			return []byte("a-string-secret-at-least-256-bits-long"), nil
		})

		if err == nil && token.Valid {
			userID, ok := claims["userID"].(string)
			if ok {
				c.Set("userID", userID)
				c.Set("authType", "jwt")
				c.Next()
				return
			}
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token/API key"})
	}
}
