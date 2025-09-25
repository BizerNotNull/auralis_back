package authorization

import (
	"fmt"
	"net/http"
	"strings"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
)

type Guard struct {
	jwt *jwt.GinJWTMiddleware
}

// NewGuard builds a Guard helper around the provided JWT middleware instance.
func NewGuard(jwtMiddleware *jwt.GinJWTMiddleware) *Guard {
	if jwtMiddleware == nil {
		return nil
	}
	return &Guard{jwt: jwtMiddleware}
}

// Guard exposes reusable authorization helpers for other modules.
func (m *Module) Guard() *Guard {
	if m == nil {
		return nil
	}
	return NewGuard(m.jwtMiddleware)
}

// RequireAuthenticated ensures the request carries a valid JWT.
func (g *Guard) RequireAuthenticated() gin.HandlerFunc {
	if g == nil || g.jwt == nil {
		return func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		}
	}
	return g.jwt.MiddlewareFunc()
}

// RequireAnyRole authorises requests that own at least one of the provided roles.
func (g *Guard) RequireAnyRole(roles ...string) gin.HandlerFunc {
	normalized := make([]string, 0, len(roles))
	for _, role := range roles {
		trimmed := strings.ToLower(strings.TrimSpace(role))
		if trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}

	if len(normalized) == 0 {
		return func(c *gin.Context) {
			c.Next()
		}
	}

	humanReadable := make([]string, 0, len(roles))
	for _, role := range roles {
		trimmed := strings.TrimSpace(role)
		if trimmed != "" {
			humanReadable = append(humanReadable, trimmed)
		}
	}

	return func(c *gin.Context) {
		claims := jwt.ExtractClaims(c)
		if len(claims) == 0 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		currentRoles := extractRoles(claims)
		for _, has := range currentRoles {
			candidate := strings.ToLower(strings.TrimSpace(has))
			for _, expected := range normalized {
				if candidate == expected {
					c.Next()
					return
				}
			}
		}

		message := "insufficient privileges"
		if len(humanReadable) == 1 {
			message = fmt.Sprintf("%s role required", humanReadable[0])
		} else if len(humanReadable) > 1 {
			message = fmt.Sprintf("one of [%s] roles required", strings.Join(humanReadable, ", "))
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": message})
	}
}

// RequireRole authorises requests that own the specified role.
func (g *Guard) RequireRole(role string) gin.HandlerFunc {
	return g.RequireAnyRole(role)
}
