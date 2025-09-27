package llm

import (
	"time"

	"gorm.io/datatypes"
)

type conversation struct {
	ID               uint64     `gorm:"primaryKey"`
	AgentID          uint64     `gorm:"column:agent_id;index:idx_conversations_user_agent,priority:2"`
	UserID           uint64     `gorm:"column:user_id;index:idx_conversations_user_agent,priority:1"`
	Title            *string    `gorm:"column:title"`
	Summary          *string    `gorm:"column:summary"`
	Lang             *string    `gorm:"column:lang"`
	Channel          string     `gorm:"column:channel;size:16;not null;default:'web'"`
	Status           string     `gorm:"type:ENUM('active','archived','ended');not null;default:'active';index:idx_conversations_user_agent,priority:3"`
	RetentionDays    *int       `gorm:"column:retention_days"`
	TokenInputSum    int        `gorm:"column:token_input_sum;default:0"`
	TokenOutputSum   int        `gorm:"column:token_output_sum;default:0"`
	SummaryUpdatedAt *time.Time `gorm:"column:summary_updated_at"`
	StartedAt        time.Time  `gorm:"column:started_at"`
	LastMsgAt        time.Time  `gorm:"column:last_msg_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

func (conversation) TableName() string {
	return "conversations"
}

type message struct {
	ID              uint64         `gorm:"primaryKey"`
	ConversationID  uint64         `gorm:"column:conversation_id"`
	Seq             int            `gorm:"column:seq"`
	Role            string         `gorm:"column:role"`
	Format          string         `gorm:"column:format"`
	Content         string         `gorm:"column:content"`
	ParentMessageID *uint64        `gorm:"column:parent_msg_id"`
	LatencyMs       *int           `gorm:"column:latency_ms"`
	TokenInput      *int           `gorm:"column:token_input"`
	TokenOutput     *int           `gorm:"column:token_output"`
	ErrCode         *string        `gorm:"column:err_code"`
	ErrMsg          *string        `gorm:"column:err_msg"`
	Extras          datatypes.JSON `gorm:"column:extras"`
	CreatedAt       time.Time      `gorm:"column:created_at"`
}

func (message) TableName() string {
	return "messages"
}

type userAgentMemory struct {
	ID             uint64         `gorm:"primaryKey"`
	AgentID        uint64         `gorm:"column:agent_id;not null;uniqueIndex:idx_user_agent_memory,priority:2"`
	UserID         uint64         `gorm:"column:user_id;not null;uniqueIndex:idx_user_agent_memory,priority:1"`
	Preferences    datatypes.JSON `gorm:"column:preferences"`
	ProfileSummary *string        `gorm:"column:profile_summary"`
	LastTask       *string        `gorm:"column:last_task"`
	CreatedAt      time.Time      `gorm:"column:created_at"`
	UpdatedAt      time.Time      `gorm:"column:updated_at"`
}

func (userAgentMemory) TableName() string {
	return "user_agent_memory"
}
