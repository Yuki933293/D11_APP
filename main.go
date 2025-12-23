package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	vado "github.com/maxhawkins/go-webrtc-vad"

	"ai_box/aec"
)

// ================= 配置区 =================
const DASH_API_KEY = "sk-fb64515c017945fc9282f9ace355cad3"

const TTS_WS_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"
const LLM_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text-generation/generation"
const WS_ASR_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"

// 打断词
var EXIT_WORDS = []string{"关闭系统", "关机", "退出程序", "再见", "退下", "拜拜"}
var INTERRUPT_WORDS = []string{"闭嘴", "停止", "安静", "别说了", "暂停", "打断"}

type AppState int

const (
	STATE_LISTENING AppState = iota
	STATE_THINKING
	STATE_SPEAKING
)

// 全局性能统计变量
var (
	tsVadEnd     time.Time
	tsAsrEnd     time.Time
	tsLlmStart   time.Time
	tsLlmFirst   time.Time
	tsTtsStart   time.Time
	tsFirstAudio time.Time
)

var (
	currentState AppState = STATE_LISTENING
	stateMutex   sync.Mutex

	// 上下文控制
	sessionCtx    context.Context
	sessionCancel context.CancelFunc
	ctxMutex      sync.Mutex

	// ★ 会话 ID 控制 (解决连接残留问题)
	currentSessionID string
	sessionIDMutex   sync.Mutex

	insecureClient *http.Client

	ttsManagerChan chan string
	audioPcmChan   chan []byte
	playerStdin    io.WriteCloser
	playerCmd      *exec.Cmd
	playerMutex    sync.Mutex

	emojiRegex *regexp.Regexp
)

func init() {
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 60 * time.Second}
	tr := &http.Transport{
		DialContext:     dialer.DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:    100,
		IdleConnTimeout: 90 * time.Second,
	}
	insecureClient = &http.Client{Transport: tr, Timeout: 0}
	rand.Seed(time.Now().UnixNano())
	emojiRegex = regexp.MustCompile(`[\x{1F600}-\x{1F64F}\x{1F300}-\x{1F5FF}\x{1F680}-\x{1F6FF}]`)
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== RK3308 AI 助手 (V149.0 会话ID对齐版) ===")

	exec.Command("amixer", "-c", "2", "sset", "Master", "100%", "unmute").Run()
	exec.Command("amixer", "-c", "2", "sset", "Playback", "100%", "unmute").Run()
	exec.Command("amixer", "-c", "2", "sset", "Capture", "100%", "unmute").Run()

	ttsManagerChan = make(chan string, 500)
	audioPcmChan = make(chan []byte, 4000)

	sessionCtx, sessionCancel = context.WithCancel(context.Background())
	currentSessionID = uuid.New().String()

	go audioPlayer()
	go ttsManagerLoop()

	aecProc := aec.NewProcessor()
	vadEng, err := vado.New()
	if err != nil {
		log.Fatal("❌ VAD 初始化失败:", err)
	}
	// VAD Mode 3 + 8s 强制截断
	vadEng.SetMode(3)

	go audioLoop(aecProc, vadEng)

	select {}
}

func cleanText(text string) string { return strings.TrimSpace(emojiRegex.ReplaceAllString(text, "")) }

// ================= 播放器 =================
func audioPlayer() {
	cmd := exec.Command("aplay", "-D", "default", "-t", "raw", "-r", "22050", "-f", "S16_LE", "-c", "1", "-B", "200000")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	playerMutex.Lock()
	playerCmd = cmd
	playerStdin = stdin
	playerMutex.Unlock()
	log.Println("🔈 播放器就绪")

	for pcmData := range audioPcmChan {
		if _, err := stdin.Write(pcmData); err != nil {
			cmd.Process.Kill()
			playerCmd.Wait()
			go audioPlayer()
			return
		}
	}
}

