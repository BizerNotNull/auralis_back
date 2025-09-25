package authorization

import (
	"strings"
	"sync"
	"time"

	"github.com/mojocn/base64Captcha"
)

// CaptchaChallenge represents an issued captcha image.
type CaptchaChallenge struct {
	ID          string
	ImageBase64 string
	ExpiresAt   time.Time
	TTL         time.Duration
}

// CaptchaStore manages captcha generation and verification.
type CaptchaStore struct {
	mu     sync.Mutex
	driver *base64Captcha.DriverDigit
	store  base64Captcha.Store
	ttl    time.Duration
}

// NewCaptchaStore creates an image-based captcha store with the provided ttl window.
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

// Issue generates a new captcha challenge.
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

// Verify checks whether the supplied captcha answer is valid.
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
