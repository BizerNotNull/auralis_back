package agents

import (
	"time"

	"gorm.io/datatypes"
)

// Agent 表示可供用户交互的智能体核心信息模型。
type Agent struct {
	ID               uint64         `gorm:"primaryKey" json:"id"`
	Name             string         `gorm:"size:100;not null" json:"name"`
	Gender           string         `gorm:"size:16" json:"gender"`
	TitleAddress     *string        `gorm:"size:50" json:"title_address,omitempty"`
	OneSentenceIntro *string        `gorm:"size:255" json:"one_sentence_intro,omitempty"`
	PersonaDesc      *string        `gorm:"type:text" json:"persona_desc,omitempty"`
	OpeningLine      *string        `gorm:"type:text" json:"opening_line,omitempty"`
	FirstTurnHint    *string        `gorm:"type:text" json:"first_turn_hint,omitempty"`
	Live2DModelID    *string        `gorm:"size:100" json:"live2d_model_id,omitempty"`
	VoiceID          *string        `gorm:"size:100" json:"voice_id,omitempty"`
	VoiceProvider    *string        `gorm:"size:50" json:"voice_provider,omitempty"`
	AvatarURL        *string        `gorm:"size:255" json:"avatar_url,omitempty"`
	Status           string         `gorm:"size:16;not null;default:'pending'" json:"status"`
	LangDefault      string         `gorm:"size:10;not null;default:'zh-CN'" json:"lang_default"`
	Tags             datatypes.JSON `gorm:"type:json" json:"tags,omitempty"`
	Version          int            `gorm:"not null;default:1" json:"version"`
	Notes            *string        `gorm:"type:text" json:"notes,omitempty"`
	AverageRating    float64        `gorm:"-" json:"average_rating"`
	RatingCount      int64          `gorm:"-" json:"rating_count"`
	ViewCount        uint64         `gorm:"not null;default:0" json:"view_count"`
	HotScore         float64        `gorm:"-" json:"-"`
	CreatedBy        uint64         `gorm:"not null;index" json:"created_by"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

// TableName 指定 Agent 模型对应的数据库表名。
func (Agent) TableName() string {
	return "agents"
}

// AgentRating 记录用户针对智能体的评分与可选评价内容。
type AgentRating struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	AgentID   uint64    `gorm:"not null;index:idx_agent_user,unique" json:"agent_id"`
	UserID    uint64    `gorm:"not null;index:idx_agent_user,unique" json:"user_id"`
	Score     int       `gorm:"not null" json:"score"`
	Comment   *string   `gorm:"type:text" json:"comment,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定 AgentRating 模型的存储表。
func (AgentRating) TableName() string {
	return "agent_ratings"
}

// AgentChatConfig 保存智能体使用的大模型及补充配置。
type AgentChatConfig struct {
	AgentID          uint64         `gorm:"primaryKey" json:"agent_id"`
	ModelProvider    string         `gorm:"size:50;not null" json:"model_provider"`
	ModelName        string         `gorm:"size:100;not null" json:"model_name"`
	ModelParams      datatypes.JSON `gorm:"type:json" json:"model_params,omitempty"`
	SystemPrompt     *string        `gorm:"type:mediumtext" json:"system_prompt,omitempty"`
	StyleGuide       datatypes.JSON `gorm:"type:json" json:"style_guide,omitempty"`
	ResponseFormat   string         `gorm:"size:16;not null;default:'text'" json:"response_format"`
	CitationRequired bool           `gorm:"not null;default:false" json:"citation_required"`
	FunctionCalling  bool           `gorm:"not null;default:false" json:"function_calling"`
	RagParams        datatypes.JSON `gorm:"type:json" json:"rag_params,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

// TableName 指定 AgentChatConfig 的数据库表名。
func (AgentChatConfig) TableName() string {
	return "agent_chat_config"
}
