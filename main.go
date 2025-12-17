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
	"io/fs"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// 音乐文件存放目录
const MUSIC_DIR = "/userdata/music"

// 优先级 1: 退出词 (杀进程)
var EXIT_WORDS = []string{
	"关闭系统", "关机", "退出程序", "再见", "退下", "拜拜", "结束对话", "结束程序", "关闭",
}

// 优先级 2: 强行停止词 (停止 TTS 和 音乐)
var INTERRUPT_WORDS = []string{
	"闭嘴", "停止", "安静", "别说了", "别唱了", "关掉音乐", "停止播放", "等一下", "暂停",
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

	// --- 音乐控制 (内存混音) ---
	musicCmd       *exec.Cmd
	musicStdin     io.WriteCloser
	musicMutex     sync.Mutex
	isMusicPlaying bool
	targetVolume   float64 = 1.0 // 目标音量 (1.0 = 100%, 0.2 = 20%)
	currentVolume  float64 = 1.0 // 当前平滑音量
	volMutex       sync.Mutex
	stopMusicChan  chan struct{}

	// --- TTS 控制 (防重叠) ---
	ttsCmd   *exec.Cmd
	ttsMutex sync.Mutex
)

func init() {
	// 配置 HTTP Client (跳过 SSL 验证以加速)
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

	rand.Seed(time.Now().UnixNano())
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== RK3308 AI 助手 (V24.0 最终版) ===")

	globalSessionID = generateSessionID()
	log.Printf("✨ 会话ID: %s", globalSessionID)

	// 1. 初始化回声消除 (AEC)
	aecProc := aec.NewProcessor()

	// 2. 初始化 VAD
	vadEng, err := vado.New()
	if err != nil {
		log.Fatalf("VAD Init 失败: %v", err)
	}
	vadEng.SetMode(3) // 激进模式

	stopPlayChan = make(chan struct{}, 1)

	// 3. 启动音频采集循环
	go audioLoop(aecProc, vadEng)

	// 阻塞主进程
	select {}
}

func generateSessionID() string {
	return fmt.Sprintf("session-%d-%d", time.Now().Unix(), rand.Intn(10000))
}

// ================= 1. 音乐播放与软闪避 (内存混音) =================

func setTargetVolume(vol float64) {
	volMutex.Lock()
	targetVolume = vol
	volMutex.Unlock()
}

func duckMusic() {
	musicMutex.Lock()
	playing := isMusicPlaying
	musicMutex.Unlock()

	if playing {
		// Log 太多会刷屏，这里可以注释掉，或者保留用于调试
		// log.Println("📉 [闪避] 降低音乐音量")
		setTargetVolume(0.2) // 降到 20%
	}
}

func unduckMusic() {
	musicMutex.Lock()
	playing := isMusicPlaying
	musicMutex.Unlock()

	if playing {
		log.Println("📈 [恢复] 恢复音乐音量")
		setTargetVolume(1.0) // 恢复 100%
	}
}

