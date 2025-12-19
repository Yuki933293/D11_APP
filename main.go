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
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
	vado "github.com/maxhawkins/go-webrtc-vad"

	"ai_box/aec"
)

// ================= 配置区 =================
const DASH_API_KEY = "sk-fb64515c017945fc9282f9ace355cad3"
const APP_ID = "16356830643247938dfa31f8414fd58d"

const WS_ASR_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"
const TTS_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"

const MUSIC_DIR = "/userdata/music"
const SESSION_TIMEOUT = 30 * time.Second

// 唤醒后冷却时间 (防止听到自己说话)
const WAKE_COOLDOWN = 1500 * time.Millisecond

// Sherpa 模型路径
const (
	KWS_TOKENS   = "./models/tokens.txt"
	KWS_ENCODER  = "./models/encoder-epoch-12-avg-2-chunk-16-left-64.onnx"
	KWS_DECODER  = "./models/decoder-epoch-12-avg-2-chunk-16-left-64.onnx"
	KWS_JOINER   = "./models/joiner-epoch-12-avg-2-chunk-16-left-64.onnx"
	KWS_KEYWORDS = "./keywords.txt"
)

var EXIT_WORDS = []string{"关闭系统", "关机", "退出程序", "再见", "退下", "拜拜", "结束对话", "结束程序", "关闭"}
var INTERRUPT_WORDS = []string{"闭嘴", "停止", "安静", "别说了", "别唱了", "关掉音乐", "停止播放", "等一下", "暂停"}

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

	// --- 唤醒状态 ---
	isAwake        bool = false
	lastActiveTime time.Time
	wakeUpTime     time.Time
	statusMutex    sync.Mutex
	kwsSpotter     *sherpa.KeywordSpotter

	// --- 音乐 ---
	musicCmd       *exec.Cmd
	musicStdin     io.WriteCloser
	musicMutex     sync.Mutex
	isMusicPlaying bool
	targetVolume   float64 = 1.0
	currentVolume  float64 = 1.0
	volMutex       sync.Mutex
	stopMusicChan  chan struct{}

	// --- TTS ---
	ttsCmd   *exec.Cmd
	ttsMutex sync.Mutex
)

func init() {
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	insecureClient = &http.Client{Transport: tr, Timeout: 0}
	rand.Seed(time.Now().UnixNano())
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== RK3308 AI 助手 (V70.0 修复反馈版) ===")

	globalSessionID = generateSessionID()
	log.Printf("✨ 会话ID: %s", globalSessionID)

	log.Println("🚀 [1/3] 加载唤醒模型...")
	featConfig := sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80}
	modelConfig := sherpa.OnlineModelConfig{
		Transducer: sherpa.OnlineTransducerModelConfig{
			Encoder: KWS_ENCODER, Decoder: KWS_DECODER, Joiner: KWS_JOINER,
		},
		Tokens:     KWS_TOKENS,
		NumThreads: 1,
		Provider:   "cpu",
		ModelType:  "zipformer2",
	}
	kwsConfig := sherpa.KeywordSpotterConfig{
		FeatConfig:   featConfig,
		ModelConfig:  modelConfig,
		KeywordsFile: KWS_KEYWORDS,
	}
	kwsSpotter = sherpa.NewKeywordSpotter(&kwsConfig)
	if kwsSpotter == nil {
		log.Fatal("❌ Sherpa加载失败")
	}
	log.Println("✅ [2/3] 唤醒引擎就绪")

	aecProc := aec.NewProcessor()
	vadEng, err := vado.New()
	if err != nil {
		log.Fatal(err)
	}
	vadEng.SetMode(2)

	stopPlayChan = make(chan struct{}, 1)

	go timeoutCheckLoop()
	log.Println("✅ [3/3] 系统启动完成，待机中... (请喊 '小瑞')")
	audioLoop(aecProc, vadEng)

	select {}
}

func generateSessionID() string {
	return fmt.Sprintf("session-%d-%d", time.Now().Unix(), rand.Intn(10000))
}

func updateActiveTime() {
	statusMutex.Lock()
	lastActiveTime = time.Now()
	statusMutex.Unlock()
}

func timeoutCheckLoop() {
	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		statusMutex.Lock()
		musicMutex.Lock()
		playing := isMusicPlaying
		musicMutex.Unlock()

		if isAwake && !playing && time.Since(lastActiveTime) > SESSION_TIMEOUT {
			log.Println("💤 [超时] 自动进入待机模式")
			isAwake = false
			go speakQwenFlashStream("小瑞先退下了")
		}
		statusMutex.Unlock()
	}
}

// 核心唤醒逻辑
func performWakeUp() {
	log.Println("✨ 【触发唤醒】")
	stopTTS()
	duckMusic()

	statusMutex.Lock()
	isAwake = true
	lastActiveTime = time.Now()
	wakeUpTime = time.Now()
	statusMutex.Unlock()

	go speakQwenFlashStream("我在")
}

