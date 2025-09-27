package authorization

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"

	cache "auralis_back/cache"
	filestore "auralis_back/storage"
	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const (
	identityKey    = "user_id"
	defaultTimeout = 24 * time.Hour
)

const userAvatarURLExpiry = 15 * time.Minute

const defaultTokenBalance int64 = 100000
const maxPurchaseTokensPerRequest int64 = 1_000_000
const (
	userCacheTTL          = 5 * time.Minute
	userRolesCacheTTL     = 2 * time.Minute
	cacheOperationTimeout = 500 * time.Millisecond
)

var (
	ErrUsernameTaken      = errors.New("authorization: username already exists")
	ErrWeakPassword       = errors.New("authorization: password must be at least 6 characters")
	ErrInvalidDisplayName = errors.New("authorization: display name cannot be empty")
	ErrInvalidNickname    = errors.New("authorization: nickname cannot be empty")
	ErrInvalidEmail       = errors.New("authorization: invalid email address")
	ErrEmailTaken         = errors.New("authorization: email already exists")
	ErrInvalidTokenAmount = errors.New("authorization: token amount must be positive")
)

var (
	sharedUserCacheMu sync.RWMutex
	sharedUserCache   *userCache
)

// Module wires together the JWT middleware and backing services.
type Module struct {
	db                 *gorm.DB
	userStore          *UserStore
	jwtMiddleware      *jwt.GinJWTMiddleware
	captcha            *CaptchaStore
	avatarStorage      *filestore.AvatarStorage
	authService        *AuthService
	adminRequestMailer *adminRequestMailer
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
	if client, err := cache.GetRedisClient(); err != nil {
		log.Printf("authorization: redis disabled: %v", err)
	} else {
		userCache := newUserCache(client)
		userStore.cache = userCache
		setSharedUserCache(userCache)
	}
	captchaStore := NewCaptchaStore(3 * time.Minute)
	avatarStore, err := filestore.NewAvatarStorageFromEnv()
	if err != nil {
		return nil, err
	}
	authService := &AuthService{users: userStore}

	mailer, err := newAdminRequestMailerFromEnv()
	if err != nil {
		log.Printf("authorization: admin request mailer disabled: %v", err)
	}

	middleware, err := buildJWTMiddleware(authService, avatarStore)
	if err != nil {
		return nil, err
	}

	module := &Module{
		db:                 db,
		userStore:          userStore,
		jwtMiddleware:      middleware,
		captcha:            captchaStore,
		avatarStorage:      avatarStore,
		authService:        authService,
		adminRequestMailer: mailer,
	}

	authGroup := router.Group("/auth")
	authGroup.GET("/captcha", module.handleCaptcha)
	authGroup.POST("/register", module.handleRegister)
	authGroup.POST("/login", module.handleLogin)
	authGroup.POST("/refresh", module.handleRefresh)

	secured := authGroup.Group("")
	secured.Use(module.jwtMiddleware.MiddlewareFunc())
	secured.GET("/profile", module.handleProfile)
	secured.PUT("/profile", module.handleUpdateProfile)
	secured.POST("/profile/avatar", module.handleUploadAvatar)
	secured.POST("/tokens/purchase", module.handlePurchaseTokens)
	secured.POST("/admin-request", module.handleAdminRequest)

	return module, nil
}

// handleCaptcha godoc
// @Summary 获取图形验证码
// @Description 获取登录或注册时需要的人机验证图像
// @Tags Authorization
// @Produce json
// @Success 200 {object} map[string]interface{} "验证码信息"
// @Failure 503 {object} map[string]string "服务不可用"
// @Author bizer
// @Router /auth/captcha [get]
func (m *Module) handleCaptcha(c *gin.Context) {
	if m == nil || m.captcha == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "captcha service unavailable"})
		return
	}

	challenge := m.captcha.Issue()
	expiresIn := int(challenge.TTL.Seconds())
	if expiresIn < 1 {
		expiresIn = int(time.Until(challenge.ExpiresAt).Seconds())
		if expiresIn < 1 {
			expiresIn = 1
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"captcha_id":   challenge.ID,
		"image_base64": challenge.ImageBase64,
		"expires_in":   expiresIn,
		"expires_at":   challenge.ExpiresAt.UTC(),
	})
}

