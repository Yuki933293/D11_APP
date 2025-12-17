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
	"net"
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

// ★★★ 音乐文件路径 (必须确保文件存在) ★★★
const MUSIC_FILE_PATH = "/userdata/song.wav"

var EXIT_WORDS = []string{
	"关闭", "再见", "退出", "关机", "拜拜", "退下",
}

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

	// ★★★ 音乐播放控制变量 ★★★
	musicCmd       *exec.Cmd
	musicMutex     sync.Mutex
	isMusicPlaying bool
)

func init() {
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	insecureClient = &http.Client{Transport: tr, Timeout: 0}
}

func generateSessionID() string {
	return fmt.Sprintf("session-%d-%d", time.Now().Unix(), rand.Intn(10000))
}

// ★★★ 简易音乐播放器 (调用 aplay) ★★★
func playMusic() {
	musicMutex.Lock()
	defer musicMutex.Unlock()

	// 1. 如果已经在播，先杀掉旧进程
	if musicCmd != nil && musicCmd.Process != nil {
		if musicCmd.ProcessState == nil || !musicCmd.ProcessState.Exited() {
			log.Println("🎵 切歌 (重启播放)")
			musicCmd.Process.Kill()
			musicCmd.Wait() // 等待彻底退出
		}
	}

	// 2. 启动新进程
	// 使用 -D default 确保走 dmix 混音，和 TTS 兼容
	musicCmd = exec.Command("aplay", "-D", "default", "-q", MUSIC_FILE_PATH)

	if err := musicCmd.Start(); err != nil {
		log.Printf("❌ 无法播放音乐: %v", err)
		isMusicPlaying = false
		return
	}

	isMusicPlaying = true
	log.Println("🎵 开始播放音乐...")

	// 3. 监听播放自然结束
	go func(cmd *exec.Cmd) {
		cmd.Wait()
		musicMutex.Lock()
		if musicCmd == cmd { // 确保不是被新指令挤掉的
			isMusicPlaying = false
			log.Println("🎵 音乐播放结束")
		}
		musicMutex.Unlock()
	}(musicCmd)
}

// ★★★ 停止音乐 ★★★
func stopMusic() {
	musicMutex.Lock()
	defer musicMutex.Unlock()

	if musicCmd != nil && musicCmd.Process != nil {
		// 检查进程是否还活着
		if musicCmd.ProcessState == nil || !musicCmd.ProcessState.Exited() {
			log.Println("🛑 停止背景音乐")
			musicCmd.Process.Kill()
		}
	}
	isMusicPlaying = false
}

// ★★★ 音乐意图识别 ★★★
func handleMusicIntent(text string) bool {
	// 播放指令
	if strings.Contains(text, "放歌") || strings.Contains(text, "播放音乐") || strings.Contains(text, "来首歌") || strings.Contains(text, "唱首歌") || strings.Contains(text, "放首歌") {
		log.Println("🎵 [指令] 播放音乐")
		speakQwenFlashStream("好的，来听听这首歌。")
		playMusic()
		return true
	}

	// 停止指令
	if strings.Contains(text, "别唱了") || strings.Contains(text, "关闭音乐") {
		log.Println("🎵 [指令] 停止音乐")
		stopMusic()
		speakQwenFlashStream("已停止。")
		return true
	}

	return false
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== RK3308 AI 助手 (V20.0 Aplay-Lite 混音版) ===")

	globalSessionID = generateSessionID()
	log.Printf("✨ 会话ID: %s", globalSessionID)

	aecProc := aec.NewProcessor()
	vadEng, err := vado.New()
	if err != nil {
		log.Fatalf("VAD Init 失败: %v", err)
	}
	vadEng.SetMode(3)

	stopPlayChan = make(chan struct{}, 1)

	go audioLoop(aecProc, vadEng)

	select {}
}

func logCost(stage string, start time.Time) {
	duration := time.Since(start)
	log.Printf("⏱️ [%s] 耗时: %d ms", stage, duration.Milliseconds())
}

