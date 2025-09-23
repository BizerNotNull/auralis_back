package authorization

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const (
	identityKey    = "user_id"
	defaultTimeout = time.Hour
)

var (
	ErrUsernameTaken = errors.New("authorization: username already exists")
	ErrWeakPassword  = errors.New("authorization: password must be at least 6 characters")
)

// Module wires together the JWT middleware and backing services.
type Module struct {
	db            *gorm.DB
	userStore     *UserStore
	jwtMiddleware *jwt.GinJWTMiddleware
}

// RegisterRoutes bootstraps the authentication endpoints under /auth.
func RegisterRoutes(router *gin.Engine) (*Module, error) {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_DSN"))
	if dsn == "" {
		return nil, errors.New("authorization: DATABASE_DSN environment variable is required")
	}

	driver := strings.TrimSpace(os.Getenv("DATABASE_DRIVER"))
	if driver == "" {
		driver = inferDriverFromDSN(dsn)
		if driver == "" {
			return nil, errors.New("authorization: DATABASE_DRIVER environment variable is required when DSN does not contain a scheme")
		}
	}

	db, err := openDatabase(driver, dsn)
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(&User{}, &Role{}, &UserRole{}); err != nil {
		return nil, fmt.Errorf("authorization: migrate models: %w", err)
	}

	userStore := &UserStore{db: db}
	authService := &AuthService{users: userStore}

	middleware, err := buildJWTMiddleware(authService)
	if err != nil {
		return nil, err
	}

	authGroup := router.Group("/auth")
	authGroup.POST("/register", func(c *gin.Context) {
		var req RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
			return
		}

		user, err := authService.Register(c.Request.Context(), req.Username, req.Password)
		if err != nil {
			switch {
			case errors.Is(err, jwt.ErrMissingLoginValues):
				c.JSON(http.StatusBadRequest, gin.H{"error": "username and password are required"})
			case errors.Is(err, ErrWeakPassword):
				c.JSON(http.StatusBadRequest, gin.H{"error": ErrWeakPassword.Error()})
			case errors.Is(err, ErrUsernameTaken):
				c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register"})
			}
			return
		}

		c.JSON(http.StatusCreated, gin.H{
			"id":       user.ID,
			"username": user.Username,
		})
	})

	authGroup.POST("/login", middleware.LoginHandler)
	authGroup.POST("/refresh", middleware.RefreshHandler)

	secured := authGroup.Group("")
	secured.Use(middleware.MiddlewareFunc())
	secured.GET("/profile", func(c *gin.Context) {
		claims := jwt.ExtractClaims(c)
		c.JSON(200, gin.H{
			"id":       claims[identityKey],
			"username": claims["username"],
			"roles":    claims["roles"],
		})
	})

	return &Module{db: db, userStore: userStore, jwtMiddleware: middleware}, nil
}

func (m *Module) Middleware() gin.HandlerFunc {
	if m == nil || m.jwtMiddleware == nil {
		return nil
	}
	return m.jwtMiddleware.MiddlewareFunc()
}

func openDatabase(driver, dsn string) (*gorm.DB, error) {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pg":
		return gorm.Open(postgres.Open(dsn), &gorm.Config{NowFunc: func() time.Time { return time.Now().UTC() }})
	case "mysql":
		return gorm.Open(mysql.Open(dsn), &gorm.Config{NowFunc: func() time.Time { return time.Now().UTC() }})
	case "sqlite", "sqlite3":
		return gorm.Open(sqlite.Open(dsn), &gorm.Config{NowFunc: func() time.Time { return time.Now().UTC() }})
	default:
		return nil, fmt.Errorf("authorization: unsupported database driver %q", driver)
	}
}

func inferDriverFromDSN(dsn string) string {
	lower := strings.ToLower(dsn)
	switch {
	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		return "postgres"
	case strings.HasPrefix(lower, "mysql://"):
		return "mysql"
	case strings.HasPrefix(lower, "sqlite://"), strings.HasSuffix(lower, ".db"), strings.HasSuffix(lower, ".sqlite"):
		return "sqlite"
	default:
		return ""
	}
}