// handleRegister godoc
// @Summary 用户注册
// @Description 创建新用户账号并返回初始信息
// @Tags Authorization
// @Accept json
// @Produce json
// @Param request body RegisterRequest true "注册请求"
// @Success 201 {object} map[string]interface{} "新建用户信息"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 409 {object} map[string]string "资源冲突"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// @Router /auth/register [post]
func (m *Module) handleRegister(c *gin.Context) {
	if m == nil || m.authService == nil || m.userStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "registration service unavailable"})
		return
	}

	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}

	if m.captcha != nil && !m.captcha.Verify(req.CaptchaID, req.CaptchaAnswer) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid captcha"})
		return
	}

	nickname := strings.TrimSpace(req.Nickname)
	email := strings.TrimSpace(req.Email)

	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		if nickname != "" {
			displayName = nickname
		} else {
			displayName = req.Username
		}
	}

	ctx := c.Request.Context()
	user, err := m.authService.Register(ctx, req.Username, req.Password, displayName, nickname, email, req.AvatarURL, req.Bio)
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrMissingLoginValues):
			c.JSON(http.StatusBadRequest, gin.H{"error": "username and password are required"})
		case errors.Is(err, ErrWeakPassword):
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrWeakPassword.Error()})
		case errors.Is(err, ErrInvalidNickname):
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrInvalidNickname.Error()})
		case errors.Is(err, ErrInvalidEmail):
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrInvalidEmail.Error()})
		case errors.Is(err, ErrUsernameTaken):
			c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
		case errors.Is(err, ErrEmailTaken):
			c.JSON(http.StatusConflict, gin.H{"error": "email already exists"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register"})
		}
		return
	}

	roles, err := m.userStore.FindRoleNames(ctx, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user roles"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"user": buildUserPayload(ctx, m.avatarStorage, user, roles)})
}

// handleLogin godoc
// @Summary 用户登录
// @Description 验证凭证并返回访问令牌
// @Tags Authorization
// @Accept json
// @Produce json
// @Param request body LoginRequest true "登录请求"
// @Success 200 {object} map[string]interface{} "登录响应"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 503 {object} map[string]string "服务不可用"
// @Author bizer
// @Router /auth/login [post]
func (m *Module) handleLogin(c *gin.Context) {
	if m == nil || m.jwtMiddleware == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "authentication service unavailable"})
		return
	}

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

	if m.captcha != nil && !m.captcha.Verify(req.CaptchaID, req.CaptchaAnswer) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid captcha"})
		return
	}

	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	m.jwtMiddleware.LoginHandler(c)
}

// handleRefresh godoc
// @Summary 刷新访问令牌
// @Description 使用刷新令牌获取新的访问令牌
// @Tags Authorization
// @Produce json
// @Success 200 {object} map[string]interface{} "刷新结果"
// @Failure 503 {object} map[string]string "服务不可用"
// @Author bizer
// @Router /auth/refresh [post]
func (m *Module) handleRefresh(c *gin.Context) {
	if m == nil || m.jwtMiddleware == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "authentication service unavailable"})
		return
	}

	m.jwtMiddleware.RefreshHandler(c)
}

// handleProfile godoc
// @Summary 获取个人信息
// @Description 返回当前登录用户的详细资料
// @Tags Authorization
// @Produce json
// @Success 200 {object} map[string]interface{} "用户资料"
// @Failure 401 {object} map[string]string "未授权"
// @Failure 404 {object} map[string]string "未找到"
// @Failure 503 {object} map[string]string "服务不可用"
// @Author bizer
// @Router /auth/profile [get]
func (m *Module) handleProfile(c *gin.Context) {
	if m == nil || m.userStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "profile service unavailable"})
		return
	}

	claims := jwt.ExtractClaims(c)
	userID := extractUserID(claims)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}

	ctx := c.Request.Context()
	user, err := m.userStore.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
		}
		return
	}

	roles, err := m.userStore.FindRoleNames(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load roles"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"user": buildUserPayload(ctx, m.avatarStorage, user, roles)})
}

