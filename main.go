package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

const MUSIC_DIR = "/userdata/music"

var EXIT_WORDS = []string{"关闭系统", "关机", "退出程序", "再见", "退下", "拜拜", "结束吧", "结束程序", "停止运行", "关闭助手", "关闭"}
var INTERRUPT_WORDS = []string{"闭嘴", "停止", "安静", "别说了", "暂停", "打断", "别唱了", "等一下"}

type AppState int

const (
	STATE_LISTENING AppState = iota
	STATE_THINKING
	STATE_SPEAKING
)

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

	sessionCtx    context.Context
	sessionCancel context.CancelFunc
	ctxMutex      sync.Mutex

	currentSessionID string
	sessionIDMutex   sync.Mutex

	insecureClient *http.Client

	ttsManagerChan chan string
	audioPcmChan   chan []byte
	playerStdin    io.WriteCloser
	playerCmd      *exec.Cmd
	playerMutex    sync.Mutex

	emojiRegex *regexp.Regexp

	musicMgr *MusicManager
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
	log.Println("=== RK3308 AI 助手 (V160.1 首句抢跑优化版) ===")

	exec.Command("amixer", "-c", "2", "sset", "Master", "100%", "unmute").Run()
	exec.Command("amixer", "-c", "2", "sset", "Playback", "100%", "unmute").Run()
	exec.Command("amixer", "-c", "2", "sset", "Capture", "100%", "unmute").Run()

	ttsManagerChan = make(chan string, 500)
	audioPcmChan = make(chan []byte, 4000)

	sessionCtx, sessionCancel = context.WithCancel(context.Background())
	currentSessionID = uuid.New().String()

	musicMgr = NewMusicManager()

	go audioPlayer()
	go ttsManagerLoop()

	aecProc := aec.NewProcessor()
	vadEng, err := vado.New()
	if err != nil {
		log.Fatal("❌ VAD 初始化失败:", err)
	}
	vadEng.SetMode(3)

	go audioLoop(aecProc, vadEng)

	select {}
}

func cleanText(text string) string { return strings.TrimSpace(emojiRegex.ReplaceAllString(text, "")) }

// ================= 🎵 音乐管理器 =================
type MusicManager struct {
	isPlaying     bool
	mu            sync.Mutex
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stopChan      chan struct{}
	targetVolume  float64
	currentVolume float64
	volMutex      sync.Mutex
}

func NewMusicManager() *MusicManager {
	return &MusicManager{targetVolume: 1.0, currentVolume: 1.0}
}

func (m *MusicManager) IsPlaying() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isPlaying
}

func (m *MusicManager) setTargetVolume(vol float64) {
	m.volMutex.Lock()
	m.targetVolume = vol
	m.volMutex.Unlock()
}

func (m *MusicManager) Duck() {
	if m.IsPlaying() {
		m.setTargetVolume(0.2)
	}
}
func (m *MusicManager) Unduck() {
	if m.IsPlaying() {
		m.setTargetVolume(1.0)
	}
}

func (m *MusicManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isPlaying {
		log.Println("🛑 [MUSIC] 停止播放")
		select {
		case m.stopChan <- struct{}{}:
		default:
		}
		if m.stdin != nil {
			m.stdin.Close()
		}
		if m.cmd != nil && m.cmd.Process != nil {
			m.cmd.Process.Kill()
			m.cmd.Wait()
		}
		m.isPlaying = false
	}
}

