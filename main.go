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

	"ai_box/aec"
	"ai_box/vad"
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

	// 新增：全局退出标志，防止“诈尸”
	isExiting bool
)

func init() {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	insecureClient = &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

func main() {
	// 设置日志包含微秒，方便排查延迟
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== RK3308 AI 助手 (V17.0 性能分析与逻辑修复版) ===")

	aecProc := aec.NewProcessor()
	vadEng := vad.NewEngine()
	stopPlayChan = make(chan struct{})

	// 启动核心循环
	go audioLoop(aecProc, vadEng)

	select {}
}

// 辅助函数：计算耗时
func logCost(stage string, start time.Time) {
	duration := time.Since(start)
	log.Printf("⏱️ [%s] 耗时: %d ms", stage, duration.Milliseconds())
}

func audioLoop(aecProc *aec.Processor, vadEng *vad.Engine) {
	cmd := exec.Command("arecord", "-D", "hw:2,0", "-c", "10", "-r", "16000", "-f", "S16_LE", "-t", "raw")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("无法获取录音管道: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("无法启动录音: %v", err)
	}
	log.Println("🎤 麦克风已开启...")

	frameSize := 256
	channels := 10
	frameBytes := frameSize * channels * 2
	readBuf := make([]byte, frameBytes)
	var asrBuffer []int16

	// VAD 参数
	vadSilenceCounter := 0
	vadSpeechCounter := 0
	isSpeechTriggered := false

	// 延迟调试：记录用户何时停止说话
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

		rawInt16 := make([]int16, frameSize*channels)
		for i := 0; i < len(rawInt16); i++ {
			rawInt16[i] = int16(binary.LittleEndian.Uint16(readBuf[i*2 : i*2+2]))
		}

		// AEC 耗时极短，一般忽略，但为了严谨也可以打点
		cleanAudio, _ := aecProc.Process(rawInt16)
		if cleanAudio == nil {
			continue
		}

		isSpeech := vadEng.IsSpeech(cleanAudio)

		stateMutex.Lock()
		curr := currentState
		stateMutex.Unlock()

		if isSpeech {
			vadSpeechCounter++
			vadSilenceCounter = 0
			// 每次检测到有人说话，重置静音计时
			silenceStartTime = time.Time{}
		} else {
			vadSilenceCounter++
			vadSpeechCounter = 0
			// 刚开始静音时记录时间
			if vadSilenceCounter == 1 {
				silenceStartTime = time.Now()
			}
		}

		// === 打断逻辑 ===
		if vadSpeechCounter > 5 {
			if curr == STATE_SPEAKING {
				log.Println("🛑 [Barge-in] 检测到打断！执行 Kill...")
				select {
				case stopPlayChan <- struct{}{}:
				default:
				}
				setState(STATE_LISTENING)
				asrBuffer = []int16{}
				isSpeechTriggered = true
			}

			if curr == STATE_LISTENING {
				if !isSpeechTriggered {
					log.Println("👂 [VAD] 检测到人声开始...")
					isSpeechTriggered = true
				}
			}
		}

		// === 收集音频与发送 ===
		if curr == STATE_LISTENING {
			if isSpeechTriggered {
				asrBuffer = append(asrBuffer, cleanAudio...)

				// 判停逻辑：800ms 静音 (50帧)
				if vadSilenceCounter > 50 && len(asrBuffer) > 16000*1 {
					// 计算 VAD 等待带来的延迟
					vadWaitDuration := time.Since(silenceStartTime)
					log.Printf("⚡ [VAD] 说话结束 (VAD等待确认耗时: %d ms), 开始处理...", vadWaitDuration.Milliseconds())

					bufferCopy := make([]int16, len(asrBuffer))
					copy(bufferCopy, asrBuffer)

					// 异步处理，不阻塞录音
					go processASR(bufferCopy)

					asrBuffer = []int16{}
					isSpeechTriggered = false
					vadSilenceCounter = 0
				}
			} else {
				// 预读缓冲 500ms
				if len(asrBuffer) > 16000/2 {
					asrBuffer = asrBuffer[256:]
					asrBuffer = append(asrBuffer, cleanAudio...)
				} else {
					asrBuffer = append(asrBuffer, cleanAudio...)
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

// ================= 核心业务流程 (含埋点) =================
func processASR(pcmDataInt16 []int16) {
	// 全流程计时起点
	pipelineStart := time.Now()

	setState(STATE_THINKING)

	pcmBytes := make([]byte, len(pcmDataInt16)*2)
	for i, v := range pcmDataInt16 {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	// 1. ASR 阶段
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

	// === 修复“诈尸”BUG：在这里拦截控制指令 ===
	// 在请求 Agent 之前，先看是不是要关闭
	if strings.Contains(text, "关闭") || strings.Contains(text, "再见") || strings.Contains(text, "退下") {
		log.Println("🛑 [指令] 检测到退出指令，系统即将关闭...")
		isExiting = true // 锁死状态，防止 audioLoop 继续收音

		// 播放最后的告别 (不走 Agent，直接 TTS)
		finalReply := "好的，正在关闭系统，再见。"

		setState(STATE_SPEAKING) // 此时虽然 Exiting，但为了播放还是切一下状态
		speakQwenFlash(finalReply)

		// 等待播放完 (简单 Sleep，或者优化为等待播放结束信号)
		time.Sleep(4 * time.Second)

		log.Println("👋 [系统] 进程退出")
		os.Exit(0)
		return
	}

	// 2. Agent 阶段 (LLM)
	agentStart := time.Now()
	log.Println("🧠 [Agent] 请求 LLM 思考中...")
	reply := callAgent(text)
	logCost("Agent思考", agentStart)

	log.Printf("🤖 [Agent回复] %s", reply)

	// 如果在思考期间，用户又打断说“关闭”了，这里再次检查
	if isExiting {
		return
	}

	// 3. TTS 阶段
	ttsStart := time.Now()
	setState(STATE_SPEAKING)
	log.Println("🔊 [TTS] 开始生成并播放...")
	speakQwenFlash(reply)
	logCost("TTS播放全流程", ttsStart)

	// 4. 结束，统计总耗时
	logCost("===== 对话全链路总耗时 =====", pipelineStart)

	// 恢复聆听
	stateMutex.Lock()
	if currentState == STATE_SPEAKING && !isExiting {
		currentState = STATE_LISTENING
	}
	stateMutex.Unlock()
}

// ---------------- TTS (支持打断) ----------------
func speakQwenFlash(text string) {
	// 下载与播放...
	// 注意：这里为了测算首包延迟，我们在收到第一个数据包时打个日志

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

	// 启动播放进程 (流式追加写入，这里先启动 aplay 监听文件流可能更复杂，
	// 简单起见我们这里还是下载完再播，或者一边写一边播。
	// 为了排查延迟，我们先记录“收到第一个包”的时间)

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
			log.Println("🛑 [TTS] 播放被打断")
		}
	}
}

// ---------------- ASR & Agent (保持原逻辑) ----------------
func callASRWebSocket(pcmData []byte) string {
	// ... 保持原有代码不变 ...
	// 为了节省篇幅，这里复用你之前的 ASR 代码
	// 但建议在 send loop 和 recv loop 里如果你觉得慢，也可以加 log

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
		time.Sleep(10 * time.Millisecond) // 这里如果sleep太久会增加延迟，建议调小
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