// handleUpdateProfile godoc
// @Summary 更新个人信息
// @Description 修改当前登录用户的基本资料
// @Tags Authorization
// @Accept json
// @Produce json
// @Param request body UpdateProfileRequest true "资料更新请求"
// @Success 200 {object} map[string]interface{} "更新后的资料"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 401 {object} map[string]string "未授权"
// @Failure 404 {object} map[string]string "未找到"
// @Failure 503 {object} map[string]string "服务不可用"
// @Author bizer
// @Router /auth/profile [put]
func (m *Module) handleUpdateProfile(c *gin.Context) {
	if m == nil || m.userStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "profile service unavailable"})
		return
	}

	var req UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}
	if req.DisplayName == nil && req.AvatarURL == nil && req.Bio == nil && req.Nickname == nil && req.Email == nil {
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
	updated, err := m.userStore.UpdateProfile(ctx, userID, UpdateProfileParams{
		DisplayName: req.DisplayName,
		Nickname:    req.Nickname,
		Email:       req.Email,
		AvatarURL:   req.AvatarURL,
		Bio:         req.Bio,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidDisplayName):
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrInvalidDisplayName.Error()})
		case errors.Is(err, ErrInvalidNickname):
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrInvalidNickname.Error()})
		case errors.Is(err, ErrInvalidEmail):
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrInvalidEmail.Error()})
		case errors.Is(err, ErrEmailTaken):
			c.JSON(http.StatusConflict, gin.H{"error": "email already exists"})
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update profile"})
		}
		return
	}

	roles, err := m.userStore.FindRoleNames(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load roles"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"user": buildUserPayload(ctx, m.avatarStorage, updated, roles)})
}

// handleUploadAvatar godoc
// @Summary 上传头像
// @Description 上传并更新当前用户的头像文件
// @Tags Authorization
// @Accept multipart/form-data
// @Produce json
// @Param avatar formData file true "头像文件"
// @Success 200 {object} map[string]interface{} "更新后的资料"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 401 {object} map[string]string "未授权"
// @Failure 503 {object} map[string]string "服务不可用"
// @Author bizer
// @Router /auth/profile/avatar [post]
func (m *Module) handleUploadAvatar(c *gin.Context) {
	if m == nil || m.userStore == nil || m.avatarStorage == nil {
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
	existing, err := m.userStore.FindByID(ctx, userID)
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

	uploaded, err := m.avatarStorage.Upload(ctx, file, "users", fmt.Sprintf("%d", userID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to upload avatar", "details": err.Error()})
		return
	}

	updated, err := m.userStore.UpdateProfile(ctx, userID, UpdateProfileParams{AvatarURL: &uploaded})
	if err != nil {
		_ = m.avatarStorage.Remove(ctx, uploaded)
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update profile"})
		}
		return
	}

	if oldAvatar != "" && oldAvatar != uploaded {
		_ = m.avatarStorage.Remove(ctx, oldAvatar)
	}

	roles, err := m.userStore.FindRoleNames(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load roles"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"user": buildUserPayload(ctx, m.avatarStorage, updated, roles)})
}

// handlePurchaseTokens godoc
// @Summary 购买通用令牌
// @Description 为当前用户增加可用的对话令牌余额
// @Tags Authorization
// @Accept json
// @Produce json
// @Param request body purchaseTokensRequest true "购买请求"
// @Success 200 {object} map[string]interface{} "最新令牌余额"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 401 {object} map[string]string "未授权"
// @Failure 404 {object} map[string]string "未找到"
// @Failure 503 {object} map[string]string "服务不可用"
// @Author bizer
// @Router /auth/tokens/purchase [post]
func (m *Module) handlePurchaseTokens(c *gin.Context) {
	if m == nil || m.userStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "token service unavailable"})
		return
	}

	var req purchaseTokensRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}

	if req.Amount <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": ErrInvalidTokenAmount.Error()})
		return
	}
	if req.Amount > maxPurchaseTokensPerRequest {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("token amount exceeds limit of %d", maxPurchaseTokensPerRequest)})
		return
	}

	claims := jwt.ExtractClaims(c)
	userID := extractUserID(claims)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}

	ctx := c.Request.Context()
	balance, err := m.userStore.AddTokens(ctx, userID, req.Amount)
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		case errors.Is(err, ErrInvalidTokenAmount):
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrInvalidTokenAmount.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update token balance"})
		}
		return
	}

	user, err := m.userStore.FindByID(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
		return
	}
	user.TokenBalance = balance

	roles, err := m.userStore.FindRoleNames(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load roles"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token_balance": balance,
		"user":          buildUserPayload(ctx, m.avatarStorage, user, roles),
	})
}

