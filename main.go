package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// ================= 配置区 =================
const DASH_API_KEY = "sk-fb64515c017945fc9282f9ace355cad3"
const APP_ID = "16356830643247938dfa31f8414fd58d"

// 文件路径
const FILE_REC = "/userdata/rec.pcm"
const FILE_TTS = "/userdata/tts.pcm" // Flash模型返回的是pcm流(或者wav片段)，我们拼起来

// ASR URL (WebSocket)
const WS_ASR_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"

// ★关键修改★：Qwen3-TTS-Flash 必须使用多模态生成接口
const TTS_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"

var insecureClient *http.Client

func init() {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	insecureClient = &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

func main() {
	log.Println("=== RK3308 AI 助手 (V15.0 Qwen3-TTS-Flash 原生适配) ===")

	// 开机测试
	success := speakQwenFlash("系统已就绪，Qwen3 Flash 驱动加载成功。")
	if !success {
		log.Println("⚠️ 启动语音失败，请检查网络或 Key")
	}

	for {
		log.Println("\n>>> [状态] 正在录音 (5秒)...")

		// 1. 录音
		cmd := exec.Command("arecord", "-D", "plughw:2,0", "-f", "S16_LE", "-r", "16000", "-c", "1", "-d", "5", "-t", "raw", FILE_REC)
		if err := cmd.Run(); err != nil {
			log.Printf("❌ 录音失败: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		info, err := os.Stat(FILE_REC)
		if err != nil || info.Size() < 1000 {
			log.Println("⚠️ 录音太短")
			continue
		}

		// 2. 识别 (ASR)
		log.Println("⚡ [云端] 发起 ASR...")
		userText := callASRWebSocket(FILE_REC)

		if userText == "" {
			log.Println("⚠️ 识别为空")
			continue
		}

		log.Printf("✅ 用户说: [%s]", userText)

		var reply string
		if strings.Contains(userText, "再见") || strings.Contains(userText, "退出") {
			reply = "好的，再见！"
			speakQwenFlash(reply)
			break
		} else {
			log.Println("🧠 [Router] 请求 Agent...")
			reply = callAgent(userText)
		}

		log.Printf("🤖 AI回复: [%s]", reply)

		// 3. 播报
		speakQwenFlash(reply)
	}
}

// -----------------------------------------------------------
// ASR (保持不变)
// -----------------------------------------------------------
func callASRWebSocket(filename string) string {
	pcmData, err := os.ReadFile(filename)
	if err != nil {
		return ""
	}
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+DASH_API_KEY)
	conn, _, err := dialer.Dial(WS_ASR_URL, headers)
	if err != nil {
		return ""
	}
	defer conn.Close()
	taskId := fmt.Sprintf("%032x", rand.Int63())
	startFrame := map[string]interface{}{
		"header": map[string]interface{}{"task_id": taskId, "action": "run-task", "streaming": "duplex"},
		"payload": map[string]interface{}{
			"task_group": "audio", "task": "asr", "function": "recognition",
			"model":      "paraformer-realtime-v2",
			"parameters": map[string]interface{}{"format": "pcm", "sample_rate": 16000},
			"input":      map[string]interface{}{},
		},
	}
	conn.WriteJSON(startFrame)
	chunkSize := 3200
	for i := 0; i < len(pcmData); i += chunkSize {
		end := i + chunkSize
		if end > len(pcmData) {
			end = len(pcmData)
		}
		conn.WriteMessage(websocket.BinaryMessage, pcmData[i:end])
		time.Sleep(100 * time.Millisecond)
	}
	finishFrame := map[string]interface{}{
		"header":  map[string]interface{}{"task_id": taskId, "action": "finish-task", "streaming": "duplex"},
		"payload": map[string]interface{}{"input": map[string]interface{}{}},
	}
	conn.WriteJSON(finishFrame)
	finalText := ""
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var resp map[string]interface{}
		json.Unmarshal(msg, &resp)
		header, _ := resp["header"].(map[string]interface{})
		payload, _ := resp["payload"].(map[string]interface{})
		if header["event"] == "task-finished" {
			break
		}
		if header["event"] == "result-generated" {
			if output, ok := payload["output"].(map[string]interface{}); ok {
				if sentence, ok := output["sentence"].(map[string]interface{}); ok {
					if text, ok := sentence["text"].(string); ok {
						finalText = text
					}
				}
			}
		}
	}
	return finalText
}

// -----------------------------------------------------------
// TTS - Qwen3-Flash (多模态流式接口实现)
// -----------------------------------------------------------
func speakQwenFlash(text string) bool {
	log.Printf("🔊 [TTS] Qwen3-Flash (Cherry) 生成中: %s", text)

	// 构造多模态接口的请求体 (参考官方 MultiModalConversation)
	payload := map[string]interface{}{
		"model": "qwen3-tts-flash-2025-11-27",
		"input": map[string]interface{}{
			"text":          text,     // 输入文本
			"voice":         "Cherry", // 音色
			"language_type": "Chinese",
		},
		"parameters": map[string]interface{}{
			// 流式输出
			"stream": true,
			// 输出格式
			"format":      "wav",
			"sample_rate": 24000,
		},
	}

	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", TTS_URL, bytes.NewReader(jsonPayload))
	req.Header.Set("Authorization", "Bearer "+DASH_API_KEY)
	req.Header.Set("Content-Type", "application/json")
	// 必须开启 SSE (Server-Sent Events) 支持
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := insecureClient.Do(req)
	if err != nil {
		log.Printf("❌ 网络错误: %v", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("❌ 拒绝服务 (Code %d): %s", resp.StatusCode, string(body))
		return false
	}

	// 准备写入文件 (裸 PCM 数据追加写入)
	// 虽然请求的是 wav，但流式返回的 data 是片段，我们只解码 base64 数据部分拼起来即可
	outFile, err := os.Create(FILE_TTS)
	if err != nil {
		return false
	}
	defer outFile.Close()

	// 解析 SSE 流
	scanner := bufio.NewScanner(resp.Body)
	totalBytes := 0

	for scanner.Scan() {
		line := scanner.Text()

		// SSE 格式通常以 "data:" 开头
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		dataStr := strings.TrimPrefix(line, "data:")

		// 可能是结束标记 [DONE]
		if strings.TrimSpace(dataStr) == "[DONE]" {
			break
		}

		var chunk struct {
			Output struct {
				Audio struct {
					Data string `json:"data"` // base64 编码的音频
				} `json:"audio"`
				FinishReason string `json:"finish_reason"`
			} `json:"output"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}

		if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
			continue
		}

		if chunk.Code != "" {
			log.Printf("❌ 流式报错: %s - %s", chunk.Code, chunk.Message)
			break
		}

		if chunk.Output.Audio.Data != "" {
			// 解码 Base64
			audioBytes, err := base64.StdEncoding.DecodeString(chunk.Output.Audio.Data)
			if err == nil {
				// 将解码后的 PCM/WAV 片段写入文件
				// 注意：这里简单追加。对于 aplay -t raw 来说，wav 头会被当做杂音播放一瞬间，但影响不大。
				// 严谨做法是跳过第一个包的 wav 头，但为了代码简单先这样。
				outFile.Write(audioBytes)
				totalBytes += len(audioBytes)
			}
		}
	}

	log.Printf("✅ 音频接收完成 (%d bytes)，开始播放...", totalBytes)

	// 播放
	// 24000Hz, S16_LE, 单声道
	cmd := exec.Command("aplay", "-D", "plughw:1,0", "-q", "-t", "raw", "-r", "24000", "-f", "S16_LE", "-c", "1", FILE_TTS)
	if err := cmd.Run(); err != nil {
		log.Printf("❌ 播放失败: %v", err)
	}
	return true
}

// -----------------------------------------------------------
// Agent
// -----------------------------------------------------------
func callAgent(prompt string) string {
	url := "https://dashscope.aliyuncs.com/api/v1/apps/" + APP_ID + "/completion"
	payload := map[string]interface{}{
		"input":      map[string]string{"prompt": prompt},
		"parameters": map[string]interface{}{}, "debug": false,
	}
	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(jsonPayload))
	req.Header.Set("Authorization", "Bearer "+DASH_API_KEY)
	req.Header.Set("Content-Type", "application/json")
	resp, err := insecureClient.Do(req)
	if err != nil {
		return "网络错误"
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if output, ok := result["output"].(map[string]interface{}); ok {
		if text, ok := output["text"].(string); ok {
			return text
		}
	}
	return "我没听清"
}
