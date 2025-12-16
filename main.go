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
	insecureClient = &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

func generateSessionID() string {
	return fmt.Sprintf("session-%d-%d", time.Now().Unix(), rand.Intn(10000))
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== RK3308 AI 助手 (V18.7 最终完美版) ===")

	globalSessionID = generateSessionID()
	log.Printf("✨ 会话ID: %s", globalSessionID)

	aecProc := aec.NewProcessor()
	vadEng, err := vado.New()
	if err != nil {
		log.Fatalf("VAD Init 失败: %v", err)
	}

	// ★★★ VAD 策略: Mode 3 (强力抗噪，防拍手误触) ★★★
	vadEng.SetMode(3)

	// ★★★ 核心修复 1: 使用带缓冲的 Channel (容量1) ★★★
	// 确保 audioLoop 发出的打断信号一定能被 speak 函数接收到，不会丢失
	stopPlayChan = make(chan struct{}, 1)

	go audioLoop(aecProc, vadEng)

	select {}
}

func logCost(stage string, start time.Time) {
	duration := time.Since(start)
	log.Printf("⏱️ [%s] 耗时: %d ms", stage, duration.Milliseconds())
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
				silenceStartTime = time.Time{}
			} else {
				vadSilenceCounter++
				vadSpeechCounter = 0
				if vadSilenceCounter == 1 {
					silenceStartTime = time.Now()
				}
			}

			// ★★★ VAD 策略: 阈值回调至 15帧 (300ms) ★★★
			// Mode 3 已经过滤了噪音，所以这里可以用较短的时间阈值，确保"等一下"能生效
			if vadSpeechCounter > 15 {
				if curr == STATE_SPEAKING || curr == STATE_THINKING {
					log.Println("🛑 [Barge-in] 检测到人声指令，执行打断！")

					// 非阻塞发送 (由于有缓冲，这里几乎肯定能发进去)
					select {
					case stopPlayChan <- struct{}{}:
					default:
						// 如果缓冲区满了(极少见)，说明已经有一个打断信号了，忽略本次
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

			if curr == STATE_LISTENING {
				if isSpeechTriggered {
					asrBuffer = append(asrBuffer, currentFrame...)

					// 判停：800ms 静音
					if vadSilenceCounter > 40 && len(asrBuffer) > 16000*0.5 {
						vadWaitDuration := time.Since(silenceStartTime)
						log.Printf("⚡ [VAD] 说话结束 (静音: %d ms)", vadWaitDuration.Milliseconds())

						bufferCopy := make([]int16, len(asrBuffer))
						copy(bufferCopy, asrBuffer)

						go processASR(bufferCopy)

						asrBuffer = []int16{}
						isSpeechTriggered = false
						vadSilenceCounter = 0
					}
				} else {
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

func processASR(pcmDataInt16 []int16) {
	// [1] 全链路计时起点
	pipelineStart := time.Now()

	setState(STATE_THINKING)

	pcmBytes := make([]byte, len(pcmDataInt16)*2)
	for i, v := range pcmDataInt16 {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	// ==========================================
	// [2] 测量 ASR (语音转文字) 耗时
	// ==========================================
	asrStart := time.Now()
	text := callASRWebSocket(pcmBytes)

	// ★★★ 新增日志 ★★★
	logCost("ASR识别(语音转文字)", asrStart)

	if text == "" {
		setState(STATE_LISTENING)
		return
	}
	log.Printf("✅ 用户说: [%s]", text)

	// 指令拦截
	if strings.Contains(text, "关闭") || strings.Contains(text, "再见") {
		isExiting = true
		speakQwenFlashStream("好的，再见。")
		time.Sleep(3 * time.Second)
		os.Exit(0)
		return
	}

	if strings.Contains(text, "重置") || strings.Contains(text, "忘掉") {
		globalSessionID = generateSessionID()
		speakQwenFlashStream("好的，我已经重置了记忆。")
		stateMutex.Lock()
		currentState = STATE_LISTENING
		stateMutex.Unlock()
		return
	}

	// ==========================================
	// [3] 测量 LLM (大模型思考) 耗时
	// ==========================================
	llmStart := time.Now()
	reply := callAgent(text)

	// ★★★ 新增日志 ★★★
	logCost("LLM思考(智能生成)", llmStart)

	log.Printf("🤖 AI回复: %s", reply)

	// 过时检查
	stateMutex.Lock()
	if currentState != STATE_THINKING || isExiting {
		stateMutex.Unlock()
		log.Println("⚠️ [Process] 状态已变更(检测到打断)，放弃播放")
		return
	}
	currentState = STATE_SPEAKING
	stateMutex.Unlock()

	// ==========================================
	// [4] TTS 播放 (TTFB 已在函数内部打印)
	// ==========================================
	speakQwenFlashStream(reply)

	// [5] 全链路总耗时
	logCost("全链路总耗时(对话闭环)", pipelineStart)

	stateMutex.Lock()
	if currentState == STATE_SPEAKING && !isExiting {
		currentState = STATE_LISTENING
	}
	stateMutex.Unlock()
}

// ---------------- TTS (流式 + 缓冲清理) ----------------
func speakQwenFlashStream(text string) {
	// ★★★ 核心修复 2: 清理“僵尸”信号 (Drain Channel) ★★★
	// 在开始新播放前，排空可能残留的旧打断信号，防止误杀本次播放
	select {
	case <-stopPlayChan:
		log.Println("🧹 [TTS] 清理上一轮残留的打断信号")
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
		// 检查打断信号 (现在 channel 有缓冲，信号不会丢了)
		select {
		case <-stopPlayChan:
			log.Println("🛑 [TTS] 流式播放被打断")
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
				logCost("TTS 首包延迟 (TTFB)", startTime)
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
			log.Println("🛑 [TTS] 播放尾部被打断")
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
			"session_id": globalSessionID, // 携带记忆
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