func setSharedUserCache(c *userCache) {
	sharedUserCacheMu.Lock()
	defer sharedUserCacheMu.Unlock()
	sharedUserCache = c
}

func getSharedUserCache() *userCache {
	sharedUserCacheMu.RLock()
	defer sharedUserCacheMu.RUnlock()
	return sharedUserCache
}

// InvalidateUserCache clears cached user data so fresh values are loaded from the database.
func InvalidateUserCache(ctx context.Context, userID uint) {
	if userID == 0 {
		return
	}
	cache := getSharedUserCache()
	if cache == nil {
		return
	}
	cache.invalidateUser(ctx, userID)
	cache.invalidateRoles(ctx, userID)
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
	Nickname      string  `json:"nickname" binding:"required"`
	Email         string  `json:"email" binding:"required,email"`
	CaptchaID     string  `json:"captcha_id" binding:"required"`
	CaptchaAnswer string  `json:"captcha_answer" binding:"required"`
	AvatarURL     *string `json:"avatar_url"`
	Bio           *string `json:"bio"`
}

// UpdateProfileRequest captures profile update fields.
type UpdateProfileRequest struct {
	DisplayName *string `json:"display_name"`
	Nickname    *string `json:"nickname"`
	Email       *string `json:"email" binding:"omitempty,email"`
	AvatarURL   *string `json:"avatar_url"`
	Bio         *string `json:"bio"`
}

// purchaseTokensRequest represents token top-up payload.
type purchaseTokensRequest struct {
	Amount int64 `json:"amount"`
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
func (s *AuthService) Register(ctx context.Context, username, password, displayName, nickname, email string, avatarURL, bio *string) (*User, error) {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	displayName = strings.TrimSpace(displayName)
	nickname = strings.TrimSpace(nickname)
	email = strings.TrimSpace(email)

	if username == "" || password == "" {
		return nil, jwt.ErrMissingLoginValues
	}
	if len(password) < 6 {
		return nil, ErrWeakPassword
	}
	if nickname == "" {
		return nil, ErrInvalidNickname
	}
	if displayName == "" {
		displayName = nickname
	}

	normalizedEmail, err := normalizeEmail(email)
	if err != nil {
		return nil, err
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
		Nickname:     nickname,
		Email:        normalizedEmail,
		AvatarURL:    storedAvatar,
		Bio:          storedBio,
		TokenBalance: defaultTokenBalance,
	}
	if err := s.users.Create(ctx, user); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			if isDuplicateEmailError(err) {
				return nil, ErrEmailTaken
			}
			return nil, ErrUsernameTaken
		}
		return nil, fmt.Errorf("authorization: create user: %w", err)
	}

	return user, nil
}

// UserStore provides data access helpers backed by GORM.
type UserStore struct {
	db    *gorm.DB
	cache *userCache
}

