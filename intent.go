package main

import "strings"

func cleanText(text string) string { return strings.TrimSpace(emojiRegex.ReplaceAllString(text, "")) }

func isExit(text string) bool {
	for _, w := range EXIT_WORDS {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

func isInterrupt(text string) bool {
	for _, w := range INTERRUPT_WORDS {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// 辅助判定：ASR 文本是否包含明确的点歌/换歌意图
func hasMusicIntent(text string) bool {
	// 包含这些动词通常意味着用户想操作音乐
	musicKeywords := []string{"播放", "想要听", "要听"}
	for _, k := range musicKeywords {
		if strings.Contains(text, k) {
			return true
		}
	}
	return false
}

// 仅用于“正在播放音乐时”的最小化切歌词表，避免误触正常聊天
func isQuickSwitch(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(musicPunct.ReplaceAllString(text, "")))
	switchWords := []string{"换首歌", "下一首", "切歌"}
	for _, w := range switchWords {
		if strings.Contains(normalized, w) {
			return true
		}
	}
	return false
}
