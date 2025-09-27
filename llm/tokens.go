package llm

import (
	authorization "auralis_back/authorization"
	"context"
	"errors"
	"log"
	"time"

	"gorm.io/gorm"
)

var ErrInsufficientTokens = errors.New("llm: insufficient token balance")

func intPointerIfPositive(value int) *int {
	if value <= 0 {
		return nil
	}
	v := value
	return &v
}

func int64Pointer(value int64) *int64 {
	v := value
	return &v
}

func totalTokensUsed(usage *ChatUsage) int64 {
	if usage == nil {
		return 0
	}
	total := int64(usage.PromptTokens) + int64(usage.CompletionTokens)
	if total < 0 {
		return 0
	}
	return total
}

func (m *Module) getUserTokenBalance(ctx context.Context, userID uint64) (int64, error) {
	if m == nil || m.db == nil {
		return 0, errors.New("llm: database not initialized")
	}
	var result struct {
		TokenBalance int64
	}
	query := m.db.WithContext(ctx).
		Table("users").
		Select("token_balance").
		Where("id = ?", userID)
	if err := query.Take(&result).Error; err != nil {
		return 0, err
	}
	return result.TokenBalance, nil
}

func (m *Module) applyUsageToUserTokens(ctx context.Context, userID uint64, usage *ChatUsage, startingBalance int64) (int64, error) {
	if m == nil || m.db == nil {
		return 0, errors.New("llm: database not initialized")
	}
	if usage == nil {
		if startingBalance >= 0 {
			return startingBalance, nil
		}
		return m.getUserTokenBalance(ctx, userID)
	}
	total := totalTokensUsed(usage)
	if total <= 0 {
		if startingBalance >= 0 {
			return startingBalance, nil
		}
		return m.getUserTokenBalance(ctx, userID)
	}
	updates := map[string]any{
		"token_balance": gorm.Expr("CASE WHEN token_balance >= ? THEN token_balance - ? ELSE 0 END", total, total),
		"updated_at":    time.Now().UTC(),
	}
	res := m.db.WithContext(ctx).
		Table("users").
		Where("id = ?", userID).
		Updates(updates)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, gorm.ErrRecordNotFound
	}
	authorization.InvalidateUserCache(ctx, uint(userID))
	return m.getUserTokenBalance(ctx, userID)
}

func (m *Module) incrementConversationTokens(ctx context.Context, convID uint64, usage *ChatUsage) {
	if m == nil || m.db == nil || usage == nil || convID == 0 {
		return
	}

	updates := make(map[string]any, 2)
	if usage.PromptTokens > 0 {
		updates["token_input_sum"] = gorm.Expr("COALESCE(token_input_sum, 0) + ?", usage.PromptTokens)
	}
	if usage.CompletionTokens > 0 {
		updates["token_output_sum"] = gorm.Expr("COALESCE(token_output_sum, 0) + ?", usage.CompletionTokens)
	}
	if len(updates) == 0 {
		return
	}

	if err := m.db.WithContext(ctx).Model(&conversation{}).Where("id = ?", convID).Updates(updates).Error; err != nil {
		log.Printf("llm: failed to update conversation token sums: %v", err)
	}
}