func playMusicFile(path string) bool {
	musicMutex.Lock()
	defer musicMutex.Unlock()

	// ================= 1. 严密的清理逻辑 =================
	if isMusicPlaying {
		// 发送停止信号 (通知旧的 Goroutine 停止写入)
		select {
		case stopMusicChan <- struct{}{}:
		default:
		}

		// 主动关闭管道 (这是最快让 Goroutine 退出的方法)
		if musicStdin != nil {
			musicStdin.Close()
		}

		// 强制杀掉旧进程
		if musicCmd != nil && musicCmd.Process != nil {
			musicCmd.Process.Kill()
			// 必须等待僵尸进程彻底回收
			_ = musicCmd.Wait()
		}

		// 稍微延时，确保 ALSA 缓冲区排空
		time.Sleep(100 * time.Millisecond)
	}
	// ====================================================

	// 打开新文件
	file, err := os.Open(path)
	if err != nil {
		log.Printf("❌ 无法打开文件: %v", err)
		return false
	}

	// 启动 aplay
	// 注意：使用 default 设备
	cmd := exec.Command("aplay", "-D", "default", "-q", "-t", "raw", "-r", "16000", "-c", "1", "-f", "S16_LE")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		file.Close()
		return false
	}
	if err := cmd.Start(); err != nil {
		file.Close()
		return false
	}

	// 更新全局状态
	musicCmd = cmd                         // 更新全局指针
	musicStdin = stdin                     // 更新全局管道
	isMusicPlaying = true                  // 标记正在播放
	stopMusicChan = make(chan struct{}, 1) //以此建立新的信号通道

	// 重置音量
	setTargetVolume(1.0)
	currentVolume = 1.0

	log.Printf("🎵 正在播放: %s", filepath.Base(path))

	// ================= 2. 聪明的 Goroutine =================
	// ★ 关键修改：把 cmd 作为参数传进去，让协程认准自己的“主人”
	go func(f *os.File, pipe io.WriteCloser, myCmd *exec.Cmd) {
		defer f.Close()
		defer pipe.Close()

		f.Seek(44, 0) // 跳过 WAV 头

		buf := make([]byte, 1024)
		int16Buf := make([]int16, 512)

		for {
			// 检查停止信号
			select {
			case <-stopMusicChan:
				// ★ 核心点：如果是被信号打断的，直接 return，不要改 isMusicPlaying！
				return
			default:
			}

			n, err := f.Read(buf)
			if err != nil {
				// EOF (自然播完) 或者文件错误
				break
			}

			// --- 音量处理 (保持不变) ---
			volMutex.Lock()
			target := targetVolume
			volMutex.Unlock()

			step := 0.05
			if currentVolume < target {
				currentVolume += step
				if currentVolume > target {
					currentVolume = target
				}
			} else if currentVolume > target {
				currentVolume -= step
				if currentVolume < target {
					currentVolume = target
				}
			}

			count := n / 2
			for i := 0; i < count; i++ {
				sample := int16(binary.LittleEndian.Uint16(buf[i*2 : i*2+2]))
				scaled := int16(float64(sample) * currentVolume)
				int16Buf[i] = scaled
			}

			for i := 0; i < count; i++ {
				binary.LittleEndian.PutUint16(buf[i*2:], uint16(int16Buf[i]))
			}

			_, wErr := pipe.Write(buf[:n])
			if wErr != nil {
				// 管道断裂（通常是因为外部 close 了管道），视为被打断
				return
			}
		}

		// ================= 3. 安全的状态更新 =================
		// 只有代码走到这里，才说明是“自然播完”的

		musicMutex.Lock()
		defer musicMutex.Unlock()

		// ★ 关键判断：只有当全局的 musicCmd 还是我自己时，才把 playing 设为 false
		// 如果主线程已经切歌了，musicCmd 会指向新歌，我就不能乱改状态了
		if isMusicPlaying && musicCmd == myCmd {
			isMusicPlaying = false
			log.Println("🎵 播放自然结束")

			// 顺便回收一下进程资源
			go func() {
				myCmd.Wait()
			}()
		}

	}(file, stdin, cmd) // 将 cmd 传入闭包

	return true
}

func stopMusic() {
	musicMutex.Lock()
	defer musicMutex.Unlock()

	if isMusicPlaying {
		log.Println("🛑 停止背景音乐")

		// 1. 发信号 (通知 Goroutine 赶紧退，别改状态)
		select {
		case stopMusicChan <- struct{}{}:
		default:
		}

		// 2. 关管道 (物理切断)
		if musicStdin != nil {
			musicStdin.Close()
		}

		// 3. 杀进程 (物理超度)
		if musicCmd != nil && musicCmd.Process != nil {
			musicCmd.Process.Kill()
			_ = musicCmd.Wait() // 必须等它死透
		}

		// 4. 更新状态
		isMusicPlaying = false
		musicCmd = nil
		musicStdin = nil
	}
}