type userCache struct {
	client *redis.Client
}

func newUserCache(client *redis.Client) *userCache {
	if client == nil {
		return nil
	}
	return &userCache{client: client}
}

func (c *userCache) cacheContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), cacheOperationTimeout)
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= cacheOperationTimeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, cacheOperationTimeout)
}

func (c *userCache) userKeyByID(id uint) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprintf("auth:user:%d", id)
}

func (c *userCache) userKeyByUsername(username string) string {
	normalized := strings.ToLower(strings.TrimSpace(username))
	if normalized == "" {
		return ""
	}
	return fmt.Sprintf("auth:user:username:%s", normalized)
}

func (c *userCache) rolesKey(userID uint) string {
	if userID == 0 {
		return ""
	}
	return fmt.Sprintf("auth:user:%d:roles", userID)
}

func (c *userCache) getUserByID(ctx context.Context, id uint) (*User, error) {
	if c == nil || c.client == nil || id == 0 {
		return nil, redis.Nil
	}
	ctx, cancel := c.cacheContext(ctx)
	defer cancel()
	data, err := c.client.Get(ctx, c.userKeyByID(id)).Bytes()
	if err != nil {
		return nil, err
	}
	var user User
	if err := json.Unmarshal(data, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

func (c *userCache) getUserByUsername(ctx context.Context, username string) (*User, error) {
	if c == nil || c.client == nil {
		return nil, redis.Nil
	}
	key := c.userKeyByUsername(username)
	if key == "" {
		return nil, redis.Nil
	}
	ctx, cancel := c.cacheContext(ctx)
	defer cancel()
	data, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, err
	}
	var user User
	if err := json.Unmarshal(data, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

func (c *userCache) storeUser(ctx context.Context, user *User) {
	if c == nil || c.client == nil || user == nil || user.ID == 0 {
		return
	}
	payload, err := json.Marshal(user)
	if err != nil {
		log.Printf("authorization: marshal user cache payload failed: %v", err)
		return
	}
	ctx, cancel := c.cacheContext(ctx)
	defer cancel()
	pipe := c.client.TxPipeline()
	pipe.Set(ctx, c.userKeyByID(user.ID), payload, userCacheTTL)
	if usernameKey := c.userKeyByUsername(user.Username); usernameKey != "" {
		pipe.Set(ctx, usernameKey, payload, userCacheTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("authorization: store user cache failed: %v", err)
	}
}

func (c *userCache) invalidateUser(ctx context.Context, userID uint, usernames ...string) {
	if c == nil || c.client == nil || userID == 0 {
		return
	}
	keys := []string{}
	if idKey := c.userKeyByID(userID); idKey != "" {
		keys = append(keys, idKey)
	}
	for _, name := range usernames {
		if key := c.userKeyByUsername(name); key != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return
	}
	ctx, cancel := c.cacheContext(ctx)
	defer cancel()
	if _, err := c.client.Del(ctx, keys...).Result(); err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("authorization: invalidate user cache keys %v failed: %v", keys, err)
	}
}

func (c *userCache) getRoles(ctx context.Context, userID uint) ([]string, error) {
	if c == nil || c.client == nil || userID == 0 {
		return nil, redis.Nil
	}
	ctx, cancel := c.cacheContext(ctx)
	defer cancel()
	data, err := c.client.Get(ctx, c.rolesKey(userID)).Bytes()
	if err != nil {
		return nil, err
	}
	var roles []string
	if err := json.Unmarshal(data, &roles); err != nil {
		return nil, err
	}
	return roles, nil
}

func (c *userCache) storeRoles(ctx context.Context, userID uint, roles []string) {
	if c == nil || c.client == nil || userID == 0 {
		return
	}
	payload, err := json.Marshal(roles)
	if err != nil {
		log.Printf("authorization: marshal roles cache payload failed: %v", err)
		return
	}
	ctx, cancel := c.cacheContext(ctx)
	defer cancel()
	if err := c.client.Set(ctx, c.rolesKey(userID), payload, userRolesCacheTTL).Err(); err != nil {
		log.Printf("authorization: store roles cache failed: %v", err)
	}
}

func (c *userCache) invalidateRoles(ctx context.Context, userID uint) {
	if c == nil || c.client == nil || userID == 0 {
		return
	}
	ctx, cancel := c.cacheContext(ctx)
	defer cancel()
	if err := c.client.Del(ctx, c.rolesKey(userID)).Err(); err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("authorization: invalidate roles cache failed: %v", err)
	}
}

// UpdateProfileParams holds the fields eligible for profile updates.
type UpdateProfileParams struct {
	DisplayName *string
	Nickname    *string
	Email       *string
	AvatarURL   *string
	Bio         *string
}

// FindByID loads a user by primary key.
func (s *UserStore) FindByID(ctx context.Context, id uint) (*User, error) {
	if s == nil {
		return nil, errors.New("authorization: user store not initialized")
	}
	if id == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	if s.cache != nil {
		if cached, err := s.cache.getUserByID(ctx, id); err == nil {
			return cached, nil
		} else if err != nil && !errors.Is(err, redis.Nil) {
			log.Printf("authorization: fetch user %d from cache: %v", id, err)
		}
	}
	var user User
	result := s.db.WithContext(ctx).Where("id = ?", id).First(&user)
	if result.Error != nil {
		return nil, result.Error
	}
	if s.cache != nil {
		s.cache.storeUser(ctx, &user)
	}
	return &user, nil
}

// FindByUsername loads a user by unique username.
func (s *UserStore) FindByUsername(ctx context.Context, username string) (*User, error) {
	if s == nil {
		return nil, errors.New("authorization: user store not initialized")
	}
	trimmed := strings.TrimSpace(username)
	if trimmed == "" {
		return nil, gorm.ErrRecordNotFound
	}
	if s.cache != nil {
		if cached, err := s.cache.getUserByUsername(ctx, trimmed); err == nil {
			return cached, nil
		} else if err != nil && !errors.Is(err, redis.Nil) {
			log.Printf("authorization: fetch user %s from cache: %v", trimmed, err)
		}
	}
	var user User
	result := s.db.WithContext(ctx).Where("username = ?", trimmed).First(&user)
	if result.Error != nil {
		return nil, result.Error
	}
	if s.cache != nil {
		s.cache.storeUser(ctx, &user)
	}
	return &user, nil
}

// Create inserts a new user record.
func (s *UserStore) Create(ctx context.Context, user *User) error {
	if s == nil {
		return errors.New("authorization: user store not initialized")
	}
	if user == nil {
		return errors.New("authorization: user payload is nil")
	}
	if err := s.db.WithContext(ctx).Create(user).Error; err != nil {
		return err
	}
	if s.cache != nil {
		s.cache.storeUser(ctx, user)
		s.cache.invalidateRoles(ctx, user.ID)
	}
	return nil
}

// FindRoleNames returns the roles assigned to the given user.
func (s *UserStore) FindRoleNames(ctx context.Context, userID uint) ([]string, error) {
	if s == nil {
		return nil, errors.New("authorization: user store not initialized")
	}
	if userID == 0 {
		return []string{}, nil
	}
	if s.cache != nil {
		if cached, err := s.cache.getRoles(ctx, userID); err == nil {
			return cached, nil
		} else if err != nil && !errors.Is(err, redis.Nil) {
			log.Printf("authorization: fetch roles for user %d from cache: %v", userID, err)
		}
	}
	var roles []string
	err := s.db.WithContext(ctx).
		Model(&Role{}).
		Select("roles.code").
		Joins("JOIN user_roles ON user_roles.role_id = roles.id").
		Where("user_roles.user_id = ?", userID).
		Scan(&roles).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return []string{}, nil
		}
		return nil, err
	}
	normalized := make([]string, 0, len(roles))
	for _, role := range roles {
		trimmed := strings.TrimSpace(role)
		if trimmed == "" {
			continue
		}
		normalized = append(normalized, strings.ToLower(trimmed))
	}
	if s.cache != nil {
		s.cache.storeRoles(ctx, userID, normalized)
	}
	return normalized, nil
}

