package main

import (
	"log"
	"os"

	"auralis_back/authorization"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func mustLoadEnv() {
	_ = godotenv.Load()
}

func main() {
	mustLoadEnv()

	r := gin.Default()

	if _, err := authorization.RegisterRoutes(r); err != nil {
		log.Fatalf("register auth routes: %v", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if err := r.Run(":" + port); err != nil {
		log.Fatalf("start server: %v", err)
	}
}
