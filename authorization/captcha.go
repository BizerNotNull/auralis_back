package authorization

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// captchaEntry tracks a pending captcha challenge.
type captchaEntry struct {
	answer    string
	expiresAt time.Time
}

// CaptchaChallenge represents an issued captcha.
type CaptchaChallenge struct {
	ID        string
	Question  string
	ExpiresAt time.Time
	TTL       time.Duration
}

// CaptchaStore keeps captcha challenges in memory.
type CaptchaStore struct {
	mu      sync.Mutex
	entries map[string]captchaEntry
	ttl     time.Duration
	rnd     *rand.Rand
}

// NewCaptchaStore creates an in-memory captcha store with the provided ttl window.
func NewCaptchaStore(ttl time.Duration) *CaptchaStore {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &CaptchaStore{
		entries: make(map[string]captchaEntry),
		ttl:     ttl,
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Issue generates a new captcha challenge.
func (s *CaptchaStore) Issue() CaptchaChallenge {
	if s == nil {
		return CaptchaChallenge{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()

	a := s.rnd.Intn(9) + 1
	b := s.rnd.Intn(9) + 1
	question := fmt.Sprintf("%d + %d = ?", a, b)
	answer := fmt.Sprintf("%d", a+b)

	id := uuid.NewString()
	expiresAt := time.Now().Add(s.ttl)
	s.entries[id] = captchaEntry{answer: answer, expiresAt: expiresAt}

	return CaptchaChallenge{ID: id, Question: question, ExpiresAt: expiresAt, TTL: s.ttl}
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

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()

	entry, ok := s.entries[trimmedID]
	if !ok {
		return false
	}
	delete(s.entries, trimmedID)

	if time.Now().After(entry.expiresAt) {
		return false
	}

	return strings.EqualFold(entry.answer, trimmedAnswer)
}

func (s *CaptchaStore) cleanupLocked() {
	if s == nil {
		return
	}
	now := time.Now()
	for id, entry := range s.entries {
		if now.After(entry.expiresAt) {
			delete(s.entries, id)
		}
	}
}
