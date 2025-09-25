package main

import (
	"log"
	"os"
	"strings"
	"time"

	"auralis_back/agents"
	"auralis_back/authorization"
	"auralis_back/live2d"
	"auralis_back/llm"
	"auralis_back/tts"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func mustLoadEnv() {
	_ = godotenv.Load()
}

func configureCORS(r *gin.Engine) {
	rawOrigins := strings.Split(os.Getenv("CORS_ALLOWED_ORIGINS"), ",")
	allowOrigins := make([]string, 0, len(rawOrigins))
	for _, origin := range rawOrigins {
		trimmed := strings.TrimSpace(origin)
		if trimmed != "" {
			allowOrigins = append(allowOrigins, trimmed)
		}
	}

	if len(allowOrigins) == 0 {
		allowOrigins = []string{
			"http://localhost:3000",
			"https://localhost:3000",
		}
	}

	corsConfig := cors.Config{
		AllowOrigins:     allowOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Requested-With"},
		ExposeHeaders:    []string{"Content-Length", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}

	r.Use(cors.New(corsConfig))
}

func main() {
	mustLoadEnv()

	r := gin.Default()
	configureCORS(r)

	authModule, err := authorization.RegisterRoutes(r)
	if err != nil {
		log.Fatalf("register auth routes: %v", err)
	}

	var authGuard *authorization.Guard
	if authModule != nil {
		authGuard = authModule.Guard()
	}

	if _, err := agents.RegisterRoutes(r, authGuard); err != nil {
		log.Fatalf("register agent routes: %v", err)
	}
	if _, err := live2d.RegisterRoutes(r, authGuard); err != nil {
		log.Fatalf("register live2d routes: %v", err)
	}
	ttsModule, err := tts.RegisterRoutes(r)
	if err != nil {
		log.Fatalf("register tts routes: %v", err)
	}
	if _, err := llm.RegisterRoutes(r, ttsModule); err != nil {
		log.Fatalf("register llm routes: %v", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if err := r.Run(":" + port); err != nil {
		log.Fatalf("start server: %v", err)
	}
}