// ================= TTS 管理器 (★ 修复连接残留) =================
func ttsManagerLoop() {
	var conn *websocket.Conn
	var wg sync.WaitGroup
	var currentTaskID string
	var localSessionID string // 本地记录的会话ID

	taskStartedSignal := make(chan struct{}, 1)
	var firstPacketReceived bool

	isCanceled := func() bool {
		ctxMutex.Lock()
		defer ctxMutex.Unlock()
		return sessionCtx.Err() != nil
	}

	receiveLoop := func(c *websocket.Conn) {
		defer wg.Done()
		for {
			if isCanceled() {
				return
			}

			msgType, msg, err := c.ReadMessage()
			if err != nil {
				return
			}

			if msgType == websocket.BinaryMessage {
				if !firstPacketReceived {
					tsFirstAudio = time.Now()
					firstPacketReceived = true
					totalLatency := tsFirstAudio.Sub(tsVadEnd)
					ttsLatency := tsFirstAudio.Sub(tsTtsStart)
					log.Printf("🚀 [性能] TTS 首包延迟: %v | ⚡ 全链路响应: %v", ttsLatency, totalLatency)
				}
				if !isCanceled() {
					audioPcmChan <- msg
				}
				continue
			}

			var resp map[string]interface{}
			if err := json.Unmarshal(msg, &resp); err != nil {
				continue
			}

			header, _ := resp["header"].(map[string]interface{})
			event := header["event"].(string)

			if event == "task-started" {
				select {
				case taskStartedSignal <- struct{}{}:
				default:
				}
			}
			if event == "task-failed" {
				log.Printf("❌ TTS 引擎报错: %v", header["error_message"])
				return
			}
			if event == "task-finished" {
				return
			}

			if payload, ok := resp["payload"].(map[string]interface{}); ok {
				if output, ok := payload["output"].(map[string]interface{}); ok {
					if audioBase64, ok := output["audio"].(string); ok {
						if pcm, err := base64.StdEncoding.DecodeString(audioBase64); err == nil {
							if !isCanceled() {
								audioPcmChan <- pcm
							}
						}
					}
				}
			}
		}
	}

	closeConn := func() {
		if conn != nil {
			time.Sleep(500 * time.Millisecond)
			if !isCanceled() {
				conn.WriteJSON(map[string]interface{}{
					"header":  map[string]interface{}{"task_id": currentTaskID, "action": "finish-task", "streaming": "duplex"},
					"payload": map[string]interface{}{"input": map[string]interface{}{}},
				})
				wg.Wait()
			}
			conn.Close()
			conn = nil
			currentTaskID = ""
			log.Printf("🏁 [性能] TTS 会话总耗时: %v", time.Since(tsTtsStart))
		}
	}

	for {
		firstChunk, ok := <-ttsManagerChan
		if !ok {
			return
		}

		// ★ 关键修复：检查 Session ID 是否变化
		sessionIDMutex.Lock()
		globalID := currentSessionID
		sessionIDMutex.Unlock()

		if localSessionID != globalID {
			// ID 变了，说明开始了新一轮对话，之前的连接必须作废
			if conn != nil {
				log.Println("🔄 检测到新会话，重置 TTS 连接...")
				conn.Close()
				conn = nil
			}
			localSessionID = globalID
		}

		if isCanceled() {
			if conn != nil {
				conn.Close()
				conn = nil
			}
			continue
		}

		var combinedText strings.Builder
		var hasEndSignal bool = false

		if firstChunk == "[[END]]" {
			hasEndSignal = true
		} else {
			combinedText.WriteString(firstChunk)
		drainLoop:
			for {
				select {
				case next := <-ttsManagerChan:
					if next == "[[END]]" {
						hasEndSignal = true
						break drainLoop
					}
					combinedText.WriteString(next)
				default:
					break drainLoop
				}
			}
		}

		textToSend := combinedText.String()

		if textToSend != "" {
			cleanTxt := strings.ReplaceAll(textToSend, "\"", " ")
			cleanTxt = strings.ReplaceAll(cleanTxt, "“", " ")
			cleanTxt = strings.ReplaceAll(cleanTxt, "”", " ")

			if strings.TrimSpace(cleanTxt) != "" {
				if isCanceled() {
					if conn != nil {
						conn.Close()
						conn = nil
					}
					continue
				}

				log.Printf("🔊 [TTS] 接收文本: %s", cleanTxt)

				if conn == nil {
					dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
					headers := http.Header{}
					headers.Add("Authorization", "Bearer "+DASH_API_KEY)
					c, _, err := dialer.Dial(TTS_WS_URL, headers)
					if err != nil {
						log.Printf("❌ TTS 连接失败: %v", err)
						continue
					}
					conn = c

					currentTaskID = uuid.New().String()
					firstPacketReceived = false
					tsTtsStart = time.Now()

					select {
					case <-taskStartedSignal:
					default:
					}

					wg.Add(1)
					go receiveLoop(conn)

					conn.WriteJSON(map[string]interface{}{
						"header": map[string]interface{}{"task_id": currentTaskID, "action": "run-task", "streaming": "duplex"},
						"payload": map[string]interface{}{
							"task_group": "audio", "task": "tts", "function": "SpeechSynthesizer",
							"model": "cosyvoice-clone-v1",
							"parameters": map[string]interface{}{
								"text_type":   "PlainText",
								"voice":       "longxiaochun",
								"format":      "pcm",
								"sample_rate": 22050,
								"volume":      50,
								"enable_ssml": false,
							},
							"input": map[string]interface{}{},
						},
					})

					select {
					case <-taskStartedSignal:
						time.Sleep(100 * time.Millisecond)
					case <-time.After(5 * time.Second):
						log.Println("⚠️ [TTS] 握手超时")
						conn.Close()
						conn = nil
						continue
					}
				}

				if isCanceled() {
					conn.Close()
					conn = nil
					continue
				}

				err := conn.WriteJSON(map[string]interface{}{
					"header":  map[string]interface{}{"task_id": currentTaskID, "action": "continue-task", "streaming": "duplex"},
					"payload": map[string]interface{}{"input": map[string]interface{}{"text": cleanTxt}},
				})
				time.Sleep(100 * time.Millisecond)
				if err != nil {
					conn.Close()
					conn = nil
				}
			}
		}

		if hasEndSignal {
			closeConn()
		}
	}
}

