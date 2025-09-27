package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Service struct {
	db              *gorm.DB
	embedder        Embedder
	vectors         *qdrantClient
	chunker         *chunker
	collectionPref  string
	defaultStatus   string
	defaultVectorSz int
}

type DocumentInput struct {
	Title   string   `json:"title"`
	Summary *string  `json:"summary,omitempty"`
	Source  *string  `json:"source,omitempty"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
	Status  string   `json:"status"`
}

type DocumentUpdate struct {
	Title   *string   `json:"title"`
	Summary *string   `json:"summary"`
	Source  *string   `json:"source"`
	Content *string   `json:"content"`
	Tags    *[]string `json:"tags"`
	Status  *string   `json:"status"`
}

type DocumentRecord struct {
	ID         uint64    `json:"id"`
	AgentID    uint64    `json:"agent_id"`
	Title      string    `json:"title"`
	Summary    *string   `json:"summary,omitempty"`
	Source     *string   `json:"source,omitempty"`
	Content    string    `json:"content"`
	Tags       []string  `json:"tags"`
	Status     string    `json:"status"`
	ChunkCount int       `json:"chunk_count"`
	CreatedBy  uint64    `json:"created_by"`
	UpdatedBy  uint64    `json:"updated_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type ContextSnippet struct {
	DocumentID uint64   `json:"document_id"`
	Title      string   `json:"title"`
	Source     *string  `json:"source,omitempty"`
	Text       string   `json:"text"`
	Seq        int      `json:"seq"`
	Score      float64  `json:"score"`
	VectorID   string   `json:"vector_id"`
	Tags       []string `json:"tags"`
}

func NewServiceFromEnv(db *gorm.DB) (*Service, error) {
	if db == nil {
		return nil, errors.New("knowledge: database connection is required")
	}

	embedder, err := NewHTTPEmbedderFromEnv()
	if err != nil {
		return nil, err
	}

	vectors, err := newQdrantClientFromEnv()
	if err != nil {
		return nil, err
	}

	chunkMax := 800
	if raw := strings.TrimSpace(getEnvDefault("KNOWLEDGE_CHUNK_MAX_CHARS", "")); raw != "" {
		if parsed, convErr := strconv.Atoi(raw); convErr == nil && parsed > 200 {
			chunkMax = parsed
		}
	}
	chunkMin := chunkMax / 2
	if raw := strings.TrimSpace(getEnvDefault("KNOWLEDGE_CHUNK_MIN_CHARS", "")); raw != "" {
		if parsed, convErr := strconv.Atoi(raw); convErr == nil && parsed > 50 && parsed < chunkMax {
			chunkMin = parsed
		}
	}
	chunker := newChunker(chunkMax, chunkMin)

	service := &Service{
		db:              db,
		embedder:        embedder,
		vectors:         vectors,
		chunker:         chunker,
		collectionPref:  "agent",
		defaultStatus:   "active",
		defaultVectorSz: vectors.vectorSize,
	}
	return service, nil
}

func getEnvDefault(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value != "" {
		return value
	}
	return fallback
}

func (s *Service) AutoMigrate() error {
	if s.db == nil {
		return errors.New("knowledge: database connection is not configured")
	}
	return s.db.AutoMigrate(&Document{}, &Chunk{})
}

func (s *Service) ListDocuments(ctx context.Context, agentID uint64) ([]DocumentRecord, error) {
	if s.db == nil {
		return nil, errors.New("knowledge: database connection is not configured")
	}
	var docs []Document
	if err := s.db.WithContext(ctx).
		Where("agent_id = ?", agentID).
		Order("updated_at DESC").
		Find(&docs).Error; err != nil {
		return nil, err
	}

	counts := make(map[uint64]int)
	if len(docs) > 0 {
		var rows []struct {
			DocumentID uint64
			Count      int
		}
		if err := s.db.WithContext(ctx).
			Model(&Chunk{}).
			Select("document_id, COUNT(*) as count").
			Where("agent_id = ?", agentID).
			Group("document_id").
			Find(&rows).Error; err == nil {
			for _, row := range rows {
				counts[row.DocumentID] = row.Count
			}
		}
	}

	records := make([]DocumentRecord, 0, len(docs))
	for _, doc := range docs {
		records = append(records, buildDocumentRecord(doc, counts[doc.ID], false))
	}
	return records, nil
}

