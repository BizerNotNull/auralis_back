package agents

import (
	"time"

	"gorm.io/datatypes"
)

type Agent struct {
	ID            uint64         `gorm:"primaryKey" json:"id"`
	Name          string         `gorm:"size:100;not null" json:"name"`
	Gender        string         `gorm:"size:16" json:"gender"`
	TitleAddress  *string        `gorm:"size:50" json:"title_address,omitempty"`
	PersonaDesc   *string        `gorm:"type:text" json:"persona_desc,omitempty"`
	OpeningLine   *string        `gorm:"type:text" json:"opening_line,omitempty"`
	FirstTurnHint *string        `gorm:"type:text" json:"first_turn_hint,omitempty"`
	Live2DModelID *string        `gorm:"size:100" json:"live2d_model_id,omitempty"`
	Status        string         `gorm:"size:16;not null;default:'draft'" json:"status"`
	LangDefault   string         `gorm:"size:10;not null;default:'zh-CN'" json:"lang_default"`
	Tags          datatypes.JSON `gorm:"type:json" json:"tags,omitempty"`
	Version       int            `gorm:"not null;default:1" json:"version"`
	Notes         *string        `gorm:"type:text" json:"notes,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

func (Agent) TableName() string {
	return "agents"
}

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

func (AgentChatConfig) TableName() string {
	return "agent_chat_config"
}