// ================= 音乐控制 =================

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
		setTargetVolume(0.2)
	}
}

func unduckMusic() {
	musicMutex.Lock()
	playing := isMusicPlaying
	musicMutex.Unlock()
	if playing {
		log.Println("📈 [恢复] 恢复音乐音量")
		setTargetVolume(1.0)
	}
}

func playMusicFile(path string) bool {
	musicMutex.Lock()
	defer musicMutex.Unlock()

	if isMusicPlaying {
		select {
		case stopMusicChan <- struct{}{}:
		default:
		}
		if musicStdin != nil {
			musicStdin.Close()
		}
		if musicCmd != nil && musicCmd.Process != nil {
			musicCmd.Process.Kill()
			_ = musicCmd.Wait()
		}
		time.Sleep(100 * time.Millisecond)
	}

	file, err := os.Open(path)
	if err != nil {
		return false
	}

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

	musicCmd = cmd
	musicStdin = stdin
	isMusicPlaying = true
	stopMusicChan = make(chan struct{}, 1)
	setTargetVolume(1.0)
	currentVolume = 1.0

	log.Printf("🎵 正在播放: %s", filepath.Base(path))

	go func(f *os.File, pipe io.WriteCloser, myCmd *exec.Cmd) {
		defer f.Close()
		defer pipe.Close()
		f.Seek(44, 0)
		buf := make([]byte, 1024)
		int16Buf := make([]int16, 512)

		for {
			select {
			case <-stopMusicChan:
				return
			default:
			}
			n, err := f.Read(buf)
			if err != nil {
				break
			}

			volMutex.Lock()
			target := targetVolume
			volMutex.Unlock()

			if currentVolume < target {
				currentVolume += 0.05
			} else if currentVolume > target {
				currentVolume -= 0.05
			}

			count := n / 2
			for i := 0; i < count; i++ {
				sample := int16(binary.LittleEndian.Uint16(buf[i*2 : i*2+2]))
				int16Buf[i] = int16(float64(sample) * currentVolume)
			}
			for i := 0; i < count; i++ {
				binary.LittleEndian.PutUint16(buf[i*2:], uint16(int16Buf[i]))
			}
			_, wErr := pipe.Write(buf[:n])
			if wErr != nil {
				return
			}
		}
		musicMutex.Lock()
		defer musicMutex.Unlock()
		if isMusicPlaying && musicCmd == myCmd {
			isMusicPlaying = false
			go func() { myCmd.Wait() }()
		}
	}(file, stdin, cmd)

	return true
}

func stopMusic() {
	musicMutex.Lock()
	defer musicMutex.Unlock()
	if isMusicPlaying {
		log.Println("🛑 停止背景音乐")
		select {
		case stopMusicChan <- struct{}{}:
		default:
		}
		if musicStdin != nil {
			musicStdin.Close()
		}
		if musicCmd != nil && musicCmd.Process != nil {
			musicCmd.Process.Kill()
			_ = musicCmd.Wait()
		}
		isMusicPlaying = false
		musicCmd = nil
		musicStdin = nil
	}
}

func searchAndPlay(keyword string) (bool, string) {
	var candidates []string
	log.Printf("🔍 正在搜索: [%s]", keyword)
	subKeywords := strings.Fields(keyword)
	filepath.WalkDir(MUSIC_DIR, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".wav") {
			return nil
		}
		if keyword == "" {
			candidates = append(candidates, path)
			return nil
		}
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
	target := candidates[rand.Intn(len(candidates))]
	success := playMusicFile(target)
	return success, strings.TrimSuffix(filepath.Base(target), filepath.Ext(filepath.Base(target)))
}

// ================= TTS 控制 (Qwen Flash Stream 修复版) =================

func stopTTS() {
	ttsMutex.Lock()
	defer ttsMutex.Unlock()
	if ttsCmd != nil && ttsCmd.Process != nil {
		if ttsCmd.ProcessState == nil || !ttsCmd.ProcessState.Exited() {
			ttsCmd.Process.Kill()
			ttsCmd.Wait()
		}
	}
	ttsCmd = nil
}