// TokenBalance returns the current token balance for a user.
func (s *UserStore) TokenBalance(ctx context.Context, userID uint) (int64, error) {
	if s == nil {
		return 0, errors.New("authorization: user store not initialized")
	}

	var result struct {
		TokenBalance int64
	}

	err := s.db.WithContext(ctx).
		Table("users").
		Select("token_balance").
		Where("id = ?", userID).
		Take(&result).
		Error
	if err != nil {
		return 0, err
	}

	return result.TokenBalance, nil
}

// AddTokens increments a user token balance and returns the updated value.
func (s *UserStore) AddTokens(ctx context.Context, userID uint, amount int64) (int64, error) {
	if s == nil {
		return 0, errors.New("authorization: user store not initialized")
	}

	if amount <= 0 {
		return 0, ErrInvalidTokenAmount
	}

	updates := map[string]any{
		"token_balance": gorm.Expr("COALESCE(token_balance, 0) + ?", amount),
		"updated_at":    time.Now().UTC(),
	}

	if err := s.db.WithContext(ctx).Model(&User{}).Where("id = ?", userID).Updates(updates).Error; err != nil {
		return 0, err
	}

	if s.cache != nil {
		s.cache.invalidateUser(ctx, userID)
	}
	return s.TokenBalance(ctx, userID)
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

	if params.Nickname != nil {
		nickname := strings.TrimSpace(*params.Nickname)
		if nickname == "" {
			return nil, ErrInvalidNickname
		}
		updates["nickname"] = nickname
	}

	if params.Email != nil {
		normalizedEmail, err := normalizeEmail(*params.Email)
		if err != nil {
			return nil, err
		}
		updates["email"] = normalizedEmail
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
		if errors.Is(result.Error, gorm.ErrDuplicatedKey) && isDuplicateEmailError(result.Error) {
			return nil, ErrEmailTaken
		}
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}

	if s.cache != nil {
		s.cache.invalidateUser(ctx, userID)
	}

	return s.FindByID(ctx, userID)
}

