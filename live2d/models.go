package live2d

import "time"

// Live2DModel represents a registered Live2D asset that can be rendered on the client.
type Live2DModel struct {
	ID          uint64    `gorm:"primaryKey" json:"id"`
	Key         string    `gorm:"size:64;uniqueIndex" json:"key"`
	Name        string    `gorm:"size:100;not null" json:"name"`
	Description *string   `gorm:"type:text" json:"description,omitempty"`
	StorageType string    `gorm:"size:16;not null;default:'local'" json:"storage_type"`
	StoragePath string    `gorm:"size:255" json:"storage_path"`
	EntryFile   string    `gorm:"size:255;not null" json:"entry_file"`
	PreviewFile *string   `gorm:"size:255" json:"preview_file,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (Live2DModel) TableName() string {
	return "live2d_models"
}
