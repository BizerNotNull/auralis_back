package authorization

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	filestore "auralis_back/storage"
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

const userAvatarURLExpiry = 15 * time.Minute

var (
	ErrUsernameTaken      = errors.New("authorization: username already exists")
	ErrWeakPassword       = errors.New("authorization: password must be at least 6 characters")
	ErrInvalidDisplayName = errors.New("authorization: display name cannot be empty")
)

// Module wires together the JWT middleware and backing services.
type Module struct {
	db            *gorm.DB
	userStore     *UserStore
	jwtMiddleware *jwt.GinJWTMiddleware
	captcha       *CaptchaStore
	avatarStorage *filestore.AvatarStorage
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
	captchaStore := NewCaptchaStore(3 * time.Minute)
	avatarStore, err := filestore.NewAvatarStorageFromEnv()
	if err != nil {
		return nil, err
	}
	authService := &AuthService{users: userStore}

	middleware, err := buildJWTMiddleware(authService, avatarStore)
	if err != nil {
		return nil, err
	}

	authGroup := router.Group("/auth")
	authGroup.GET("/captcha", func(c *gin.Context) {
		challenge := captchaStore.Issue()
		expiresIn := int(challenge.TTL.Seconds())
		if expiresIn < 1 {
			expiresIn = int(time.Until(challenge.ExpiresAt).Seconds())
			if expiresIn < 1 {
				expiresIn = 1
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"captcha_id": challenge.ID,
			"question":   challenge.Question,
			"expires_in": expiresIn,
			"expires_at": challenge.ExpiresAt.UTC(),
		})
	})
	authGroup.POST("/register", func(c *gin.Context) {
		var req RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
			return
		}

		if captchaStore != nil && !captchaStore.Verify(req.CaptchaID, req.CaptchaAnswer) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid captcha"})
			return
		}

		displayName := strings.TrimSpace(req.DisplayName)
		if displayName == "" {
			displayName = req.Username
		}

		ctx := c.Request.Context()
		user, err := authService.Register(ctx, req.Username, req.Password, displayName, req.AvatarURL, req.Bio)
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

		roles, err := userStore.FindRoleNames(ctx, user.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user roles"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"user": buildUserPayload(c.Request.Context(), avatarStore, user, roles)})
	})

	authGroup.POST("/login", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		if len(body) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
			return
		}

		var req LoginRequest
		if err := json.Unmarshal(body, &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
			return
		}

		if captchaStore != nil && !captchaStore.Verify(req.CaptchaID, req.CaptchaAnswer) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid captcha"})
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		middleware.LoginHandler(c)
	})
	authGroup.POST("/refresh", middleware.RefreshHandler)

	secured := authGroup.Group("")
	secured.Use(middleware.MiddlewareFunc())
	secured.GET("/profile", func(c *gin.Context) {
		claims := jwt.ExtractClaims(c)
		userID := extractUserID(claims)
		if userID == 0 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		ctx := c.Request.Context()
		user, err := userStore.FindByID(ctx, userID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
			return
		}

		roles, err := userStore.FindRoleNames(ctx, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load roles"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"user": buildUserPayload(ctx, avatarStore, user, roles)})
	})

	secured.PUT("/profile", func(c *gin.Context) {
		var req UpdateProfileRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
			return
		}
		if req.DisplayName == nil && req.AvatarURL == nil && req.Bio == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
			return
		}

		claims := jwt.ExtractClaims(c)
		userID := extractUserID(claims)
		if userID == 0 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		ctx := c.Request.Context()
		updated, err := userStore.UpdateProfile(ctx, userID, UpdateProfileParams{
			DisplayName: req.DisplayName,
			AvatarURL:   req.AvatarURL,
			Bio:         req.Bio,
		})
		if err != nil {
			switch {
			case errors.Is(err, ErrInvalidDisplayName):
				c.JSON(http.StatusBadRequest, gin.H{"error": ErrInvalidDisplayName.Error()})
			case errors.Is(err, gorm.ErrRecordNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update profile"})
			}
			return
		}

		roles, err := userStore.FindRoleNames(ctx, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load roles"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"user": buildUserPayload(ctx, avatarStore, updated, roles)})
	})

	secured.POST("/profile/avatar", func(c *gin.Context) {
		if avatarStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "avatar upload not configured"})
			return
		}

		claims := jwt.ExtractClaims(c)
		userID := extractUserID(claims)
		if userID == 0 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		file, err := c.FormFile("avatar")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "avatar file is required"})
			return
		}

		ctx := c.Request.Context()
		existing, err := userStore.FindByID(ctx, userID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
			}
			return
		}

		var oldAvatar string
		if existing.AvatarURL != nil {
			oldAvatar = strings.TrimSpace(*existing.AvatarURL)
		}

		uploaded, err := avatarStore.Upload(ctx, file, "users", fmt.Sprintf("%d", userID))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to upload avatar", "details": err.Error()})
			return
		}

		updated, err := userStore.UpdateProfile(ctx, userID, UpdateProfileParams{AvatarURL: &uploaded})
		if err != nil {
			_ = avatarStore.Remove(ctx, uploaded)
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update profile"})
			}
			return
		}

		if oldAvatar != "" && oldAvatar != uploaded {
			_ = avatarStore.Remove(ctx, oldAvatar)
		}

		roles, err := userStore.FindRoleNames(ctx, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load roles"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"user": buildUserPayload(ctx, avatarStore, updated, roles)})
	})

	return &Module{db: db, userStore: userStore, jwtMiddleware: middleware, captcha: captchaStore, avatarStorage: avatarStore}, nil
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