func buildJWTMiddleware(service *AuthService) (*jwt.GinJWTMiddleware, error) {
	secret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
	if secret == "" {
		return nil, errors.New("authorization: JWT_SECRET environment variable is required")
	}

	return jwt.New(&jwt.GinJWTMiddleware{
		Realm:       "auralis",
		Key:         []byte(secret),
		Timeout:     defaultTimeout,
		MaxRefresh:  24 * time.Hour,
		IdentityKey: identityKey,
		PayloadFunc: func(data interface{}) jwt.MapClaims {
			if user, ok := data.(*AuthenticatedUser); ok {
				return jwt.MapClaims{
					identityKey: user.ID,
					"username":  user.Username,
					"roles":     user.Roles,
				}
			}
			return jwt.MapClaims{}
		},
		IdentityHandler: func(c *gin.Context) interface{} {
			claims := jwt.ExtractClaims(c)
			idValue, _ := claims[identityKey]
			username, _ := claims["username"].(string)

			var id uint
			switch v := idValue.(type) {
			case float64:
				id = uint(v)
			case int64:
				id = uint(v)
			case uint64:
				id = uint(v)
			case int:
				id = uint(v)
			case uint:
				id = v
			}

			rolesValue, _ := claims["roles"].([]interface{})
			roles := make([]string, 0, len(rolesValue))
			for _, role := range rolesValue {
				if name, ok := role.(string); ok {
					roles = append(roles, name)
				}
			}

			return &AuthenticatedUser{ID: id, Username: username, Roles: roles}
		},
		Authenticator: func(c *gin.Context) (interface{}, error) {
			var req LoginRequest
			if err := c.ShouldBindJSON(&req); err != nil {
				return nil, jwt.ErrMissingLoginValues
			}

			user, err := service.Authenticate(c.Request.Context(), req.Username, req.Password)
			if err != nil {
				return nil, err
			}

			return user, nil
		},
		Authorizator: func(data interface{}, c *gin.Context) bool {
			_, ok := data.(*AuthenticatedUser)
			return ok
		},
		Unauthorized: func(c *gin.Context, code int, message string) {
			c.JSON(code, gin.H{"error": message})
		},
		TokenLookup:   "header: Authorization, cookie: jwt",
		TokenHeadName: "Bearer",
		TimeFunc:      time.Now,
	})
}

// LoginRequest represents the expected payload for the login endpoint.
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// RegisterRequest captures the payload for user registration.
type RegisterRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required,min=6"`
}

// AuthenticatedUser is the minimal identity stored inside JWT claims.
type AuthenticatedUser struct {
	ID       uint
	Username string
	Roles    []string
}

// AuthService handles authentication concerns.
type AuthService struct {
	users *UserStore
}

// Authenticate validates the given credentials and returns an authenticated user.
func (s *AuthService) Authenticate(ctx context.Context, username, password string) (*AuthenticatedUser, error) {
	if strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return nil, jwt.ErrMissingLoginValues
	}

	user, err := s.users.FindByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, jwt.ErrFailedAuthentication
		}
		return nil, fmt.Errorf("authorization: authenticate user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, jwt.ErrFailedAuthentication
	}

	roleNames, err := s.users.FindRoleNames(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("authorization: load roles: %w", err)
	}

	return &AuthenticatedUser{ID: user.ID, Username: user.Username, Roles: roleNames}, nil
}

// Register creates a new user with the provided credentials.
func (s *AuthService) Register(ctx context.Context, username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)

	if username == "" || password == "" {
		return nil, jwt.ErrMissingLoginValues
	}
	if len(password) < 6 {
		return nil, ErrWeakPassword
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("authorization: hash password: %w", err)
	}

	user := &User{Username: username, PasswordHash: string(hash)}
	if err := s.users.Create(ctx, user); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, ErrUsernameTaken
		}
		return nil, fmt.Errorf("authorization: create user: %w", err)
	}

	return user, nil
}

// UserStore provides data access helpers backed by GORM.
type UserStore struct {
	db *gorm.DB
}

// FindByUsername loads a user by unique username.
func (s *UserStore) FindByUsername(ctx context.Context, username string) (*User, error) {
	var user User
	result := s.db.WithContext(ctx).Where("username = ?", username).First(&user)
	if result.Error != nil {
		return nil, result.Error
	}
	return &user, nil
}

// Create inserts a new user record.
func (s *UserStore) Create(ctx context.Context, user *User) error {
	return s.db.WithContext(ctx).Create(user).Error
}

// FindRoleNames returns the roles assigned to the given user.
func (s *UserStore) FindRoleNames(ctx context.Context, userID uint) ([]string, error) {
	var roles []string
	err := s.db.WithContext(ctx).
		Model(&Role{}).
		Select("roles.name").
		Joins("JOIN user_roles ON user_roles.role_id = roles.id").
		Where("user_roles.user_id = ?", userID).
		Scan(&roles).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return []string{}, nil
		}
		return nil, err
	}
	return roles, nil
}

// User represents an application account.
type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;size:64;not null"`
	PasswordHash string `gorm:"size:255;not null"`
	Status       string `gorm:"size:32;default:'active'"`
	LastLoginAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Role represents a collection of permissions assigned to users.
type Role struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"uniqueIndex;size:64;not null"`
	Code      string `gorm:"uniqueIndex;size:64;not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UserRole associates users with roles.
type UserRole struct {
	ID        uint `gorm:"primaryKey"`
	UserID    uint `gorm:"uniqueIndex:idx_user_role;not null"`
	RoleID    uint `gorm:"uniqueIndex:idx_user_role;not null"`
	CreatedAt time.Time
}
