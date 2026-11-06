package domain

import (
	"strings"
	"unicode"
)

// ChunkMarkdown prefers paragraph/heading boundaries outside fenced code, with
// 3200-rune windows and 400 runes of overlap, without splitting UTF-8 sequences.
// A window is roughly 800 ASCII tokens but can exceed 3200 tokens in Japanese;
// retrieval chunks are not prompt budgets. Ask trims them to its input budget.
func ChunkMarkdown(content string) []string {
	const window, overlap = 3200, 400
	text := []rune(strings.TrimSpace(strings.ReplaceAll(content, "\r\n", "\n")))
	result := []string{}
	boundaries := make(map[int]bool)
	fenced := false
	offset := 0
	for _, line := range strings.SplitAfter(string(text), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
		}
		if !fenced && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			boundaries[offset] = true
		}
		offset += len([]rune(line))
	}
	for start := 0; start < len(text); {
		end := min(start+window, len(text))
		if end < len(text) {
			for n := end; n > start+window/2; n-- {
				if boundaries[n] {
					end = n
					break
				}
			}
			if !boundaries[end] {
				for n := end; n > start+window*3/4; n-- {
					if unicode.IsSpace(text[n-1]) {
						end = n
						break
					}
				}
			}
		}
		chunk := strings.TrimSpace(string(text[start:end]))
		if chunk != "" {
			result = append(result, chunk)
		}
		if end == len(text) {
			break
		}
		start = end - overlap
	}
	return result
}