func speakQwenFlashStream(text string) {
	stopTTS()
	select {
	case <-stopPlayChan:
	default:
	}

	log.Printf("🔊 [TTS] 准备播放: %s", text) // ★ 显影
	setState(STATE_SPEAKING)

	payload := map[string]interface{}{
		"model":      "qwen-tts",
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
		log.Printf("❌ [TTS] 网络请求失败: %v", err)
		setState(STATE_LISTENING)
		return
	}
	defer resp.Body.Close()

	// ★★★ 核心修复：检查 HTTP 状态码 ★★★
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("❌ [TTS] 服务端报错 (Code %d): %s", resp.StatusCode, string(body))
		setState(STATE_LISTENING)
		return
	}

	cmd := exec.Command("aplay", "-D", "default", "-q", "-t", "raw", "-r", "24000", "-f", "S16_LE", "-c", "1")
	playStdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("❌ [TTS] aplay启动失败: %v", err)
		setState(STATE_LISTENING)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("❌ [TTS] aplay执行失败: %v", err)
		setState(STATE_LISTENING)
		return
	}

	ttsMutex.Lock()
	ttsCmd = cmd
	ttsMutex.Unlock()

	go func(c *exec.Cmd) {
		c.Wait()
		ttsMutex.Lock()
		if ttsCmd == c {
			ttsCmd = nil
		}
		ttsMutex.Unlock()

		stateMutex.Lock()
		if currentState == STATE_SPEAKING {
			currentState = STATE_LISTENING
		}
		stateMutex.Unlock()
	}(cmd)

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		select {
		case <-stopPlayChan:
			cmd.Process.Kill()
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
			if err == nil {
				playStdin.Write(audioBytes)
			}
		}
	}
	playStdin.Close()
	updateActiveTime()
}

// ================= 核心逻辑 =================

func performStop() {
	select {
	case stopPlayChan <- struct{}{}:
	default:
	}
	stopTTS()
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
	updateActiveTime()

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

	curr := getState()
	musicMutex.Lock()
	playing := isMusicPlaying
	musicMutex.Unlock()
	if curr == STATE_SPEAKING && !playing {
		log.Println("⚠️ [Busy] 正在说话，忽略普通指令")
		return
	}

	systemPrompt := `你是一个智能音箱助手。
1. 用户想听歌/随机播放 (如"放首歌","听周杰伦","来首稻香") -> {"action":"play","keyword":"搜索词","reply":"好的..."}。如果未指定歌名keyword设为空。
2. 用户想聊天 -> {"action":"chat","reply":"回复内容"}。
3. 只返回JSON，不要Markdown。`

	fullPrompt := systemPrompt + "\n用户输入：" + text
	jsonResponse := callAgent(fullPrompt)

	// ★ V70: 恢复 LLM 日志，方便调试
	log.Printf("🤖 [LLM回复] %s", jsonResponse)
	logCost("LLM决策", time.Since(pipelineStart))

	jsonResponse = strings.Trim(jsonResponse, "```json")
	jsonResponse = strings.Trim(jsonResponse, "```")

	var intent struct {
		Action  string `json:"action"`
		Keyword string `json:"keyword"`
		Reply   string `json:"reply"`
	}
	err := json.Unmarshal([]byte(jsonResponse), &intent)

	if err != nil {
		if playing {
			log.Println("🤫 [音乐模式] 解析失败，保持安静")
			unduckMusic()
		} else {
			speakQwenFlashStream(jsonResponse) // 朗读原始内容
		}
	} else {
		if intent.Action == "play" {
			// ★ V70: 模糊词处理
			if intent.Keyword == "音乐" || intent.Keyword == "歌曲" || intent.Keyword == "歌" {
				intent.Keyword = "" // 转为随机播放
			}

			speakQwenFlashStream(intent.Reply)
			success, songName := searchAndPlay(intent.Keyword)
			if !success {
				speakQwenFlashStream("抱歉，本地曲库没有找到这首歌。")
			} else {
				log.Printf("🎵 即将播放: %s", songName)
			}
		} else {
			if playing {
				log.Printf("🤫 [音乐模式] 识别为闲聊(%s)，主动忽略", intent.Reply)
				unduckMusic()
			} else {
				speakQwenFlashStream(intent.Reply)
				unduckMusic()
			}
		}
	}
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

	kwsStream := sherpa.NewKeywordStream(kwsSpotter)
	floatBuffer := make([]float32, 256)

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

		// KWS
		for i, v := range cleanAudioChunk {
			floatBuffer[i] = float32(v) / 32768.0
		}
		kwsStream.AcceptWaveform(16000, floatBuffer)

		if kwsSpotter.IsReady(kwsStream) {
			kwsSpotter.Decode(kwsStream)
			keyword := kwsSpotter.GetResult(kwsStream).Keyword
			if keyword != "" {
				if strings.Contains(keyword, "iǎo") || strings.Contains(keyword, "uì") {
					performWakeUp()
					vadSpeechCounter = 0
					vadSilenceCounter = 0
					isSpeechTriggered = false
					asrBuffer = nil
					kwsStream = sherpa.NewKeywordStream(kwsSpotter)
					continue
				}
			}
		}

		statusMutex.Lock()
		awake := isAwake
		inCooldown := time.Since(wakeUpTime) < WAKE_COOLDOWN
		statusMutex.Unlock()

		if !awake {
			asrBuffer = nil
			continue
		}
		if inCooldown {
			asrBuffer = nil
			continue
		}

		// VAD
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

			if vadSpeechCounter > 15 {
				if !isSpeechTriggered {
					log.Println("👂 [VAD] 检测到说话...")
					duckMusic()
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

func getState() AppState {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	return currentState
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
		"input":      map[string]string{"prompt": prompt, "session_id": globalSessionID},
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