// ================= LLM 模块 =================
func callAgentStream(ctx context.Context, prompt string) {
	flushChannel(ttsManagerChan)
	tsLlmStart = time.Now()

	payload := map[string]interface{}{
		"model": "qwen-turbo",
		"input": map[string]interface{}{
			"messages": []map[string]string{
				{"role": "system", "content": "助手。自然口语。"},
				{"role": "user", "content": prompt},
			},
		},
		"parameters": map[string]interface{}{"result_format": "text", "incremental_output": true},
	}
	jsonBody, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", LLM_URL, bytes.NewReader(jsonBody))
	req.Header.Set("Authorization", "Bearer "+DASH_API_KEY)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := insecureClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var sentenceBuffer strings.Builder
	var isFirstToken = true

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			log.Println("🛑 LLM 生成已中断")
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

			if isFirstToken {
				tsLlmFirst = time.Now()
				log.Printf("⏱️ [性能] LLM 首字耗时: %v", tsLlmFirst.Sub(tsLlmStart))
				isFirstToken = false
			}

			log.Printf("📝 [LLM] 生成: %s", clean)

			sentenceBuffer.WriteString(clean)

			if strings.ContainsAny(clean, "。？！?!") || sentenceBuffer.Len() > 50 {
				select {
				case ttsManagerChan <- sentenceBuffer.String():
				case <-ctx.Done():
					return
				}
				sentenceBuffer.Reset()
			}
		}
	}
	log.Printf("⏱️ [性能] LLM 推理总耗时: %v", time.Since(tsLlmStart))

	if sentenceBuffer.Len() > 0 {
		select {
		case ttsManagerChan <- sentenceBuffer.String():
		case <-ctx.Done():
			return
		}
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case ttsManagerChan <- "[[END]]":
	case <-ctx.Done():
	}
}

// ================= 打断执行 =================
func performStop() {
	log.Println("🛑 触发打断")

	ctxMutex.Lock()
	if sessionCancel != nil {
		sessionCancel()
	}
	ctxMutex.Unlock()

	flushChannel(ttsManagerChan)
	flushChannel(audioPcmChan)

	exec.Command("killall", "-9", "aplay").Run()
	setState(STATE_LISTENING)
}

// ================= ASR & 控制 =================
func processASR(pcm []int16) {
	if float64(len(pcm))/16000.0 < 0.5 {
		return
	}

	tsVadEnd = time.Now()

	// ★ 更新会话 ID
	sessionIDMutex.Lock()
	currentSessionID = uuid.New().String()
	sessionIDMutex.Unlock()

	ctxMutex.Lock()
	if sessionCancel != nil {
		sessionCancel()
	}
	flushChannel(ttsManagerChan)
	sessionCtx, sessionCancel = context.WithCancel(context.Background())
	currentCtx := sessionCtx
	ctxMutex.Unlock()

	setState(STATE_THINKING)

	pcmBytes := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	text := callASRWebSocket(pcmBytes)

	tsAsrEnd = time.Now()
	log.Printf("⏱️ [性能] ASR 耗时: %v", tsAsrEnd.Sub(tsVadEnd))

	if text == "" {
		log.Println("⚠️ ASR 为空")
		setState(STATE_LISTENING)
		return
	}
	log.Printf("✅ 用户: [%s]", text)

	if containsAny(text, EXIT_WORDS) {
		log.Println("💀 退出")
		os.Exit(0)
	}
	if containsAny(text, INTERRUPT_WORDS) {
		performStop()
		return
	}

	setState(STATE_SPEAKING)
	go callAgentStream(currentCtx, text)
}