func (s *Service) GetDocument(ctx context.Context, agentID uint64, docID uint64) (*DocumentRecord, error) {
	if s.db == nil {
		return nil, errors.New("knowledge: database connection is not configured")
	}
	var doc Document
	if err := s.db.WithContext(ctx).
		Where("id = ? AND agent_id = ?", docID, agentID).
		Take(&doc).Error; err != nil {
		return nil, err
	}
	var count int64
	_ = s.db.WithContext(ctx).
		Model(&Chunk{}).
		Where("document_id = ?", doc.ID).
		Count(&count)
	record := buildDocumentRecord(doc, int(count), true)
	return &record, nil
}

func (s *Service) CreateDocument(ctx context.Context, agentID uint64, userID uint64, input DocumentInput) (*DocumentRecord, error) {
	if s.db == nil {
		return nil, errors.New("knowledge: database connection is not configured")
	}
	sanitized := sanitizeDocumentInput(input)
	if sanitized.Title == "" {
		return nil, errors.New("knowledge: title is required")
	}
	if sanitized.Content == "" {
		return nil, errors.New("knowledge: content is required")
	}

	segments := s.chunker.split(sanitized.Content)
	if len(segments) == 0 {
		return nil, errors.New("knowledge: content is too short to chunk")
	}

	texts := make([]string, 0, len(segments))
	for _, segment := range segments {
		texts = append(texts, segment.Text)
	}

	embeddings, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(embeddings) != len(segments) {
		return nil, fmt.Errorf("knowledge: embedding count mismatch (expected %d, got %d)", len(segments), len(embeddings))
	}

	vectorIDs := make([]string, len(segments))
	chunks := make([]Chunk, len(segments))
	for i, segment := range segments {
		vectorIDs[i] = uuid.NewString()
		chunks[i] = Chunk{
			AgentID:    agentID,
			Seq:        i + 1,
			Text:       segment.Text,
			VectorID:   vectorIDs[i],
			TokenCount: segment.TokenCount,
		}
	}

	collection := s.collectionName(agentID)
	if err := s.vectors.EnsureCollection(ctx, collection, s.vectorSize()); err != nil {
		return nil, err
	}

	var created Document
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		doc := Document{
			AgentID:   agentID,
			Title:     sanitized.Title,
			Summary:   sanitized.Summary,
			Source:    sanitized.Source,
			Content:   sanitized.Content,
			Tags:      tagsToJSON(sanitized.Tags),
			Status:    sanitized.Status,
			CreatedBy: userID,
			UpdatedBy: userID,
		}
		if err := tx.Create(&doc).Error; err != nil {
			return err
		}

		points := make([]QdrantPoint, len(chunks))
		tagList := sanitized.Tags
		for i := range chunks {
			chunks[i].DocumentID = doc.ID
			payload := map[string]interface{}{
				"agent_id":    agentID,
				"document_id": doc.ID,
				"title":       doc.Title,
				"status":      doc.Status,
				"seq":         chunks[i].Seq,
				"text":        chunks[i].Text,
			}
			if doc.Source != nil {
				payload["source"] = *doc.Source
			}
			if doc.Summary != nil {
				payload["summary"] = *doc.Summary
			}
			if len(tagList) > 0 {
				payload["tags"] = tagList
			}
			points[i] = QdrantPoint{
				ID:      chunks[i].VectorID,
				Vector:  embeddings[i],
				Payload: payload,
			}
		}

		if err := s.vectors.UpsertPoints(ctx, collection, points); err != nil {
			return err
		}

		if err := tx.Create(&chunks).Error; err != nil {
			cleanupErr := s.vectors.DeletePoints(ctx, collection, vectorIDs)
			if cleanupErr != nil {
				log.Printf("knowledge: cleanup qdrant points failed: %v", cleanupErr)
			}
			return err
		}

		created = doc
		return nil
	}); err != nil {
		return nil, err
	}

	record := buildDocumentRecord(created, len(chunks), true)
	record.Content = sanitized.Content
	record.Tags = sanitized.Tags
	return &record, nil
}

