package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	// ★★★ 引入 WebRTC VAD 库 ★★★
	vado "github.com/maxhawkins/go-webrtc-vad"

	"ai_box/aec"
	// "ai_box/vad" // 移除旧的简陋 VAD
)

// ================= 配置区 =================
const DASH_API_KEY = "sk-fb64515c017945fc9282f9ace355cad3"
const APP_ID = "16356830643247938dfa31f8414fd58d"

const FILE_TTS = "/userdata/tts.pcm"
const WS_ASR_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"
const TTS_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"

// 状态定义
type AppState int

const (
	STATE_LISTENING AppState = iota
	STATE_THINKING
	STATE_SPEAKING
)

var (
	currentState   AppState = STATE_LISTENING
	stateMutex     sync.Mutex
	stopPlayChan   chan struct{}
	insecureClient *http.Client
	isExiting      bool
)

func init() {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	insecureClient = &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== RK3308 AI 助手 (V18.0 WebRTC VAD 适配版) ===")

	aecProc := aec.NewProcessor()

	// ★★★ 1. 初始化 WebRTC VAD ★★★
	vadEng, err := vado.New()
	if err != nil {
		log.Fatalf("VAD Init 失败 (请检查 libs 引用): %v", err)
	}
	vadEng.SetMode(2) // 模式 2 (Aggressive)，适合嘈杂环境，如果太不灵敏可改回 1

	stopPlayChan = make(chan struct{})

	// 启动核心循环
	go audioLoop(aecProc, vadEng)

	select {}
}

func logCost(stage string, start time.Time) {
	duration := time.Since(start)
	log.Printf("⏱️ [%s] 耗时: %d ms", stage, duration.Milliseconds())
}

// 核心音频循环：录音 -> AEC -> 缓冲适配 -> VAD
func audioLoop(aecProc *aec.Processor, vadEng *vado.VAD) {
	// 启动录音
	// period-size=256 是硬件 buffer 大小，决定了每次 read 的数据量
	cmd := exec.Command("arecord", "-D", "hw:2,0", "-c", "10", "-r", "16000", "-f", "S16_LE", "-t", "raw", "--period-size=256", "--buffer-size=16384")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("无法获取录音管道: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("无法启动录音: %v", err)
	}
	log.Println("🎤 麦克风已开启 (WebRTC VAD 监听中)...")

	// 硬件帧参数
	const HARDWARE_FRAME_SIZE = 256
	readChunkSize := HARDWARE_FRAME_SIZE * 10 * 2 // 10通道 * 2bytes
	readBuf := make([]byte, readChunkSize)

	// ★★★ 2. VAD 适配参数 ★★★
	// WebRTC 强制要求 20ms = 320 samples
	const VAD_FRAME_SAMPLES = 320
	// 蓄水池：用于暂存 AEC 处理后的数据
	vadAccumulator := make([]int16, 0, 1024)
	// 临时字节 buffer，用于传给 VAD
	vadByteBuf := make([]byte, VAD_FRAME_SAMPLES*2)

	var asrBuffer []int16
	vadSilenceCounter := 0
	vadSpeechCounter := 0
	isSpeechTriggered := false
	var silenceStartTime time.Time

	for {
		if isExiting {
			time.Sleep(1 * time.Second)
			continue
		}

		// 1. 读取硬件数据 (256 samples)
		_, err := io.ReadFull(stdout, readBuf)
		if err != nil {
			log.Printf("录音管道断开: %v", err)
			break
		}

		// 解析 10 通道
		rawInt16 := make([]int16, HARDWARE_FRAME_SIZE*10)
		for i := 0; i < len(rawInt16); i++ {
			rawInt16[i] = int16(binary.LittleEndian.Uint16(readBuf[i*2 : i*2+2]))
		}

		// 2. AEC 处理 (输入10通道 -> 输出1通道, 长度 256)
		cleanAudioChunk, _ := aecProc.Process(rawInt16)
		if cleanAudioChunk == nil {
			continue
		}

		// ★★★ 3. 存入蓄水池 (解决 256 vs 320 冲突) ★★★
		vadAccumulator = append(vadAccumulator, cleanAudioChunk...)

		// ★★★ 4. 循环切出 320 点的标准帧喂给 VAD ★★★
		for len(vadAccumulator) >= VAD_FRAME_SAMPLES {
			// 切出 20ms
			currentFrame := vadAccumulator[:VAD_FRAME_SAMPLES]
			vadAccumulator = vadAccumulator[VAD_FRAME_SAMPLES:]

			// 转成 byte 数组 (Little Endian)
			for i, v := range currentFrame {
				binary.LittleEndian.PutUint16(vadByteBuf[i*2:], uint16(v))
			}

			// 5. 调用 WebRTC VAD
			isSpeech, err := vadEng.Process(16000, vadByteBuf)
			if err != nil {
				// 忽略初始化错误的帧
				continue
			}

			// 6. 状态机逻辑 (此处逻辑与 V17 基本一致，只是步进单位变成了 20ms)
			stateMutex.Lock()
			curr := currentState
			stateMutex.Unlock()

			if isSpeech {
				vadSpeechCounter++
				vadSilenceCounter = 0
				silenceStartTime = time.Time{}
			} else {
				vadSilenceCounter++
				vadSpeechCounter = 0
				if vadSilenceCounter == 1 {
					silenceStartTime = time.Now()
				}
			}

			// === 打断逻辑 (Barge-in) ===
			// 15帧 * 20ms = 300ms 连续人声触发打断
			if vadSpeechCounter > 15 {
				if curr == STATE_SPEAKING {
					log.Println("🛑 [Barge-in] 检测到打断！")
					select {
					case stopPlayChan <- struct{}{}:
					default:
					}
					setState(STATE_LISTENING)
					asrBuffer = []int16{}
					isSpeechTriggered = true
				}
				if curr == STATE_LISTENING && !isSpeechTriggered {
					log.Println("👂 [VAD] 检测到说话开始...")
					isSpeechTriggered = true
				}
			}

			// === 收集音频 ===
			if curr == STATE_LISTENING {
				if isSpeechTriggered {
					asrBuffer = append(asrBuffer, currentFrame...)

					// 判停：40帧 * 20ms = 800ms 静音
					if vadSilenceCounter > 40 && len(asrBuffer) > 16000*0.5 {
						vadWaitDuration := time.Since(silenceStartTime)
						log.Printf("⚡ [VAD] 说话结束 (静音等待: %d ms), 开始处理...", vadWaitDuration.Milliseconds())

						bufferCopy := make([]int16, len(asrBuffer))
						copy(bufferCopy, asrBuffer)

						go processASR(bufferCopy)

						asrBuffer = []int16{}
						isSpeechTriggered = false
						vadSilenceCounter = 0
					}
				} else {
					// 预读缓冲 (保持 500ms)
					if len(asrBuffer) > 16000/2 {
						asrBuffer = asrBuffer[VAD_FRAME_SAMPLES:]
						asrBuffer = append(asrBuffer, currentFrame...)
					} else {
						asrBuffer = append(asrBuffer, currentFrame...)
					}
				}
			}
		}
	}
}

