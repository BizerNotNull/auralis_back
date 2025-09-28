package llm

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// ChatModelOption 描述可选的聊天模型及能力标签。
type ChatModelOption struct {
	Provider     string   `json:"provider"`
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name"`
	Description  string   `json:"description,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Recommended  bool     `json:"recommended,omitempty"`
}

var defaultChatModelCatalog = []ChatModelOption{
	{
		Provider:     "qiniu",
		Name:         "gpt-oss-120b",
		DisplayName:  "GPT-OSS 120B",
		Description:  "默认通用模型，兼容 OpenAI Chat Completions 协议。",
		Capabilities: []string{"chat", "stream"},
		Recommended:  true,
	},
	{
		Provider:     "qiniu",
		Name:         "deepseek/deepseek-v3.1-terminus",
		DisplayName:  "DeepSeek Terminus v3.1",
		Description:  "注重复杂推理的旗舰模型，适合深入分析任务。",
		Capabilities: []string{"chat", "reasoning"},
	},
	{
		Provider:     "qiniu",
		Name:         "x-ai/grok-4-fast",
		DisplayName:  "Grok-4 Fast",
		Description:  "实时搜索增强，响应速度快，适合需要快速反馈的场景。",
		Capabilities: []string{"chat", "search"},
	},
	{
		Provider:     "qiniu",
		Name:         "qwen3-max",
		DisplayName:  "Qwen 3 Max",
		Description:  "多语言表现优秀的大模型，擅长长文本理解与创作。",
		Capabilities: []string{"chat", "multilingual"},
	},
	{
		Provider:     "qiniu",
		Name:         "MiniMax-M1",
		DisplayName:  "MiniMax M1",
		Description:  "均衡型模型，适合通用助理和内容创作。",
		Capabilities: []string{"chat"},
	},
	{
		Provider:     "qiniu",
		Name:         "doubao-seed-1.6",
		DisplayName:  "Doubao Seed 1.6",
		Description:  "语义理解稳定，可作入门业务接入模型。",
		Capabilities: []string{"chat"},
	},
}

// loadChatModelCatalog 加载模型目录（支持环境变量覆盖）。
func loadChatModelCatalog() []ChatModelOption {
	if catalog := loadChatModelCatalogFromEnv(); len(catalog) > 0 {
		return catalog
	}
	return append([]ChatModelOption(nil), defaultChatModelCatalog...)
}

// loadChatModelCatalogFromEnv 从环境变量或文件读取模型目录。
func loadChatModelCatalogFromEnv() []ChatModelOption {
	rawInline := strings.TrimSpace(os.Getenv("LLM_MODEL_CATALOG"))
	if rawInline != "" {
		if catalog := parseModelCatalogJSON(rawInline); len(catalog) > 0 {
			return catalog
		}
		log.Printf("llm: failed to parse LLM_MODEL_CATALOG override")
	}

	rawPath := strings.TrimSpace(os.Getenv("LLM_MODEL_CATALOG_FILE"))
	if rawPath != "" {
		data, err := os.ReadFile(filepath.Clean(rawPath))
		if err != nil {
			log.Printf("llm: read LLM_MODEL_CATALOG_FILE failed: %v", err)
		} else if catalog := parseModelCatalogJSON(string(data)); len(catalog) > 0 {
			return catalog
		} else {
			log.Printf("llm: failed to parse catalog file %s", rawPath)
		}
	}

	return nil
}

func parseModelCatalogJSON(raw string) []ChatModelOption {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}

	var wrapped struct {
		Models []ChatModelOption `json:"models"`
	}
	if err := json.Unmarshal([]byte(trimmed), &wrapped); err == nil && len(wrapped.Models) > 0 {
		return normalizeModelCatalog(wrapped.Models)
	}

	var list []ChatModelOption
	if err := json.Unmarshal([]byte(trimmed), &list); err == nil && len(list) > 0 {
		return normalizeModelCatalog(list)
	}

	return nil
}

func normalizeModelCatalog(list []ChatModelOption) []ChatModelOption {
	if len(list) == 0 {
		return nil
	}

	result := make([]ChatModelOption, 0, len(list))
	seen := make(map[string]struct{}, len(list))

	for _, item := range list {
		provider := strings.TrimSpace(item.Provider)
		name := strings.TrimSpace(item.Name)
		if provider == "" || name == "" {
			continue
		}

		key := strings.ToLower(provider) + "|" + strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		option := ChatModelOption{
			Provider:     provider,
			Name:         name,
			DisplayName:  strings.TrimSpace(item.DisplayName),
			Description:  strings.TrimSpace(item.Description),
			Capabilities: normalizeStringSlice(item.Capabilities),
			Tags:         normalizeStringSlice(item.Tags),
			Recommended:  item.Recommended,
		}
		if option.DisplayName == "" {
			option.DisplayName = name
		}

		result = append(result, option)
	}

	return result
}

func normalizeStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		lowered := strings.ToLower(trimmed)
		if _, exists := seen[lowered]; exists {
			continue
		}
		seen[lowered] = struct{}{}
		result = append(result, trimmed)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