func (s *Service) UpdateDocument(ctx context.Context, agentID uint64, docID uint64, userID uint64, changes DocumentUpdate) (*DocumentRecord, error) {
	if s.db == nil {
		return nil, errors.New("knowledge: database connection is not configured")
	}

	var existing Document
	if err := s.db.WithContext(ctx).
		Where("id = ? AND agent_id = ?", docID, agentID).
		Take(&existing).Error; err != nil {
		return nil, err
	}

	updatedDoc := existing
	if changes.Title != nil {
		updatedDoc.Title = strings.TrimSpace(*changes.Title)
	}
	if changes.Summary != nil {
		summary := strings.TrimSpace(*changes.Summary)
		if summary == "" {
			updatedDoc.Summary = nil
		} else {
			updatedDoc.Summary = &summary
		}
	}
	if changes.Source != nil {
		source := strings.TrimSpace(*changes.Source)
		if source == "" {
			updatedDoc.Source = nil
		} else {
			updatedDoc.Source = &source
		}
	}
	if changes.Status != nil {
		updatedDoc.Status = sanitizeStatus(*changes.Status, s.defaultStatus)
	}

	tagsChanged := false
	var normalizedTags []string
	if changes.Tags != nil {
		normalizedTags = normalizeTags(*changes.Tags)
		updatedDoc.Tags = tagsToJSON(normalizedTags)
		tagsChanged = true
	} else {
		normalizedTags = parseTags(updatedDoc.Tags)
	}

	contentChanged := false
	if changes.Content != nil {
		trimmed := strings.TrimSpace(*changes.Content)
		if trimmed == "" {
			return nil, errors.New("knowledge: content cannot be empty")
		}
		contentChanged = trimmed != existing.Content
		updatedDoc.Content = trimmed
	} else {
		updatedDoc.Content = existing.Content
	}

	needsReindex := contentChanged || tagsChanged || changes.Status != nil || changes.Title != nil || changes.Summary != nil || changes.Source != nil

	var chunks []Chunk
	var embeddings [][]float32
	var vectorIDs []string
	if needsReindex {
		segments := s.chunker.split(updatedDoc.Content)
		if len(segments) == 0 {
			return nil, errors.New("knowledge: content is too short to chunk")
		}
		texts := make([]string, len(segments))
		for i, segment := range segments {
			texts[i] = segment.Text
		}
		var err error
		embeddings, err = s.embedder.Embed(ctx, texts)
		if err != nil {
			return nil, err
		}
		if len(embeddings) != len(segments) {
			return nil, fmt.Errorf("knowledge: embedding count mismatch (expected %d, got %d)", len(segments), len(embeddings))
		}
		chunks = make([]Chunk, len(segments))
		vectorIDs = make([]string, len(segments))
		for i, segment := range segments {
			id := uuid.NewString()
			vectorIDs[i] = id
			chunks[i] = Chunk{
				AgentID:    agentID,
				DocumentID: docID,
				Seq:        i + 1,
				Text:       segment.Text,
				VectorID:   id,
				TokenCount: segment.TokenCount,
			}
		}
	}

	collection := s.collectionName(agentID)
	if needsReindex {
		if err := s.vectors.EnsureCollection(ctx, collection, s.vectorSize()); err != nil {
			return nil, err
		}
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updates := map[string]interface{}{
			"title":      updatedDoc.Title,
			"status":     updatedDoc.Status,
			"updated_by": userID,
			"updated_at": time.Now().UTC(),
		}
		if updatedDoc.Summary == nil {
			updates["summary"] = gorm.Expr("NULL")
		} else {
			updates["summary"] = *updatedDoc.Summary
		}
		if updatedDoc.Source == nil {
			updates["source"] = gorm.Expr("NULL")
		} else {
			updates["source"] = *updatedDoc.Source
		}
		if tagsChanged {
			updates["tags"] = updatedDoc.Tags
		}
		if needsReindex {
			updates["content"] = updatedDoc.Content
		}

		if err := tx.Model(&Document{}).
			Where("id = ? AND agent_id = ?", docID, agentID).
			Updates(updates).Error; err != nil {
			return err
		}

		if needsReindex {
			var existingVectors []string
			if err := tx.Model(&Chunk{}).
				Where("document_id = ?", docID).
				Pluck("vector_id", &existingVectors).Error; err != nil {
				return err
			}
			if len(existingVectors) > 0 {
				if err := s.vectors.DeletePoints(ctx, collection, existingVectors); err != nil {
					return err
				}
			}
			if err := tx.Where("document_id = ?", docID).Delete(&Chunk{}).Error; err != nil {
				return err
			}

			tagList := normalizedTags
			if len(tagList) == 0 {
				tagList = parseTags(updatedDoc.Tags)
			}

			points := make([]QdrantPoint, len(chunks))
			for i := range chunks {
				payload := map[string]interface{}{
					"agent_id":    agentID,
					"document_id": docID,
					"title":       updatedDoc.Title,
					"status":      updatedDoc.Status,
					"seq":         chunks[i].Seq,
					"text":        chunks[i].Text,
				}
				if updatedDoc.Source != nil {
					payload["source"] = *updatedDoc.Source
				}
				if updatedDoc.Summary != nil {
					payload["summary"] = *updatedDoc.Summary
				}
				if len(tagList) > 0 {
					payload["tags"] = tagList
				}
				points[i] = QdrantPoint{
					ID:      chunks[i].VectorID,
					Vector:  embeddings[i],
					Payload: payload,
				}
			}

			if err := s.vectors.UpsertPoints(ctx, collection, points); err != nil {
				return err
			}
			if err := tx.Create(&chunks).Error; err != nil {
				cleanupErr := s.vectors.DeletePoints(ctx, collection, vectorIDs)
				if cleanupErr != nil {
					log.Printf("knowledge: cleanup qdrant points failed: %v", cleanupErr)
				}
				return err
			}
		}

		existing = updatedDoc
		return nil
	})
	if err != nil {
		return nil, err
	}

	chunkCount := 0
	if needsReindex {
		chunkCount = len(chunks)
	} else {
		var count int64
		_ = s.db.WithContext(ctx).
			Model(&Chunk{}).
			Where("document_id = ?", existing.ID).
			Count(&count)
		chunkCount = int(count)
	}

	record := buildDocumentRecord(existing, chunkCount, true)
	record.Content = updatedDoc.Content
	record.Tags = parseTags(existing.Tags)
	return &record, nil
}

