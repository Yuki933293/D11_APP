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

const FILE_TTS = "/userdata/tts.pcm"
const WS_ASR_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"
const TTS_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"

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
	log.Println("=== RK3308 AI 助手 (V18.2 竞态修复版) ===")

	aecProc := aec.NewProcessor()
	vadEng, err := vado.New()
	if err != nil {
		log.Fatalf("VAD Init 失败: %v", err)
	}
	vadEng.SetMode(2) // Aggressive

	stopPlayChan = make(chan struct{})

	go audioLoop(aecProc, vadEng)

	select {}
}

func logCost(stage string, start time.Time) {
	duration := time.Since(start)
	log.Printf("⏱️ [%s] 耗时: %d ms", stage, duration.Milliseconds())
}

func audioLoop(aecProc *aec.Processor, vadEng *vado.VAD) {
	// 启动 arecord
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

	// VAD 适配 (320 samples)
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

		// 解析 10 通道
		rawInt16 := make([]int16, HARDWARE_FRAME_SIZE*10)
		for i := 0; i < len(rawInt16); i++ {
			rawInt16[i] = int16(binary.LittleEndian.Uint16(readBuf[i*2 : i*2+2]))
		}

		// AEC
		cleanAudioChunk, _ := aecProc.Process(rawInt16)
		if cleanAudioChunk == nil {
			continue
		}

		// 存入蓄水池
		vadAccumulator = append(vadAccumulator, cleanAudioChunk...)

		// 循环切出 320 点
		for len(vadAccumulator) >= VAD_FRAME_SAMPLES {
			currentFrame := vadAccumulator[:VAD_FRAME_SAMPLES]
			vadAccumulator = vadAccumulator[VAD_FRAME_SAMPLES:]

			for i, v := range currentFrame {
				binary.LittleEndian.PutUint16(vadByteBuf[i*2:], uint16(v))
			}

			// VAD 检测
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

			// ★★★ 修复点 1: 扩大打断监测范围 ★★★
			// 无论是“正在说话”还是“正在思考(等待LLM)”，只要用户说话，一律打断
			if vadSpeechCounter > 15 { // 300ms
				if curr == STATE_SPEAKING || curr == STATE_THINKING {
					log.Println("🛑 [Barge-in] 检测到打断，重置状态！")

					// 1. 停止播放 (如果在播)
					select {
					case stopPlayChan <- struct{}{}:
					default:
					}

					// 2. 强制切回监听
					setState(STATE_LISTENING)

					// 3. 准备接收新语音
					asrBuffer = []int16{}
					isSpeechTriggered = true
				}

				// 正常监听模式下的触发
				if curr == STATE_LISTENING && !isSpeechTriggered {
					log.Println("👂 [VAD] 检测到说话开始...")
					isSpeechTriggered = true
				}
			}

			// 收集音频
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
					// Pre-roll buffer
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
	pipelineStart := time.Now()
	setState(STATE_THINKING)

	pcmBytes := make([]byte, len(pcmDataInt16)*2)
	for i, v := range pcmDataInt16 {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	// 1. ASR
	text := callASRWebSocket(pcmBytes)
	if text == "" {
		setState(STATE_LISTENING)
		return
	}
	log.Printf("✅ 用户说: [%s]", text)

	// 指令拦截
	if strings.Contains(text, "关闭") || strings.Contains(text, "再见") {
		isExiting = true
		speakQwenFlash("好的，再见。")
		time.Sleep(3 * time.Second)
		os.Exit(0)
		return
	}

	// 2. Agent (无 Session ID)
	reply := callAgent(text)
	log.Printf("🤖 AI回复: %s", reply)

	// ★★★ 修复点 2: 关键的过时检查 (Guard Clause) ★★★
	stateMutex.Lock()
	// 如果在 LLM 思考期间，audioLoop 发现用户又说话了，会将状态强行改为 LISTENING。
	// 此时这里检查到状态不对，就知道自己生成的内容已经过时了，直接丢弃。
	if currentState != STATE_THINKING || isExiting {
		stateMutex.Unlock()
		log.Println("⚠️ [Process] 状态已变更(检测到打断)，放弃播放旧内容")
		return // 直接退出，不再播放
	}
	// 确认安全，进入播放状态
	currentState = STATE_SPEAKING
	stateMutex.Unlock()

	// 3. TTS
	speakQwenFlash(reply)
	logCost("全链路耗时", pipelineStart)

	stateMutex.Lock()
	if currentState == STATE_SPEAKING && !isExiting {
		currentState = STATE_LISTENING
	}
	stateMutex.Unlock()
}

// ---------------- TTS ----------------
func speakQwenFlash(text string) {
	payload := map[string]interface{}{
		"model":      "qwen3-tts-flash-2025-11-27",
		"input":      map[string]interface{}{"text": text, "voice": "Cherry", "language_type": "Chinese"},
		"parameters": map[string]interface{}{"stream": true, "format": "wav", "sample_rate": 24000},
	}
	jsonPayload, _ := json.Marshal(payload)

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
			audioBytes, _ := base64.StdEncoding.DecodeString(chunk.Output.Audio.Data)
			outFile.Write(audioBytes)
		}
	}
	outFile.Close()

	// 播放
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
			log.Println("🛑 [TTS] 播放停止")
		}
	}
}

// ---------------- ASR ----------------
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

// ---------------- Agent (无 Session ID) ----------------
func callAgent(prompt string) string {
	url := "https://dashscope.aliyuncs.com/api/v1/apps/" + APP_ID + "/completion"

	payload := map[string]interface{}{
		"input": map[string]string{
			"prompt": prompt,
			// session_id 已移除
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