func buildJWTMiddleware(service *AuthService, store *filestore.AvatarStorage) (*jwt.GinJWTMiddleware, error) {
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

			c.Set("authenticated_user", user)

			return user, nil
		},
		Authorizator: func(data interface{}, c *gin.Context) bool {
			_, ok := data.(*AuthenticatedUser)
			return ok
		},
		Unauthorized: func(c *gin.Context, code int, message string) {
			c.JSON(code, gin.H{"error": message})
		},
		LoginResponse: func(c *gin.Context, code int, token string, expire time.Time) {
			response := gin.H{
				"token":  token,
				"expire": expire,
			}

			if value, ok := c.Get("authenticated_user"); ok {
				if authUser, ok := value.(*AuthenticatedUser); ok && authUser != nil {
					if user, err := service.users.FindByID(c.Request.Context(), authUser.ID); err == nil {
						roles := authUser.Roles
						if roles == nil {
							roles = []string{}
						}
						response["user"] = buildUserPayload(c.Request.Context(), store, user, roles)
					}
				}
			} else {
				claims := jwt.ExtractClaims(c)
				userID := extractUserID(claims)
				if userID != 0 {
					if user, err := service.users.FindByID(c.Request.Context(), userID); err == nil {
						roles := extractRoles(claims)
						response["user"] = buildUserPayload(c.Request.Context(), store, user, roles)
					}
				}
			}

			c.JSON(code, response)
		},
		RefreshResponse: func(c *gin.Context, code int, token string, expire time.Time) {
			response := gin.H{
				"token":  token,
				"expire": expire,
			}

			claims := jwt.ExtractClaims(c)
			userID := extractUserID(claims)
			roles := extractRoles(claims)

			if userID != 0 {
				if user, err := service.users.FindByID(c.Request.Context(), userID); err == nil {
					response["user"] = buildUserPayload(c.Request.Context(), store, user, roles)
				}
			}

			c.JSON(code, response)
		},
		TokenLookup:   "header: Authorization, cookie: jwt, cookie: token",
		TokenHeadName: "Bearer",
		TimeFunc:      time.Now,
	})
}

// LoginRequest represents the expected payload for the login endpoint.
type LoginRequest struct {
	Username      string `json:"username" binding:"required"`
	Password      string `json:"password" binding:"required"`
	CaptchaID     string `json:"captcha_id" binding:"required"`
	CaptchaAnswer string `json:"captcha_answer" binding:"required"`
}

// RegisterRequest captures the payload for user registration.
type RegisterRequest struct {
	Username      string  `json:"username" binding:"required"`
	Password      string  `json:"password" binding:"required,min=6"`
	DisplayName   string  `json:"display_name"`
	CaptchaID     string  `json:"captcha_id" binding:"required"`
	CaptchaAnswer string  `json:"captcha_answer" binding:"required"`
	AvatarURL     *string `json:"avatar_url"`
	Bio           *string `json:"bio"`
}

