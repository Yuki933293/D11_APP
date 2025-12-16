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
	vado "github.com/maxhawkins/go-webrtc-vad"

	"ai_box/aec"
)

// ================= 配置区 =================
const DASH_API_KEY = "sk-fb64515c017945fc9282f9ace355cad3"
const APP_ID = "16356830643247938dfa31f8414fd58d"

const WS_ASR_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"
const TTS_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"

// ★★★ 核心定义：打断关键词 ★★★
var INTERRUPT_WORDS = []string{
	"等一下", "暂停", "停一下", "别说了", "闭嘴", "打住", "停止", "安静",
}

type AppState int

const (
	STATE_LISTENING AppState = iota
	STATE_THINKING
	STATE_SPEAKING
)

var (
	currentState    AppState = STATE_LISTENING
	stateMutex      sync.Mutex
	stopPlayChan    chan struct{}
	insecureClient  *http.Client
	isExiting       bool
	globalSessionID string
)

func init() {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	// 设置超时，防止网络卡死
	insecureClient = &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func generateSessionID() string {
	return fmt.Sprintf("session-%d-%d", time.Now().Unix(), rand.Intn(10000))
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== RK3308 AI 助手 (V18.9 云端熔断修正版) ===")

	globalSessionID = generateSessionID()
	log.Printf("✨ 会话ID: %s", globalSessionID)
	log.Printf("🛡️ 熔断关键词: %v", INTERRUPT_WORDS)

	aecProc := aec.NewProcessor()
	vadEng, err := vado.New()
	if err != nil {
		log.Fatalf("VAD Init 失败: %v", err)
	}

	// 保持 Mode 3 强力抗噪
	vadEng.SetMode(3)

	// 带缓冲通道，防信号丢失
	stopPlayChan = make(chan struct{}, 1)

	go audioLoop(aecProc, vadEng)

	select {}
}

func logCost(stage string, start time.Time) {
	duration := time.Since(start)
	log.Printf("⏱️ [%s] 耗时: %d ms", stage, duration.Milliseconds())
}

// 辅助函数：检查关键词
func containsKeyword(text string) bool {
	for _, kw := range INTERRUPT_WORDS {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// 辅助函数：执行物理停止
func performStop() {
	// 1. 发送停止信号 (非阻塞)
	select {
	case stopPlayChan <- struct{}{}:
	default:
	}
	// 2. 状态强制归位
	stateMutex.Lock()
	currentState = STATE_LISTENING
	stateMutex.Unlock()
}

func audioLoop(aecProc *aec.Processor, vadEng *vado.VAD) {
	cmd := exec.Command("arecord", "-D", "hw:2,0", "-c", "10", "-r", "16000", "-f", "S16_LE", "-t", "raw", "--period-size=256", "--buffer-size=16384")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	log.Println("🎤 麦克风已开启...")

	const HARDWARE_FRAME_SIZE = 256
	readBuf := make([]byte, HARDWARE_FRAME_SIZE*10*2)

	const VAD_FRAME_SAMPLES = 320
	vadAccumulator := make([]int16, 0, 1024)
	vadByteBuf := make([]byte, VAD_FRAME_SAMPLES*2)

	var asrBuffer []int16
	vadSilenceCounter := 0
	vadSpeechCounter := 0
	isSpeechTriggered := false

	// ★★★ 修复点 1: 声明变量 ★★★
	var silenceStartTime time.Time

	for {
		if isExiting {
			time.Sleep(1 * time.Second)
			continue
		}

		_, err := io.ReadFull(stdout, readBuf)
		if err != nil {
			break
		}

		rawInt16 := make([]int16, HARDWARE_FRAME_SIZE*10)
		for i := 0; i < len(rawInt16); i++ {
			rawInt16[i] = int16(binary.LittleEndian.Uint16(readBuf[i*2 : i*2+2]))
		}

		cleanAudioChunk, _ := aecProc.Process(rawInt16)
		if cleanAudioChunk == nil {
			continue
		}

		vadAccumulator = append(vadAccumulator, cleanAudioChunk...)

		for len(vadAccumulator) >= VAD_FRAME_SAMPLES {
			currentFrame := vadAccumulator[:VAD_FRAME_SAMPLES]
			vadAccumulator = vadAccumulator[VAD_FRAME_SAMPLES:]

			for i, v := range currentFrame {
				binary.LittleEndian.PutUint16(vadByteBuf[i*2:], uint16(v))
			}

			isSpeech, _ := vadEng.Process(16000, vadByteBuf)

			stateMutex.Lock()
			curr := currentState
			stateMutex.Unlock()

			if isSpeech {
				vadSpeechCounter++
				vadSilenceCounter = 0
				// 重置静音开始时间
				silenceStartTime = time.Time{}
			} else {
				vadSilenceCounter++
				vadSpeechCounter = 0
				// 记录静音开始时间
				if vadSilenceCounter == 1 {
					silenceStartTime = time.Now()
				}
			}

			// === VAD 触发逻辑 ===
			if vadSpeechCounter > 15 {
				if !isSpeechTriggered {
					if curr == STATE_SPEAKING || curr == STATE_THINKING {
						log.Println("🛡️ [VAD] 监听到疑似打断，后台校验中...")
					} else {
						log.Println("👂 [VAD] 开始录音...")
					}
					isSpeechTriggered = true
				}
			}

			if isSpeechTriggered {
				asrBuffer = append(asrBuffer, currentFrame...)

				// 判停：800ms 静音
				if vadSilenceCounter > 40 && len(asrBuffer) > 16000*0.5 {

					// ★★★ 修复点 2: 使用变量 (打印日志) ★★★
					// 之前这里漏掉了使用 silenceStartTime，导致报错
					vadWaitDuration := time.Since(silenceStartTime)

					bufferCopy := make([]int16, len(asrBuffer))
					copy(bufferCopy, asrBuffer)

					asrBuffer = []int16{}
					isSpeechTriggered = false
					vadSilenceCounter = 0

					// ★★★ 核心分流 ★★★
					if curr == STATE_LISTENING {
						log.Printf("⚡ [VAD] 录音结束 (静音: %d ms)，正常处理", vadWaitDuration.Milliseconds())
						go processASR(bufferCopy)
					} else {
						log.Printf("⚡ [VAD] 录音结束，校验打断词...")
						go processInterruptionCheck(bufferCopy)
					}
				}
			} else {
				// Pre-roll
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

func setState(s AppState) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	currentState = s
}

// ★★★ 第一道防线：专用打断校验通道 ★★★
func processInterruptionCheck(pcmDataInt16 []int16) {
	pcmBytes := make([]byte, len(pcmDataInt16)*2)
	for i, v := range pcmDataInt16 {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	text := callASRWebSocket(pcmBytes)
	if text == "" {
		return
	}

	log.Printf("🕵️ [打断校验] 识别内容: [%s]", text)

	if containsKeyword(text) {
		log.Println("🛑 [校验通过] 触发打断，停止播放！")
		performStop()
	} else {
		log.Println("🛡️ [校验忽略] 非打断词，继续播放")
	}
}

// ★★★ 主对话链路 ★★★
func processASR(pcmDataInt16 []int16) {
	pipelineStart := time.Now()
	setState(STATE_THINKING)

	pcmBytes := make([]byte, len(pcmDataInt16)*2)
	for i, v := range pcmDataInt16 {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	asrStart := time.Now()
	text := callASRWebSocket(pcmBytes)
	logCost("ASR识别", asrStart)

	if text == "" {
		setState(STATE_LISTENING)
		return
	}
	log.Printf("✅ 用户说: [%s]", text)

	// ★★★ 第二道防线：主流程指令熔断 ★★★
	if containsKeyword(text) {
		log.Println("🚫 [指令熔断] 检测到停止指令，不请求 LLM")
		performStop()
		setState(STATE_LISTENING)
		speakQwenFlashStream("好的")
		return
	}

	// 特殊指令拦截
	if strings.Contains(text, "关闭") || strings.Contains(text, "再见") {
		isExiting = true
		speakQwenFlashStream("再见")
		time.Sleep(2 * time.Second)
		os.Exit(0)
		return
	}

	if strings.Contains(text, "重置") || strings.Contains(text, "忘掉") {
		globalSessionID = generateSessionID()
		speakQwenFlashStream("记忆已重置")
		setState(STATE_LISTENING)
		return
	}

	llmStart := time.Now()
	reply := callAgent(text)
	logCost("LLM思考", llmStart)
	log.Printf("🤖 AI回复: %s", reply)

	// ★★★ 第三道防线：过时检查 ★★★
	stateMutex.Lock()
	if currentState != STATE_THINKING || isExiting {
		stateMutex.Unlock()
		log.Println("⚠️ [Process] 状态已变更(检测到打断)，放弃播放")
		return
	}
	currentState = STATE_SPEAKING
	stateMutex.Unlock()

	speakQwenFlashStream(reply)
	logCost("全链路总耗时", pipelineStart)

	stateMutex.Lock()
	if currentState == STATE_SPEAKING && !isExiting {
		currentState = STATE_LISTENING
	}
	stateMutex.Unlock()
}

// ---------------- TTS (流式 + 缓冲清理) ----------------
func speakQwenFlashStream(text string) {
	// 清理僵尸信号
	select {
	case <-stopPlayChan:
		log.Println("🧹 [TTS] 清理残留信号")
	default:
	}

	payload := map[string]interface{}{
		"model":      "qwen3-tts-flash-2025-11-27",
		"input":      map[string]interface{}{"text": text, "voice": "Cherry", "language_type": "Chinese"},
		"parameters": map[string]interface{}{"stream": true, "format": "pcm", "sample_rate": 24000},
	}
	jsonPayload, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", TTS_URL, bytes.NewReader(jsonPayload))
	req.Header.Set("Authorization", "Bearer "+DASH_API_KEY)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := insecureClient.Do(req)
	if err != nil {
		log.Printf("TTS 请求失败: %v", err)
		return
	}
	defer resp.Body.Close()

	playCmd := exec.Command("aplay", "-D", "plughw:1,0", "-q", "-t", "raw", "-r", "24000", "-f", "S16_LE", "-c", "1")
	playStdin, err := playCmd.StdinPipe()
	if err != nil {
		return
	}

	if err := playCmd.Start(); err != nil {
		return
	}

	playDone := make(chan error, 1)
	go func() { playDone <- playCmd.Wait() }()

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	firstPacket := true
	startTime := time.Now()

	for scanner.Scan() {
		// 检查打断信号
		select {
		case <-stopPlayChan:
			log.Println("🛑 [TTS] 收到停止信号，中断播放")
			playCmd.Process.Kill()
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
				Audio struct {
					Data string `json:"data"`
				} `json:"audio"`
			} `json:"output"`
		}

		if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
			continue
		}

		if chunk.Output.Audio.Data != "" {
			audioBytes, err := base64.StdEncoding.DecodeString(chunk.Output.Audio.Data)
			if err != nil {
				continue
			}

			if firstPacket {
				logCost("TTS 首包", startTime)
				firstPacket = false
			}

			_, err = playStdin.Write(audioBytes)
			if err != nil {
				break
			}
		}
	}
	playStdin.Close()

	select {
	case <-playDone:
	case <-stopPlayChan:
		if playCmd.Process != nil {
			playCmd.Process.Kill()
		}
	}
}

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
		time.Sleep(5 * time.Millisecond)
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

func callAgent(prompt string) string {
	url := "https://dashscope.aliyuncs.com/api/v1/apps/" + APP_ID + "/completion"

	payload := map[string]interface{}{
		"input": map[string]string{
			"prompt":     prompt,
			"session_id": globalSessionID,
		},
		"parameters": map[string]interface{}{},
		"debug":      false,
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