func setState(s AppState) { stateMutex.Lock(); currentState = s; stateMutex.Unlock() }
func containsAny(text string, k []string) bool {
	for _, w := range k {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// ================= 音频采集 (★ 8s 强制截断) =================
func audioLoop(aecProc *aec.Processor, vadEng *vado.VAD) {
	cmd := exec.Command("arecord", "-D", "hw:2,0", "-c", "10", "-r", "16000", "-f", "S16_LE", "-t", "raw", "--period-size=256", "--buffer-size=16384")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	log.Println("🎤 麦克风已开启 (10通道)...")

	readBuf := make([]byte, 256*10*2)
	vadBuf := make([]byte, 320*2)
	vadAccumulator := make([]int16, 0, 1024)
	var asrBuffer []int16

	silenceCount := 0
	speechCount := 0
	triggered := false

	for {
		if _, err := io.ReadFull(stdout, readBuf); err != nil {
			break
		}

		rawInt16 := make([]int16, 256*10)
		for i := 0; i < len(rawInt16); i++ {
			val := int16(binary.LittleEndian.Uint16(readBuf[i*2 : i*2+2]))
			rawInt16[i] = val
		}

		clean, _ := aecProc.Process(rawInt16)
		if clean == nil {
			continue
		}
		vadAccumulator = append(vadAccumulator, clean...)

		for len(vadAccumulator) >= 320 {
			frame := vadAccumulator[:320]
			vadAccumulator = vadAccumulator[320:]
			for i, v := range frame {
				binary.LittleEndian.PutUint16(vadBuf[i*2:], uint16(v))
			}
			active, _ := vadEng.Process(16000, vadBuf)

			if active {
				speechCount++
				silenceCount = 0
			} else {
				silenceCount++
				speechCount = 0
			}

			if speechCount > 10 && !triggered {
				log.Println("👂 [VAD] 检测到说话...")
				triggered = true
			}

			if triggered {
				asrBuffer = append(asrBuffer, frame...)
				isTooLong := len(asrBuffer) > 16000*8

				if silenceCount > 10 || isTooLong {
					if isTooLong {
						log.Println("⚠️ 录音超时(8s)，强制发送")
					}

					if len(asrBuffer) > 16000*0.3 {
						finalData := make([]int16, len(asrBuffer))
						copy(finalData, asrBuffer)
						go processASR(finalData)
					}
					asrBuffer = []int16{}
					triggered = false
					silenceCount = 0
				}
			} else {
				if len(asrBuffer) > 8000 {
					asrBuffer = asrBuffer[320:]
				}
				asrBuffer = append(asrBuffer, frame...)
			}
		}
	}
}

// ================= WebSocket =================
func callASRWebSocket(data []byte) string {
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+DASH_API_KEY)
	conn, _, err := dialer.Dial(WS_ASR_URL, headers)
	if err != nil {
		return ""
	}
	defer conn.Close()

	id := fmt.Sprintf("%032x", rand.Int63())
	// 1. Run
	conn.WriteJSON(map[string]interface{}{
		"header": map[string]interface{}{"task_id": id, "action": "run-task", "streaming": "duplex"},
		"payload": map[string]interface{}{
			"task_group": "audio", "task": "asr", "function": "recognition",
			"model": "paraformer-realtime-v2", "parameters": map[string]interface{}{"format": "pcm", "sample_rate": 16000},
			"input": map[string]interface{}{},
		},
	})
	// 2. Audio
	for i := 0; i < len(data); i += 3200 {
		end := i + 3200
		if end > len(data) {
			end = len(data)
		}
		conn.WriteMessage(websocket.BinaryMessage, data[i:end])
		time.Sleep(5 * time.Millisecond)
	}

	// 3. Finish
	conn.WriteJSON(map[string]interface{}{
		"header":  map[string]interface{}{"task_id": id, "action": "finish-task"},
		"payload": map[string]interface{}{"input": map[string]interface{}{}},
	})

	res := ""
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var r map[string]interface{}
		json.Unmarshal(msg, &r)
		h, _ := r["header"].(map[string]interface{})

		if h["event"] == "task-failed" {
			log.Printf("❌ ASR 报错: %v", h["error_message"])
			return ""
		}

		if h["event"] == "result-generated" {
			p, _ := r["payload"].(map[string]interface{})
			if o, ok := p["output"].(map[string]interface{}); ok {
				if s, ok := o["sentence"].(map[string]interface{}); ok {
					res = s["text"].(string)
				}
			}
		}
		if h["event"] == "task-finished" {
			break
		}
	}
	return res
}

func flushChannel[T any](c chan T) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}