func (s *Service) DeleteDocument(ctx context.Context, agentID uint64, docID uint64) error {
	if s.db == nil {
		return errors.New("knowledge: database connection is not configured")
	}

	collection := s.collectionName(agentID)

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var doc Document
		if err := tx.Where("id = ? AND agent_id = ?", docID, agentID).Take(&doc).Error; err != nil {
			return err
		}

		var vectorIDs []string
		if err := tx.Model(&Chunk{}).
			Where("document_id = ?", docID).
			Pluck("vector_id", &vectorIDs).Error; err != nil {
			return err
		}
		if len(vectorIDs) > 0 {
			if err := s.vectors.DeletePoints(ctx, collection, vectorIDs); err != nil {
				return err
			}
		}

		if err := tx.Where("document_id = ?", docID).Delete(&Chunk{}).Error; err != nil {
			return err
		}

		return tx.Delete(&Document{}, docID).Error
	})
}

func (s *Service) QueryTopChunks(ctx context.Context, agentID uint64, query string, limit int) ([]ContextSnippet, error) {
	if s.embedder == nil || s.vectors == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, nil
	}

	embeddings, err := s.embedder.Embed(ctx, []string{trimmed})
	if err != nil {
		return nil, err
	}
	if len(embeddings) == 0 {
		return nil, nil
	}

	filter := map[string]interface{}{
		"must": []map[string]interface{}{
			{
				"key":   "agent_id",
				"match": map[string]interface{}{"value": agentID},
			},
			{
				"key":   "status",
				"match": map[string]interface{}{"value": "active"},
			},
		},
	}

	collection := s.collectionName(agentID)
	results, err := s.vectors.Search(ctx, collection, embeddings[0], limit, filter)
	if err != nil {
		return nil, err
	}

	snippets := make([]ContextSnippet, 0, len(results))
	for _, item := range results {
		payload := item.Payload
		snippet := ContextSnippet{
			VectorID: item.ID,
			Score:    item.Score,
		}
		if payload != nil {
			if v, ok := payload["document_id"].(float64); ok && v > 0 {
				snippet.DocumentID = uint64(v)
			}
			if v, ok := payload["title"].(string); ok {
				snippet.Title = v
			}
			if v, ok := payload["source"].(string); ok && v != "" {
				snippet.Source = &v
			}
			if v, ok := payload["text"].(string); ok {
				snippet.Text = v
			}
			if seq, ok := payload["seq"].(float64); ok {
				snippet.Seq = int(seq)
			}
			if rawTags, ok := payload["tags"].([]interface{}); ok {
				snippet.Tags = toStringSlice(rawTags)
			}
		}
		if snippet.DocumentID == 0 {
			continue
		}
		snippets = append(snippets, snippet)
	}

	sort.Slice(snippets, func(i, j int) bool {
		return snippets[i].Score > snippets[j].Score
	})

	return snippets, nil
}