func (m *MusicManager) PlayFile(path string) {
	m.Stop()
	time.Sleep(200 * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	file, err := os.Open(path)
	if err != nil {
		return
	}

	cmd := exec.Command("aplay", "-D", "default", "-q", "-t", "raw", "-r", "16000", "-c", "1", "-f", "S16_LE", "-B", "20000")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		file.Close()
		return
	}
	if err := cmd.Start(); err != nil {
		file.Close()
		return
	}

	m.cmd = cmd
	m.stdin = stdin
	m.isPlaying = true
	m.stopChan = make(chan struct{}, 1)
	m.targetVolume = 1.0
	m.currentVolume = 1.0

	log.Printf("🎵 [MUSIC] 开始播放: %s", filepath.Base(path))

	go func(f *os.File, pipe io.WriteCloser, myCmd *exec.Cmd, stopCh chan struct{}) {
		defer f.Close()
		defer pipe.Close()
		f.Seek(44, 0)
		buf := make([]byte, 4096)
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			n, err := f.Read(buf)
			if err != nil {
				break
			}

			m.volMutex.Lock()
			target := m.targetVolume
			m.volMutex.Unlock()

			if m.currentVolume < target {
				m.currentVolume += 0.05
				if m.currentVolume > target {
					m.currentVolume = target
				}
			} else if m.currentVolume > target {
				m.currentVolume -= 0.05
				if m.currentVolume < target {
					m.currentVolume = target
				}
			}

			count := n / 2
			for i := 0; i < count; i++ {
				sample := int16(binary.LittleEndian.Uint16(buf[i*2 : i*2+2]))
				scaled := int16(float64(sample) * m.currentVolume)
				binary.LittleEndian.PutUint16(buf[i*2:], uint16(scaled))
			}
			if _, err := pipe.Write(buf[:n]); err != nil {
				return
			}
		}
		m.mu.Lock()
		if m.isPlaying && m.cmd == myCmd {
			m.isPlaying = false
			log.Println("🎵 [MUSIC] 自然结束")
			go myCmd.Wait()
		}
		m.mu.Unlock()
	}(file, stdin, cmd, m.stopChan)
}

func (m *MusicManager) SearchAndPlay(query string) bool {
	files, err := ioutil.ReadDir(MUSIC_DIR)
	if err != nil {
		return false
	}
	var candidates []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(strings.ToLower(f.Name()), ".wav") {
			candidates = append(candidates, filepath.Join(MUSIC_DIR, f.Name()))
		}
	}
	if len(candidates) == 0 {
		return false
	}
	target := ""
	if query == "RANDOM" {
		target = candidates[rand.Intn(len(candidates))]
	} else {
		q := strings.ToLower(query)
		for _, path := range candidates {
			if strings.Contains(strings.ToLower(filepath.Base(path)), q) {
				target = path
				break
			}
		}
		if target == "" {
			return false
		}
	}
	m.PlayFile(target)
	return true
}

// ================= TTS 播放器 =================
func audioPlayer() {
	var cmd *exec.Cmd
	var stdin io.WriteCloser

	startPlayer := func() (*exec.Cmd, io.WriteCloser) {
		c := exec.Command("aplay", "-D", "default", "-t", "raw", "-r", "22050", "-f", "S16_LE", "-c", "1", "-B", "100000")
		s, err := c.StdinPipe()
		if err != nil {
			return nil, nil
		}
		if err := c.Start(); err != nil {
			return nil, nil
		}
		playerMutex.Lock()
		playerCmd = c
		playerStdin = s
		playerMutex.Unlock()
		return c, s
	}

	for pcmData := range audioPcmChan {
		if len(pcmData) == 0 {
			if stdin != nil {
				stdin.Close()
			}
			if cmd != nil {
				cmd.Wait()
			}
			log.Println("✅ [Audio] 物理播放结束，重置为监听")
			setState(STATE_LISTENING)
			cmd = nil
			stdin = nil
			continue
		}

		if stdin == nil {
			cmd, stdin = startPlayer()
		}
		if stdin != nil {
			if _, err := stdin.Write(pcmData); err != nil {
				if cmd != nil && cmd.Process != nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
				cmd, stdin = startPlayer()
				if stdin != nil {
					stdin.Write(pcmData)
				}
			}
		}
	}
}