// User represents an application account.
type User struct {
	ID           uint    `gorm:"primaryKey"`
	Username     string  `gorm:"uniqueIndex;size:64;not null"`
	PasswordHash string  `gorm:"size:255;not null"`
	DisplayName  string  `gorm:"size:128;not null;default:''"`
	Nickname     string  `gorm:"size:64;not null"`
	Email        string  `gorm:"uniqueIndex;size:128;not null"`
	AvatarURL    *string `gorm:"size:255"`
	Bio          *string `gorm:"type:text"`
	Status       string  `gorm:"size:32;default:'active'"`
	LastLoginAt  *time.Time
	CreatedAt    time.Time
	TokenBalance int64 `gorm:"column:token_balance;not null;default:100000"`
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
		"nickname":      user.Nickname,
		"email":         user.Email,
		"avatar_url":    avatarField,
		"bio":           bioField,
		"status":        user.Status,
		"last_login_at": user.LastLoginAt,
		"created_at":    user.CreatedAt,
		"updated_at":    user.UpdatedAt,
		"token_balance": user.TokenBalance,
		"roles":         roles,
	}
}
func normalizeEmail(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrInvalidEmail
	}

	parsed, err := mail.ParseAddress(trimmed)
	if err != nil || parsed.Address == "" {
		return "", ErrInvalidEmail
	}

	return strings.ToLower(parsed.Address), nil
}

func isDuplicateEmailError(err error) bool {
	if err == nil {
		return false
	}

	message := strings.ToLower(err.Error())
	return strings.Contains(message, "idx_users_email") ||
		strings.Contains(message, "users.email") ||
		strings.Contains(message, "users_email")
}