// 搜索并播放 (支持空格分词搜索 + 随机)
func searchAndPlay(keyword string) (bool, string) {
	var candidates []string
	log.Printf("🔍 正在搜索: [%s]", keyword)

	// 预处理关键词：按空格拆分
	subKeywords := strings.Fields(keyword)

	filepath.WalkDir(MUSIC_DIR, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".wav") {
			return nil
		}

		// 模式 A: 随机播放 (keyword为空)
		if keyword == "" {
			candidates = append(candidates, path)
			return nil
		}

		// 模式 B: 精准/模糊搜索
		filenameLower := strings.ToLower(d.Name())
		allMatch := true
		for _, k := range subKeywords {
			if !strings.Contains(filenameLower, strings.ToLower(k)) {
				allMatch = false
				break
			}
		}
		if allMatch {
			candidates = append(candidates, path)
		}
		return nil
	})

	if len(candidates) == 0 {
		return false, ""
	}

	// 随机挑一首
	target := candidates[rand.Intn(len(candidates))]
	success := playMusicFile(target)

	baseName := filepath.Base(target)
	songName := strings.TrimSuffix(baseName, filepath.Ext(baseName))

	return success, songName
}

// ================= 2. TTS 控制 (解决双重说话) =================

func stopTTS() {
	ttsMutex.Lock()
	defer ttsMutex.Unlock()

	if ttsCmd != nil && ttsCmd.Process != nil {
		if ttsCmd.ProcessState == nil || !ttsCmd.ProcessState.Exited() {
			// log.Println("🔇 [TTS] 强制扼杀旧的说话进程") // 调试时可开
			ttsCmd.Process.Kill()
			ttsCmd.Wait()
		}
	}
	ttsCmd = nil
}

func speakQwenFlashStream(text string) {
	// ★ 1. 杀掉上一次还没说完的话
	stopTTS()

	select {
	case <-stopPlayChan:
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
		return
	}
	defer resp.Body.Close()

	// 启动 aplay
	cmd := exec.Command("aplay", "-D", "default", "-q", "-t", "raw", "-r", "24000", "-f", "S16_LE", "-c", "1")
	playStdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}

	// ★ 2. 登记全局变量
	ttsMutex.Lock()
	ttsCmd = cmd
	ttsMutex.Unlock()

	// 异步清理
	go func(c *exec.Cmd) {
		c.Wait()
		ttsMutex.Lock()
		if ttsCmd == c {
			ttsCmd = nil
		}
		ttsMutex.Unlock()
	}(cmd)

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		// 检查打断
		select {
		case <-stopPlayChan:
			cmd.Process.Kill()
			return
		default:
		}
		// 检查是否被外部 Kill
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return
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
			if err == nil {
				playStdin.Write(audioBytes)
			}
		}
	}
	playStdin.Close()
}

// ================= 3. 核心逻辑 (ASR & 意图路由) =================

func performStop() {
	// 发送软信号
	select {
	case stopPlayChan <- struct{}{}:
	default:
	}
	// ★ 强力停止 TTS
	stopTTS()
	// 停止音乐
	stopMusic()

	stateMutex.Lock()
	currentState = STATE_LISTENING
	stateMutex.Unlock()
}

func performExit() {
	log.Println("💀 收到退出指令")
	isExiting = true
	select {
	case stopPlayChan <- struct{}{}:
	default:
	}
	stopTTS()
	stopMusic()
	speakQwenFlashStream("再见")
	os.Exit(0)
}