// ================= TTS 管理器 =================
func ttsManagerLoop() {
	var conn *websocket.Conn
	var wg sync.WaitGroup
	var currentTaskID string
	var localSessionID string
	taskStartedSignal := make(chan struct{}, 1)
	var firstPacketReceived bool

	isCanceled := func() bool {
		ctxMutex.Lock()
		defer ctxMutex.Unlock()
		return sessionCtx.Err() != nil
	}

	receiveLoop := func(c *websocket.Conn) {
		defer wg.Done()
		defer func() {
			if !isCanceled() {
				audioPcmChan <- []byte{}
			}
		}()
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
					log.Printf("🚀 [性能] TTS 首包: %v", tsFirstAudio.Sub(tsTtsStart))
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
			if header["event"] == "task-started" {
				select {
				case taskStartedSignal <- struct{}{}:
				default:
				}
			}
			if header["event"] == "task-finished" || header["event"] == "task-failed" {
				return
			}
		}
	}

	for {
		firstChunk, ok := <-ttsManagerChan
		if !ok {
			return
		}

		sessionIDMutex.Lock()
		globalID := currentSessionID
		sessionIDMutex.Unlock()
		if localSessionID != globalID {
			if conn != nil {
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
		if firstChunk != "[[END]]" {
			combinedText.WriteString(firstChunk)
		drainLoop:
			for {
				select {
				case next := <-ttsManagerChan:
					if next == "[[END]]" {
						break drainLoop
					}
					combinedText.WriteString(next)
				default:
					break drainLoop
				}
			}
		}

		textToSend := combinedText.String()
		if textToSend != "" && strings.TrimSpace(textToSend) != "" {
			if isCanceled() {
				if conn != nil {
					conn.Close()
					conn = nil
				}
				continue
			}
			if conn == nil {
				dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
				headers := http.Header{}
				headers.Add("Authorization", "Bearer "+DASH_API_KEY)
				c, _, err := dialer.Dial(TTS_WS_URL, headers)
				if err != nil {
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
						"model":      "cosyvoice-clone-v1",
						"parameters": map[string]interface{}{"text_type": "PlainText", "voice": "longxiaochun", "format": "pcm", "sample_rate": 22050, "volume": 50, "enable_ssml": false},
						"input":      map[string]interface{}{},
					},
				})
				select {
				case <-taskStartedSignal:
					time.Sleep(100 * time.Millisecond)
				case <-time.After(5 * time.Second):
					conn.Close()
					conn = nil
					continue
				}
			}
			conn.WriteJSON(map[string]interface{}{
				"header":  map[string]interface{}{"task_id": currentTaskID, "action": "continue-task", "streaming": "duplex"},
				"payload": map[string]interface{}{"input": map[string]interface{}{"text": textToSend}},
			})
			time.Sleep(100 * time.Millisecond)
		}

		if conn != nil && (firstChunk == "[[END]]" || combinedText.Len() > 0) {
			if firstChunk == "[[END]]" {
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
			}
		}
	}
}

// ================= LLM (增加首句抢跑逻辑) =================
// ================= LLM (V164.2 全程流式增量提交版) =================
func callAgentStream(ctx context.Context, prompt string) {
	flushChannel(ttsManagerChan)
	tsLlmStart = time.Now()

	// 1. 定义提示词和请求体
	systemPrompt := "你是智能助手。仅在用户【明确要求播放音乐】（如“放首歌”、“听周杰伦”）时，才在回复末尾添加 [PLAY: 歌名]（随机播放用 [PLAY: RANDOM]）。" +
		"如果用户要求停止，加上 [STOP]。" +
		"回答天气、新闻、闲聊等普通问题时，【严禁】添加任何播放指令。"
	payload := map[string]interface{}{
		"model": "qwen-turbo",
		"input": map[string]interface{}{
			"messages": []map[string]string{
				{"role": "system", "content": systemPrompt},
				{"role": "user", "content": prompt},
			},
		},
		"parameters": map[string]interface{}{"result_format": "text", "incremental_output": true},
	}

	jsonBody, _ := json.Marshal(payload)

	// 2. 建立 HTTP 请求
	req, _ := http.NewRequestWithContext(ctx, "POST", LLM_URL, bytes.NewReader(jsonBody))
	req.Header.Set("Authorization", "Bearer "+DASH_API_KEY)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := insecureClient.Do(req)
	if err != nil {
		musicMgr.Unduck()
		setState(STATE_LISTENING)
		return
	}
	defer resp.Body.Close()

	// 3. 开始流式处理 LLM 吐出的内容
	scanner := bufio.NewScanner(resp.Body)
	var fullTextBuilder strings.Builder
	var chunkBuffer strings.Builder // 持续积攒文字的缓冲区
	var firstChunkSent = false

	fmt.Print("📝 [LLM 推理]: ")

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

			// ★ 增量分段提交逻辑：
			// 规则：遇到结尾标点，或者缓冲区内的文字超过 60 字节 (约20个汉字)
			currentText := chunkBuffer.String()
			shouldSend := false

			// 针对首句做特殊加速处理
			if !firstChunkSent {
				if strings.ContainsAny(clean, "，。！？,.!?\n") || chunkBuffer.Len() > 30 {
					shouldSend = true
					firstChunkSent = true
				}
			} else {
				// 后续句子遇到标点就发，保证流式顺畅
				if strings.ContainsAny(clean, "，。！？,.!?\n") || chunkBuffer.Len() > 80 {
					shouldSend = true
				}
			}

			if shouldSend {
				// 提取指令（过滤掉 [PLAY:] 等内容），避免指令被 TTS 读出来
				textToTTS := regexp.MustCompile(`\[.*?\]`).ReplaceAllString(currentText, "")
				textToTTS = strings.TrimSpace(textToTTS)

				if textToTTS != "" {
					ttsManagerChan <- textToTTS
					chunkBuffer.Reset() // 提交后清空，准备接下一段
				}
			}
		}
	}
	fmt.Println()
	log.Printf("⏱️ [性能] LLM 总耗时: %v", time.Since(tsLlmStart))

	// 4. 扫尾逻辑：发送缓冲区中最后剩余的文字
	remainText := strings.TrimSpace(regexp.MustCompile(`\[.*?\]`).ReplaceAllString(chunkBuffer.String(), ""))
	if remainText != "" {
		ttsManagerChan <- remainText
	}

	// 5. 告知 TTS 这一轮任务结束
	ttsManagerChan <- "[[END]]"

	// 6. 处理指令（控制音乐播放等）
	fullText := fullTextBuilder.String()
	if strings.Contains(fullText, "[STOP]") {
		musicMgr.Stop()
	}
	if strings.Contains(fullText, "[PLAY:") {
		re := regexp.MustCompile(`\[PLAY:\s*(.*?)\]`)
		if matches := re.FindStringSubmatch(fullText); len(matches) > 1 {
			musicMgr.SearchAndPlay(strings.TrimSpace(matches[1]))
		}
	}
}

