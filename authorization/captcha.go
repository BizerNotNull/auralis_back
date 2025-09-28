package authorization

import (
	"strings"
	"sync"
	"time"

	"github.com/mojocn/base64Captcha"
)

// CaptchaChallenge 表示生成的验证码图片信息。
type CaptchaChallenge struct {
	ID          string
	ImageBase64 string
	ExpiresAt   time.Time
	TTL         time.Duration
}

// CaptchaStore 负责验证码的生成与校验。
type CaptchaStore struct {
	mu     sync.Mutex
	driver *base64Captcha.DriverDigit
	store  base64Captcha.Store
	ttl    time.Duration
}

// NewCaptchaStore 创建带过期时间的图片验证码存储。
func NewCaptchaStore(ttl time.Duration) *CaptchaStore {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &CaptchaStore{
		driver: base64Captcha.NewDriverDigit(60, 160, 5, 0.7, 80),
		store:  base64Captcha.NewMemoryStore(2048, ttl),
		ttl:    ttl,
	}
}

// Issue 生成新的验证码挑战并返回展示数据。
func (s *CaptchaStore) Issue() CaptchaChallenge {
	if s == nil {
		return CaptchaChallenge{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	captcha := base64Captcha.NewCaptcha(s.driver, s.store)
	id, image, _, err := captcha.Generate()
	if err != nil {
		return CaptchaChallenge{}
	}

	imageData := strings.TrimSpace(image)
	if imageData != "" && !strings.HasPrefix(imageData, "data:") {
		imageData = "data:image/png;base64," + imageData
	}

	expiresAt := time.Now().Add(s.ttl)
	return CaptchaChallenge{ID: id, ImageBase64: imageData, ExpiresAt: expiresAt, TTL: s.ttl}
}

// Verify 校验用户提交的验证码是否有效。
func (s *CaptchaStore) Verify(id, answer string) bool {
	if s == nil {
		return true
	}

	trimmedID := strings.TrimSpace(id)
	trimmedAnswer := strings.TrimSpace(answer)
	if trimmedID == "" || trimmedAnswer == "" {
		return false
	}

	captcha := base64Captcha.NewCaptcha(s.driver, s.store)
	return captcha.Verify(trimmedID, trimmedAnswer, true)
}