func processASR(pcmDataInt16 []int16) {
	pipelineStart := time.Now()
	setState(STATE_THINKING)

	pcmBytes := make([]byte, len(pcmDataInt16)*2)
	for i, v := range pcmDataInt16 {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	text := callASRWebSocket(pcmBytes)
	if text == "" {
		unduckMusic()
		setState(STATE_LISTENING)
		return
	}
	log.Printf("✅ 用户说: [%s]", text)

	// --- 1. 优先处理硬指令 ---
	if containsAny(text, EXIT_WORDS) {
		performExit()
		return
	}
	if containsAny(text, INTERRUPT_WORDS) {
		log.Println("🛑 触发打断指令")
		performStop()
		speakQwenFlashStream("我在")
		return
	}

	// --- 2. 智能决策 ---
	systemPrompt := `你是一个智能音箱助手。
1. 用户想听歌/随机播放 (如"放首歌","听周杰伦","来首稻香") -> {"action":"play","keyword":"搜索词","reply":"好的..."}。如果未指定歌名keyword设为空。
2. 用户想聊天 -> {"action":"chat","reply":"回复内容"}。
3. 只返回JSON，不要Markdown。`

	fullPrompt := systemPrompt + "\n用户输入：" + text

	jsonResponse := callAgent(fullPrompt)
	logCost("LLM决策", time.Since(pipelineStart))

	jsonResponse = strings.Trim(jsonResponse, "```json")
	jsonResponse = strings.Trim(jsonResponse, "```")

	var intent struct {
		Action  string `json:"action"`
		Keyword string `json:"keyword"`
		Reply   string `json:"reply"`
	}

	err := json.Unmarshal([]byte(jsonResponse), &intent)

	stateMutex.Lock()
	currentState = STATE_SPEAKING
	stateMutex.Unlock()

	musicMutex.Lock()
	playing := isMusicPlaying
	musicMutex.Unlock()

	if err != nil {
		// 解析失败
		if playing {
			log.Println("🤫 [音乐模式] 解析失败，保持安静")
			unduckMusic()
		} else {
			log.Println("⚠️ LLM JSON解析失败，回落为普通回复")
			speakQwenFlashStream(jsonResponse)
		}
	} else {
		if intent.Action == "play" {
			// 点歌：任何时候都响应
			speakQwenFlashStream(intent.Reply)
			success, songName := searchAndPlay(intent.Keyword)
			if !success {
				speakQwenFlashStream("抱歉，本地曲库没有找到这首歌。")
			} else {
				log.Printf("🎵 即将播放: %s", songName)
			}
		} else {
			// 闲聊：只有没放歌时才响应 (高冷模式)
			if playing {
				log.Printf("🤫 [音乐模式] 识别为闲聊(%s)，主动忽略", intent.Reply)
				unduckMusic()
			} else {
				speakQwenFlashStream(intent.Reply)
				unduckMusic()
			}
		}
	}

	stateMutex.Lock()
	if currentState == STATE_SPEAKING && !isExiting {
		currentState = STATE_LISTENING
	}
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

			if isSpeech {
				vadSpeechCounter++
				vadSilenceCounter = 0
			} else {
				vadSilenceCounter++
				vadSpeechCounter = 0
			}

			// VAD 触发 (Ducking Trigger)
			if vadSpeechCounter > 15 {
				if !isSpeechTriggered {
					log.Println("👂 [VAD] 检测到说话...")
					duckMusic() // 立即降低音量
					isSpeechTriggered = true
				}
			}

			if isSpeechTriggered {
				asrBuffer = append(asrBuffer, currentFrame...)
				if vadSilenceCounter > 18 && len(asrBuffer) > 16000*0.3 {
					bufferCopy := make([]int16, len(asrBuffer))
					copy(bufferCopy, asrBuffer)
					asrBuffer = []int16{}
					isSpeechTriggered = false
					vadSilenceCounter = 0
					go processASR(bufferCopy)
				}
			} else {
				// 保持一定的缓冲区
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

// ================= 4. 辅助函数 =================

func containsAny(text string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func logCost(stage string, duration time.Duration) {
	log.Printf("⏱️ [%s] 耗时: %d ms", stage, duration.Milliseconds())
}

func setState(s AppState) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	currentState = s
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
		"parameters": map[string]interface{}{"enable_thinking": false, "enable_search": false},
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
