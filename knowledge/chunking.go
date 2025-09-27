package knowledge

import "strings"

type chunkInput struct {
	Text       string
	TokenCount int
}

type chunker struct {
	maxChars int
	minChars int
}

func newChunker(maxChars int, minChars int) *chunker {
	if maxChars <= 0 {
		maxChars = 800
	}
	if minChars <= 0 || minChars >= maxChars {
		minChars = maxChars / 2
		if minChars < 200 {
			minChars = 200
		}
	}
	return &chunker{maxChars: maxChars, minChars: minChars}
}

func (c *chunker) split(text string) []chunkInput {
	cleaned := strings.TrimSpace(normalizeNewlines(text))
	if cleaned == "" {
		return nil
	}

	runes := []rune(cleaned)
	total := len(runes)
	if total == 0 {
		return nil
	}

	maxChars := c.maxChars
	minChars := c.minChars
	if maxChars <= 0 {
		maxChars = 800
	}
	if minChars <= 0 || minChars >= maxChars {
		minChars = maxChars / 2
		if minChars < 200 {
			minChars = 200
		}
	}

	segments := make([]chunkInput, 0, (total/maxChars)+1)
	start := 0
	for start < total {
		end := start + maxChars
		if end >= total {
			end = total
		} else {
			preferred := findBoundary(runes, start+minChars, end)
			if preferred > start+minChars {
				end = preferred
			}
		}
		chunkText := strings.TrimSpace(string(runes[start:end]))
		if chunkText != "" {
			segments = append(segments, chunkInput{
				Text:       chunkText,
				TokenCount: estimateTokenCount(chunkText),
			})
		}
		if end == start {
			end = start + maxChars
			if end > total {
				end = total
			}
		}
		start = end
	}
	return segments
}

func normalizeNewlines(value string) string {
	if value == "" {
		return ""
	}
	replaced := strings.ReplaceAll(value, "\r\n", "\n")
	replaced = strings.ReplaceAll(replaced, "\r", "\n")
	return replaced
}

func findBoundary(runes []rune, min int, max int) int {
	if min < 0 {
		min = 0
	}
	if max > len(runes) {
		max = len(runes)
	}
	if max <= min {
		return min
	}
	boundaryChars := []rune{'\n', '。', '！', '？', '.', '!', '?'}
	boundarySet := make(map[rune]struct{}, len(boundaryChars))
	for _, ch := range boundaryChars {
		boundarySet[ch] = struct{}{}
	}
	for i := max - 1; i >= min; i-- {
		if _, ok := boundarySet[runes[i]]; ok {
			return i + 1
		}
	}
	return max
}

func estimateTokenCount(text string) int {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0
	}
	words := strings.Fields(trimmed)
	wordCount := len(words)
	runeCount := len([]rune(trimmed))
	estimate := wordCount + runeCount/3
	if estimate < wordCount {
		estimate = wordCount
	}
	if estimate <= 0 {
		estimate = runeCount/2 + 1
	}
	return estimate
}