func (s *Service) vectorSize() int {
	if s.defaultVectorSz > 0 {
		return s.defaultVectorSz
	}
	return 0
}

func (s *Service) collectionName(agentID uint64) string {
	if agentID == 0 {
		return "agent_knowledge"
	}
	return fmt.Sprintf("agent_%d_knowledge", agentID)
}

func sanitizeDocumentInput(input DocumentInput) DocumentInput {
	sanitized := DocumentInput{
		Title:   strings.TrimSpace(input.Title),
		Content: strings.TrimSpace(input.Content),
		Status:  sanitizeStatus(input.Status, "active"),
	}
	if input.Summary != nil {
		trimmed := strings.TrimSpace(*input.Summary)
		if trimmed != "" {
			sanitized.Summary = &trimmed
		}
	}
	if input.Source != nil {
		trimmed := strings.TrimSpace(*input.Source)
		if trimmed != "" {
			sanitized.Source = &trimmed
		}
	}
	sanitized.Tags = normalizeTags(input.Tags)
	return sanitized
}

func sanitizeStatus(value string, fallback string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "active", "draft", "inactive":
		return normalized
	case "archived":
		return "inactive"
	default:
		if fallback != "" {
			return fallback
		}
		return "active"
	}
}

func tagsToJSON(tags []string) datatypes.JSON {
	normalized := normalizeTags(tags)
	if len(normalized) == 0 {
		return datatypes.JSON([]byte("[]"))
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return datatypes.JSON([]byte("[]"))
	}
	return datatypes.JSON(raw)
}

func normalizeTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if _, exists := seen[lower]; exists {
			continue
		}
		seen[lower] = struct{}{}
		result = append(result, trimmed)
	}
	sort.Strings(result)
	return result
}

func parseTags(raw datatypes.JSON) []string {
	if len(raw) == 0 {
		return nil
	}
	var tags []string
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil
	}
	return normalizeTags(tags)
}

func buildDocumentRecord(doc Document, chunkCount int, includeContent bool) DocumentRecord {
	record := DocumentRecord{
		ID:         doc.ID,
		AgentID:    doc.AgentID,
		Title:      doc.Title,
		Summary:    doc.Summary,
		Source:     doc.Source,
		Status:     doc.Status,
		ChunkCount: chunkCount,
		CreatedBy:  doc.CreatedBy,
		UpdatedBy:  doc.UpdatedBy,
		CreatedAt:  doc.CreatedAt,
		UpdatedAt:  doc.UpdatedAt,
		Tags:       parseTags(doc.Tags),
	}
	if includeContent {
		record.Content = doc.Content
	}
	return record
}

func toStringSlice(values []interface{}) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if str, ok := value.(string); ok {
			trimmed := strings.TrimSpace(str)
			if trimmed != "" {
				result = append(result, trimmed)
			}
		}
	}
	return result
}