func setState(s AppState) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	currentState = s
}

// ================= 核心业务流程 (保留 V17 逻辑) =================
func processASR(pcmDataInt16 []int16) {
	pipelineStart := time.Now()
	setState(STATE_THINKING)

	pcmBytes := make([]byte, len(pcmDataInt16)*2)
	for i, v := range pcmDataInt16 {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	// 1. ASR
	asrStart := time.Now()
	log.Println("🚀 [ASR] 开始请求云端识别...")
	text := callASRWebSocket(pcmBytes)
	logCost("ASR识别", asrStart)

	if text == "" {
		log.Println("⚠️ [ASR] 识别为空，忽略")
		setState(STATE_LISTENING)
		return
	}
	log.Printf("✅ [ASR结果] 用户说: [%s]", text)

	// 指令拦截
	if strings.Contains(text, "关闭") || strings.Contains(text, "再见") || strings.Contains(text, "退下") {
		log.Println("🛑 [指令] 退出系统")
		isExiting = true
		finalReply := "好的，再见。"
		setState(STATE_SPEAKING)
		speakQwenFlash(finalReply)
		time.Sleep(3 * time.Second)
		os.Exit(0)
		return
	}

	// 2. Agent (使用 V17 的逻辑)
	agentStart := time.Now()
	log.Println("🧠 [Agent] 请求 LLM 思考中...")
	reply := callAgent(text)
	logCost("Agent思考", agentStart)

	log.Printf("🤖 [Agent回复] %s", reply)

	if isExiting {
		return
	}

	// 3. TTS
	ttsStart := time.Now()
	setState(STATE_SPEAKING)
	log.Println("🔊 [TTS] 开始生成并播放...")
	speakQwenFlash(reply)
	logCost("TTS播放全流程", ttsStart)

	logCost("===== 对话全链路总耗时 =====", pipelineStart)

	stateMutex.Lock()
	if currentState == STATE_SPEAKING && !isExiting {
		currentState = STATE_LISTENING
	}
	stateMutex.Unlock()
}

// ---------------- TTS (保持 V17) ----------------
func speakQwenFlash(text string) {
	payload := map[string]interface{}{
		"model":      "qwen3-tts-flash-2025-11-27",
		"input":      map[string]interface{}{"text": text, "voice": "Cherry", "language_type": "Chinese"},
		"parameters": map[string]interface{}{"stream": true, "format": "wav", "sample_rate": 24000},
	}
	jsonPayload, _ := json.Marshal(payload)

	reqStart := time.Now()
	req, _ := http.NewRequest("POST", TTS_URL, bytes.NewReader(jsonPayload))
	req.Header.Set("Authorization", "Bearer "+DASH_API_KEY)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := insecureClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	outFile, err := os.Create(FILE_TTS)
	if err != nil {
		return
	}
	defer outFile.Close()

	firstByteReceived := false
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
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
				Audio struct {
					Data string `json:"data"`
				} `json:"audio"`
			} `json:"output"`
		}
		json.Unmarshal([]byte(dataStr), &chunk)

		if chunk.Output.Audio.Data != "" {
			if !firstByteReceived {
				logCost("TTS首包延迟(TTFB)", reqStart)
				firstByteReceived = true
			}
			audioBytes, _ := base64.StdEncoding.DecodeString(chunk.Output.Audio.Data)
			outFile.Write(audioBytes)
		}
	}

	playCmd := exec.Command("aplay", "-D", "plughw:1,0", "-q", "-t", "raw", "-r", "24000", "-f", "S16_LE", "-c", "1", FILE_TTS)
	if err := playCmd.Start(); err != nil {
		return
	}

	done := make(chan error, 1)
	go func() { done <- playCmd.Wait() }()

	select {
	case <-done:
	case <-stopPlayChan:
		if playCmd.Process != nil {
			playCmd.Process.Kill()
			log.Println("🛑 [TTS] 播放被打断")
		}
	}
}

// ---------------- ASR (保持 V17) ----------------
func callASRWebSocket(pcmData []byte) string {
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
		time.Sleep(10 * time.Millisecond)
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

// ---------------- Agent (保持 V17) ----------------
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
