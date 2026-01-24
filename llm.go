package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func callAgentStream(ctx context.Context, prompt string, enableSearch bool) {
	flushChannel(ttsManagerChan)
	llmStart := time.Now()

	// 策略：联网搜索用 Max(准确但慢)，普通闲聊用 Turbo(极快)
	modelName := llmModelFast
	if enableSearch {
		modelName = llmModelSearch
		log.Println("LLM: 检测到时效性需求，已动态开启联网搜索...")
	}

	systemPrompt := "你是智能助手。仅在用户【明确要求播放音乐】（如“放首歌”、“听周杰伦”）时，才在回复末尾添加 [PLAY: 歌名]（随机播放用 [PLAY: RANDOM]）。" +
		"如果用户要求停止，加上 [STOP]。" +
		"回答天气、新闻、闲聊等普通问题时，【严禁】添加任何播放指令。"
	payload := map[string]interface{}{
		"model": modelName,
		"input": map[string]interface{}{
			"messages": []map[string]string{
				{"role": "system", "content": systemPrompt},
				{"role": "user", "content": prompt},
			},
		},
		"parameters": map[string]interface{}{
			"result_format":      "text",
			"incremental_output": true,
			"enable_search":      enableSearch, // 动态开关
		},
	}

	jsonBody, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", llmURL, bytes.NewReader(jsonBody))
	req.Header.Set("Authorization", "Bearer "+dashAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := insecureClient.Do(req)
	if err != nil {
		log.Printf("❌ LLM: 请求失败: %v", err)
		musicMgr.Unduck()
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var fullTextBuilder strings.Builder
	var chunkBuffer strings.Builder
	var firstChunkSent = false

	fmt.Print("LLM 推理: ")

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataStr := strings.TrimPrefix(line, "data:")
		if strings.TrimSpace(dataStr) == "[DONE]" {
			break
		}

		var chunk struct {
			Output struct {
				Text string `json:"text"`
			} `json:"output"`
		}
		if err := json.Unmarshal([]byte(dataStr), &chunk); err == nil && chunk.Output.Text != "" {
			clean := cleanText(chunk.Output.Text)
			if clean == "" {
				continue
			}
			fmt.Print(clean)
			fullTextBuilder.WriteString(clean)
			chunkBuffer.WriteString(clean)

			// 动态调整首包断句阈值：联网搜索时降低阈值以减少用户焦虑
			threshold := 30
			if enableSearch {
				threshold = 15 // 搜索时只要有15个字或标点就立刻播报
			}

			if !firstChunkSent {
				if strings.ContainsAny(clean, "，。！？,.!?\n") || chunkBuffer.Len() > threshold {
					firstChunkSent = true
					sendChunk(&chunkBuffer)
				}
			} else {
				if strings.ContainsAny(clean, "，。！？,.!?\n") || chunkBuffer.Len() > 80 {
					sendChunk(&chunkBuffer)
				}
			}
		}
	}
	fmt.Println()
	log.Printf("⏱LLM 推理结束，总耗时: %v", time.Since(llmStart))

	// 处理剩余文本
	sendChunk(&chunkBuffer)
	ttsManagerChan <- "[[END]]"

	// 指令解析逻辑
	fullText := fullTextBuilder.String()
	if strings.Contains(fullText, "[STOP]") {
		musicMgr.Stop()
	}
	if matches := regexp.MustCompile(`(?i)\[PLAY:\s*(.*?)\]`).FindStringSubmatch(fullText); len(matches) > 1 {
		musicMgr.SearchAndPlay(strings.TrimSpace(matches[1]))
	}
}

// 辅助函数：发送文本块到 TTS
func sendChunk(buf *strings.Builder) {
	text := regexp.MustCompile(`\[.*?\]`).ReplaceAllString(buf.String(), "")
	if strings.TrimSpace(text) != "" {
		ttsManagerChan <- strings.TrimSpace(text)
	}
	buf.Reset()
}