// ================= 打断 =================
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
	musicMgr.Stop()
	setState(STATE_LISTENING)
}

// ================= ASR =================
func processASR(pcm []int16) {
	if float64(len(pcm))/16000.0 < 0.5 {
		return
	}

	tsVadEnd = time.Now()

	sessionIDMutex.Lock()
	currentSessionID = uuid.New().String()
	sessionIDMutex.Unlock()

	pcmBytes := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	text := callASRWebSocket(pcmBytes)
	if text == "" {
		musicMgr.Unduck()
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

	if getCurrentState() == STATE_SPEAKING {
		log.Printf("🙉 [忽略]: 正在播报中，忽略闲聊 -> [%s]", text)
		musicMgr.Unduck()
		return
	}

	ctxMutex.Lock()
	if sessionCancel != nil {
		sessionCancel()
	}
	flushChannel(ttsManagerChan)
	sessionCtx, sessionCancel = context.WithCancel(context.Background())
	currentCtx := sessionCtx
	ctxMutex.Unlock()

	setState(STATE_SPEAKING)
	go callAgentStream(currentCtx, text)
}

func setState(s AppState)       { stateMutex.Lock(); currentState = s; stateMutex.Unlock() }
func getCurrentState() AppState { stateMutex.Lock(); defer stateMutex.Unlock(); return currentState }
func containsAny(text string, k []string) bool {
	for _, w := range k {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
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
				triggered = true
				musicMgr.Duck()
			}

			if triggered {
				asrBuffer = append(asrBuffer, frame...)
				isTooLong := len(asrBuffer) > 16000*8
				if silenceCount > 10 || isTooLong {
					if len(asrBuffer) > 16000*0.3 {
						finalData := make([]int16, len(asrBuffer))
						copy(finalData, asrBuffer)
						go processASR(finalData)
					} else {
						musicMgr.Unduck()
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
	conn.WriteJSON(map[string]interface{}{
		"header":  map[string]interface{}{"task_id": id, "action": "run-task", "streaming": "duplex"},
		"payload": map[string]interface{}{"task_group": "audio", "task": "asr", "function": "recognition", "model": "paraformer-realtime-v2", "parameters": map[string]interface{}{"format": "pcm", "sample_rate": 16000}, "input": map[string]interface{}{}},
	})
	for i := 0; i < len(data); i += 3200 {
		end := i + 3200
		if end > len(data) {
			end = len(data)
		}
		conn.WriteMessage(websocket.BinaryMessage, data[i:end])
		time.Sleep(5 * time.Millisecond)
	}
	conn.WriteJSON(map[string]interface{}{"header": map[string]interface{}{"task_id": id, "action": "finish-task"}, "payload": map[string]interface{}{"input": map[string]interface{}{}}})
	res := ""
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var r map[string]interface{}
		json.Unmarshal(msg, &r)
		h, _ := r["header"].(map[string]interface{})
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
