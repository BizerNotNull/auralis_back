package knowledge

import (
	"time"

	"gorm.io/datatypes"
)

type Document struct {
	ID        uint64         `gorm:"primaryKey" json:"id"`
	AgentID   uint64         `gorm:"not null;index:idx_agent_document" json:"agent_id"`
	Title     string         `gorm:"size:200;not null" json:"title"`
	Summary   *string        `gorm:"size:500" json:"summary,omitempty"`
	Source    *string        `gorm:"size:255" json:"source,omitempty"`
	Content   string         `gorm:"type:mediumtext;not null" json:"content"`
	Tags      datatypes.JSON `gorm:"type:json" json:"tags,omitempty"`
	Status    string         `gorm:"size:16;not null;default:'active'" json:"status"`
	CreatedBy uint64         `gorm:"not null;index" json:"created_by"`
	UpdatedBy uint64         `gorm:"not null" json:"updated_by"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func (Document) TableName() string {
	return "agent_knowledge_documents"
}

type Chunk struct {
	ID         uint64    `gorm:"primaryKey" json:"id"`
	DocumentID uint64    `gorm:"not null;index:idx_document_seq" json:"document_id"`
	AgentID    uint64    `gorm:"not null;index" json:"agent_id"`
	Seq        int       `gorm:"not null;index:idx_document_seq" json:"seq"`
	Text       string    `gorm:"type:text;not null" json:"text"`
	VectorID   string    `gorm:"size:128;not null;uniqueIndex" json:"vector_id"`
	TokenCount int       `gorm:"not null;default:0" json:"token_count"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (Chunk) TableName() string {
	return "agent_knowledge_chunks"
}

type ChunkWithScore struct {
	Chunk
	Score float64 `json:"score"`
}