// UpdateProfileRequest captures profile update fields.
type UpdateProfileRequest struct {
	DisplayName *string `json:"display_name"`
	AvatarURL   *string `json:"avatar_url"`
	Bio         *string `json:"bio"`
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
func (s *AuthService) Register(ctx context.Context, username, password, displayName string, avatarURL, bio *string) (*User, error) {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	displayName = strings.TrimSpace(displayName)

	if username == "" || password == "" {
		return nil, jwt.ErrMissingLoginValues
	}
	if len(password) < 6 {
		return nil, ErrWeakPassword
	}
	if displayName == "" {
		displayName = username
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("authorization: hash password: %w", err)
	}

	var storedAvatar *string
	if avatarURL != nil {
		if trimmed := strings.TrimSpace(*avatarURL); trimmed != "" {
			value := trimmed
			storedAvatar = &value
		}
	}

	var storedBio *string
	if bio != nil {
		if trimmed := strings.TrimSpace(*bio); trimmed != "" {
			value := trimmed
			storedBio = &value
		}
	}

	user := &User{
		Username:     username,
		PasswordHash: string(hash),
		DisplayName:  displayName,
		AvatarURL:    storedAvatar,
		Bio:          storedBio,
	}
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

// UpdateProfileParams holds the fields eligible for profile updates.
type UpdateProfileParams struct {
	DisplayName *string
	AvatarURL   *string
	Bio         *string
}

// FindByID loads a user by primary key.
func (s *UserStore) FindByID(ctx context.Context, id uint) (*User, error) {
	if s == nil {
		return nil, errors.New("authorization: user store not initialized")
	}
	var user User
	result := s.db.WithContext(ctx).Where("id = ?", id).First(&user)
	if result.Error != nil {
		return nil, result.Error
	}
	return &user, nil
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

// UpdateProfile persists profile related fields for the given user id.
func (s *UserStore) UpdateProfile(ctx context.Context, userID uint, params UpdateProfileParams) (*User, error) {
	if s == nil {
		return nil, errors.New("authorization: user store not initialized")
	}

	updates := make(map[string]interface{})

	if params.DisplayName != nil {
		name := strings.TrimSpace(*params.DisplayName)
		if name == "" {
			return nil, ErrInvalidDisplayName
		}
		updates["display_name"] = name
	}

	if params.AvatarURL != nil {
		avatar := strings.TrimSpace(*params.AvatarURL)
		if avatar == "" {
			updates["avatar_url"] = nil
		} else {
			updates["avatar_url"] = avatar
		}
	}

	if params.Bio != nil {
		bio := strings.TrimSpace(*params.Bio)
		if bio == "" {
			updates["bio"] = nil
		} else {
			updates["bio"] = bio
		}
	}

	if len(updates) == 0 {
		return s.FindByID(ctx, userID)
	}

	updates["updated_at"] = time.Now().UTC()
	result := s.db.WithContext(ctx).Model(&User{}).Where("id = ?", userID).Updates(updates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}

	return s.FindByID(ctx, userID)
}

// User represents an application account.
type User struct {
	ID           uint    `gorm:"primaryKey"`
	Username     string  `gorm:"uniqueIndex;size:64;not null"`
	PasswordHash string  `gorm:"size:255;not null"`
	DisplayName  string  `gorm:"size:128;not null;default:''"`
	AvatarURL    *string `gorm:"size:255"`
	Bio          *string `gorm:"type:text"`
	Status       string  `gorm:"size:32;default:'active'"`
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

func extractUserID(claims jwt.MapClaims) uint {
	if claims == nil {
		return 0
	}
	idValue, ok := claims[identityKey]
	if !ok {
		return 0
	}

	switch v := idValue.(type) {
	case float64:
		return uint(v)
	case int64:
		return uint(v)
	case uint64:
		return uint(v)
	case int:
		return uint(v)
	case uint:
		return v
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return uint(parsed)
		}
	}
	return 0
}

func extractRoles(claims jwt.MapClaims) []string {
	if claims == nil {
		return []string{}
	}

	switch raw := claims["roles"].(type) {
	case []string:
		return append([]string{}, raw...)
	case []interface{}:
		roles := make([]string, 0, len(raw))
		for _, role := range raw {
			if name, ok := role.(string); ok {
				roles = append(roles, name)
			}
		}
		return roles
	default:
		return []string{}
	}
}

func buildUserPayload(ctx context.Context, store *filestore.AvatarStorage, user *User, roles []string) gin.H {
	if user == nil {
		return gin.H{}
	}

	avatarURL := ""
	if user.AvatarURL != nil {
		avatarURL = strings.TrimSpace(*user.AvatarURL)
		if store != nil {
			if signed, err := store.PresignedURL(ctx, avatarURL, userAvatarURLExpiry); err == nil && signed != "" {
				avatarURL = signed
			}
		}
	}

	bio := ""
	if user.Bio != nil {
		bio = *user.Bio
	}

	var avatarField interface{}
	if avatarURL != "" {
		avatarField = avatarURL
	}

	var bioField interface{}
	if bio != "" {
		bioField = bio
	}

	return gin.H{
		"id":            user.ID,
		"username":      user.Username,
		"display_name":  user.DisplayName,
		"avatar_url":    avatarField,
		"bio":           bioField,
		"status":        user.Status,
		"last_login_at": user.LastLoginAt,
		"created_at":    user.CreatedAt,
		"updated_at":    user.UpdatedAt,
		"roles":         roles,
	}
}