func containsAny(text string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func performStop() {
	// 1. 停 TTS
	select {
	case stopPlayChan <- struct{}{}:
	default:
	}

	// 2. ★★★ 也要停音乐 ★★★
	stopMusic()

	stateMutex.Lock()
	currentState = STATE_LISTENING
	stateMutex.Unlock()
}

func performExit() {
	log.Println("💀 检测到退出指令，立即终止！")
	isExiting = true
	if stopPlayChan != nil {
		select {
		case stopPlayChan <- struct{}{}:
		default:
		}
	}

	stopMusic()

	speakQwenFlashStream("再见")
	log.Println("👋 进程自杀")
	os.Exit(0)
}

func audioLoop(aecProc *aec.Processor, vadEng *vado.VAD) {
	// 录音依然使用 hw:2,0 (VAD/Mic 设备)，这个不需要改 dmix
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
			time.Sleep(100 * time.Millisecond)
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

			// VAD 触发阈值 (300ms)
			if vadSpeechCounter > 15 {
				if !isSpeechTriggered {
					if curr == STATE_SPEAKING || curr == STATE_THINKING {
						log.Println("🛡️ [VAD] 监听到疑似打断...")
					} else {
						log.Println("👂 [VAD] 开始录音...")
					}

					// ★★★ 核心避让逻辑 ★★★
					// 人一开口，音乐就停。防止音乐声被录进去
					if isMusicPlaying {
						log.Println("🤫 监听到人声，暂停背景音乐")
						stopMusic()
					}

					isSpeechTriggered = true
				}
			}

			if isSpeechTriggered {
				asrBuffer = append(asrBuffer, currentFrame...)

				if vadSilenceCounter > 18 && len(asrBuffer) > 16000*0.3 {

					vadWaitDuration := time.Since(silenceStartTime)

					bufferCopy := make([]int16, len(asrBuffer))
					copy(bufferCopy, asrBuffer)

					asrBuffer = []int16{}
					isSpeechTriggered = false
					vadSilenceCounter = 0

					if curr == STATE_LISTENING {
						log.Printf("⚡ [VAD] 录音结束 (静音: %d ms)，正常处理", vadWaitDuration.Milliseconds())
						go processASR(bufferCopy)
					} else {
						log.Printf("⚡ [VAD] 录音结束，校验打断词...")
						go processInterruptionCheck(bufferCopy)
					}
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

func setState(s AppState) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	currentState = s
}

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

	if containsAny(text, EXIT_WORDS) {
		performExit()
		return
	}

	if containsAny(text, INTERRUPT_WORDS) {
		performStop()
	}
}

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

	// 1. 退出
	if containsAny(text, EXIT_WORDS) {
		performExit()
		return
	}

	// 2. 暂停
	if containsAny(text, INTERRUPT_WORDS) {
		performStop()
		speakQwenFlashStream("好的")
		return
	}

	// 3. ★★★ 检查音乐意图 (优先处理) ★★★
	if handleMusicIntent(text) {
		setState(STATE_LISTENING)
		return
	}

	// 4. 重置
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

	stateMutex.Lock()
	if currentState != STATE_THINKING || isExiting {
		stateMutex.Unlock()
		log.Println("⚠️ [Process] 状态已变更，放弃播放")
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

func speakQwenFlashStream(text string) {
	select {
	case <-stopPlayChan:
		log.Println("🧹 [TTS] 清理残留信号")
	default:
	}

	payload := map[string]interface{}{
		"model":      "qwen3-tts-flash-2025-09-18",
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

	// ★★★ 核心修改：TTS 播放也要走 default 设备 ★★★
	// 这样 TTS 就会通过 dmix 与音乐混音，而不是报错
	playCmd := exec.Command("aplay", "-D", "default", "-q", "-t", "raw", "-r", "24000", "-f", "S16_LE", "-c", "1")
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
		select {
		case <-stopPlayChan:
			log.Println("🛑 [TTS] 收到停止信号")
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

// 保持原样
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

// 保持原样
func callAgent(prompt string) string {
	url := "https://dashscope.aliyuncs.com/api/v1/apps/" + APP_ID + "/completion"

	payload := map[string]interface{}{
		"input": map[string]string{
			"prompt":     prompt,
			"session_id": globalSessionID,
		},
		"parameters": map[string]interface{}{
			"enable_thinking": false,
			"enable_search":   false,
		},
		"debug": false,
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
