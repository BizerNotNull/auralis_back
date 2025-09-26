package llm

import (
	"context"
	"log"

	"gorm.io/gorm"
)

func intPointerIfPositive(value int) *int {
	if value <= 0 {
		return nil
	}
	v := value
	return &v
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
