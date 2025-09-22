package llm

import "time"

type conversation struct {
	ID        uint64    `gorm:"primaryKey"`
	AgentID   uint64    `gorm:"column:agent_id"`
	UserID    uint64    `gorm:"column:user_id"`
	Status 	  string    `gorm:"type:ENUM('active','archived','ended');not null;default:'active';index:idx_conversations_user_agent,priority:3"`
	LastMsgAt time.Time `gorm:"column:last_msg_at"`
	StartedAt time.Time `gorm:"column:started_at"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (conversation) TableName() string {
	return "conversations"
}

type message struct {
	ID              uint64    `gorm:"primaryKey"`
	ConversationID  uint64    `gorm:"column:conversation_id"`
	Seq             int       `gorm:"column:seq"`
	Role            string    `gorm:"column:role"`
	Format          string    `gorm:"column:format"`
	Content         string    `gorm:"column:content"`
	ParentMessageID *uint64   `gorm:"column:parent_msg_id"`
	LatencyMs       *int      `gorm:"column:latency_ms"`
	TokenInput      *int      `gorm:"column:token_input"`
	TokenOutput     *int      `gorm:"column:token_output"`
	ErrCode         *string   `gorm:"column:err_code"`
	ErrMsg          *string   `gorm:"column:err_msg"`
	CreatedAt       time.Time `gorm:"column:created_at"`
}

func (message) TableName() string {
	return "messages"
}
