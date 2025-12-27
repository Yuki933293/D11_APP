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
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
	vado "github.com/maxhawkins/go-webrtc-vad"

	"ai_box/aec"
)

// ================= 1. 配置与常量 =================

// ★★★ 调试开关 ★★★
// true:  完全禁用 AEC 初始化。如果此时麦克风正常，说明是 AEC 库在抢占硬件。
// false: 尝试加载 AEC。
const DISABLE_AEC = false

const DASH_API_KEY = "sk-fb64515c017945fc9282f9ace355cad3"

const TTS_WS_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"
const LLM_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text-generation/generation"
const WS_AS_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"

const MUSIC_DIR = "/userdata/music"

const (
	KWS_TOKENS   = "./models/tokens.txt"
	KWS_ENCODER  = "./models/encoder-epoch-12-avg-2-chunk-16-left-64.onnx"
	KWS_DECODER  = "./models/decoder-epoch-12-avg-2-chunk-16-left-64.onnx"
	KWS_JOINER   = "./models/joiner-epoch-12-avg-2-chunk-16-left-64.onnx"
	KWS_KEYWORDS = "./keywords.txt"
)

const SESSION_TIMEOUT = 30 * time.Second
const WAKE_COOLDOWN = 1000 * time.Millisecond

// ================= 2. 词库 =================
var EXIT_WORDS = []string{
	"关闭系统", "关机", "退出程序", "再见", "退下",
	"拜拜", "结束吧", "结束程序", "停止运行", "关闭助手", "关闭",
}

var INTERRUPT_WORDS = []string{
	"闭嘴", "停止", "安静", "别说了", "暂停", "打断",
	"别唱了", "等一下", "换首歌", "下一首", "切歌", "不要说了",
}

// ================= 3. 全局变量 =================
var (
	sessionCtx    context.Context
	sessionCancel context.CancelFunc
	ctxMutex      sync.Mutex

	currentSessionID string
	sessionIDMutex   sync.Mutex

	insecureClient *http.Client

	ttsManagerChan chan string
	audioPcmChan   chan []byte

	playerStdin io.WriteCloser
	playerCmd   *exec.Cmd
	playerMutex sync.Mutex

	// --- 状态机 ---
	isAwake        bool = false
	lastActiveTime time.Time
	wakeUpTime     time.Time
	statusMutex    sync.Mutex
	kwsSpotter     *sherpa.KeywordSpotter

	emojiRegex *regexp.Regexp
	musicMgr   *MusicManager

	// --- 启动同步锁 ---
	recordStartedChan = make(chan struct{})
	recordStartOnce   sync.Once

	// --- 全局 AEC 处理器 ---
	globalAecProc *aec.Processor
)

// ================= 4. 初始化 =================
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
	log.Println("=== RK3308 AI 助手 (硬件冲突诊断完整版) ===")

	// 1. 硬件诊断
	checkAudioLock()

	// 2. 深度清理
	log.Println("🧹 [Init] 执行全局清理...")
	exec.Command("killall", "-9", "arecord").Run()
	exec.Command("killall", "-9", "aplay").Run()
	time.Sleep(1 * time.Second)

	// 3. 初始化 Sherpa
	log.Println("🚀 [Init] 加载 Sherpa...")
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

	// 4. 初始化 AEC (有条件加载)
	if !DISABLE_AEC {
		log.Println("🚀 [Init] 初始化 AEC 模块...")
		// 注意：如果 aec.NewProcessor 内部打开了 /dev/snd/pcmC2...，这里可能会导致后续 busy
		globalAecProc = aec.NewProcessor()
	} else {
		log.Println("⚠️ [Debug] AEC 已禁用，仅测试麦克风硬件通路")
	}

	// 5. 初始化通道
	ttsManagerChan = make(chan string, 500)
	audioPcmChan = make(chan []byte, 4000)
	sessionCtx, sessionCancel = context.WithCancel(context.Background())
	currentSessionID = uuid.New().String()
	musicMgr = NewMusicManager()

	// 6. 启动后台协程
	go audioPlayer()
	go ttsManagerLoop()
	go timeoutCheckLoop()

	vadEng, err := vado.New()
	if err != nil {
		log.Fatal("❌ VAD 初始化失败:", err)
	}
	vadEng.SetMode(3)

	log.Println("✅ 系统就绪，启动采集...")

	// 7. 进入主循环
	audioLoop(vadEng)
}

// 辅助函数：检查声卡占用（BusyBox 版 fuser 不支持 -v）
func checkAudioLock() {
	log.Println("🔍 [诊断] 检查 card 2 (hw:2,0) 占用...")

	// BusyBox 的 fuser 没有 -v；直接打印 PID 列表即可
	out, err := exec.Command("fuser", "/dev/snd/pcmC2D0c").CombinedOutput()
	s := strings.TrimSpace(string(out))

	if err != nil {
		// 有些实现未占用时也可能返回非 0；因此同时看输出内容
		if s == "" {
			log.Println("✅ 声卡目前空闲 (fuser 无输出)")
			return
		}
		log.Printf("⚠️ fuser 返回错误: %v, 输出: %s", err, s)
		return
	}

	if s != "" {
		log.Printf("⚠️ 声卡被以下 PID 占用: %s", s)
		if strings.Contains(s, fmt.Sprintf("%d", os.Getpid())) {
			log.Println("💀 ai_box 自身持有了锁 (可能是 AEC 库或其他音频模块导致)")
		}
	} else {
		log.Println("✅ 声卡目前空闲")
	}
}

// ================= 5. 音频采集 (8通道直通模式) =================

func audioLoop(vadEng *vado.VAD) {
	// RK3308: hw:2,0 的硬件能力显示 CHANNELS: 10（你已 dump 过）
	dev := "hw:2,0"
	const HW_CH = 10
	const HW_FRAME = 256 // 与 --period-size=256 对齐（每次读一个 period）

	log.Printf("🎤 启动录音 | 设备: %s | 通道: %d | 采样率: 16000", dev, HW_CH)

	// 重要：必须 -c 10，否则会报 Channels count non available 并立刻退出
	cmd := exec.Command(
		"arecord",
		"-D", dev,
		"-c", fmt.Sprintf("%d", HW_CH),
		"-r", "16000",
		"-f", "S16_LE",
		"-t", "raw",
		"--period-size=256",
		"--buffer-size=4096",
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("❌ 启动失败: %v, 详情: %s", err, stderr.String())
	}

	// 启动后短暂等待，便于 arecord 打印真实错误到 stderr
	time.Sleep(200 * time.Millisecond)
	log.Printf("✅ 录音进程已启动 (PID: %d)", cmd.Process.Pid)

	// 解锁播放器
	recordStartOnce.Do(func() {
		close(recordStartedChan)
	})

	// 初始化 Sherpa 流
	kwsStream := sherpa.NewKeywordStream(kwsSpotter)
	floatBuffer := make([]float32, HW_FRAME)

	// 每帧读取字节数：frames * channels * 2bytes
	frameSize := HW_FRAME * HW_CH * 2
	readBuf := make([]byte, frameSize)

	// 复用缓冲，避免每轮分配
	rawInt16 := make([]int16, HW_FRAME*HW_CH)
	monoData := make([]int16, HW_FRAME)

	// 你可以按需要调整：选择哪个通道作为“主麦克风”
	// 先用 0 号通道；如果后续发现唤醒不灵敏，再改 micCh=1/2/...
	const micCh = 0

	vadAccumulator := make([]int16, 0, 4096)
	var asrBuffer []int16
	silenceCount, speechCount := 0, 0
	triggered := false

	// 复用 VAD byte buffer
	vb := make([]byte, 640) // 320 samples * 2 bytes

	for {
		// 1) 读取硬件原始数据
		if _, err := io.ReadFull(stdout, readBuf); err != nil {
			log.Printf("⚠️ 录音流中断(EOF/Error): %v", err)
			log.Printf("🔍 arecord stderr: %s", stderr.String())
			return
		}

		// 2) Byte -> Int16 (10通道)
		for i := 0; i < HW_FRAME*HW_CH; i++ {
			rawInt16[i] = int16(binary.LittleEndian.Uint16(readBuf[i*2 : i*2+2]))
		}

		// 3) AEC 处理（你当前先旁路，避免通道映射问题）
		var processingData []int16
		if !DISABLE_AEC && globalAecProc != nil {
			// 注意：如果 AEC 的 Process 需要 10ch，这里可以恢复调用；
			// 但若 AEC 内部会抢占 /dev/snd，则可能导致 busy，需要单独排查。
			// processingData, _ = globalAecProc.Process(rawInt16)
			processingData = rawInt16 // 先旁路，确保采集可用
		} else {
			processingData = rawInt16
		}
		if processingData == nil {
			continue
		}

		// 4) 提取单声道（用于 KWS + VAD）
		for i := 0; i < HW_FRAME; i++ {
			monoData[i] = processingData[i*HW_CH+micCh]
		}

		// 5) Sherpa KWS
		for i, v := range monoData {
			floatBuffer[i] = float32(v) / 32768.0
		}
		kwsStream.AcceptWaveform(16000, floatBuffer)

		// 关键：按推荐方式循环 Decode，否则可能一直不出结果 :contentReference[oaicite:3]{index=3}
		for kwsSpotter.IsReady(kwsStream) {
			kwsSpotter.Decode(kwsStream)
		}

		kwRes := kwsSpotter.GetResult(kwsStream)
		if kwRes != nil && kwRes.Keyword != "" {
			log.Printf("✨ [KWS HIT] keyword=%q", kwRes.Keyword)

			// 调试阶段：命中任意 keyword 即唤醒（通常 keywords.txt 里只有一个）
			performWakeUp()

			// 清空缓存，避免“唤醒后被历史语音触发”
			triggered = false
			asrBuffer = nil
			vadAccumulator = vadAccumulator[:0]
			silenceCount, speechCount = 0, 0

			// 关键：检测到 keyword 后应 Reset stream（避免状态粘住/异常）
			kwsSpotter.Reset(kwsStream)

			continue
		}

		// 6) 状态拦截
		statusMutex.Lock()
		awake := isAwake
		inCooldown := time.Since(wakeUpTime) < WAKE_COOLDOWN
		statusMutex.Unlock()

		if !awake || inCooldown {
			asrBuffer = nil
			continue
		}

		// 7) VAD 处理
		vadAccumulator = append(vadAccumulator, monoData...)

		for len(vadAccumulator) >= 320 {
			frame := vadAccumulator[:320]
			vadAccumulator = vadAccumulator[320:]

			for i, v := range frame {
				binary.LittleEndian.PutUint16(vb[i*2:], uint16(v))
			}

			active, _ := vadEng.Process(16000, vb)
			if active {
				speechCount++
				silenceCount = 0
			} else {
				silenceCount++
				speechCount = 0
			}

			if speechCount > 4 && !triggered {
				triggered = true
				musicMgr.Duck()
				log.Println("👂 [VAD] 检测到人声...")
			}

			if triggered {
				asrBuffer = append(asrBuffer, frame...)
				if silenceCount > 15 || len(asrBuffer) > 16000*8 {
					if len(asrBuffer) > 4800 {
						finalData := make([]int16, len(asrBuffer))
						copy(finalData, asrBuffer)
						go processASR(finalData)
					} else {
						musicMgr.Unduck()
					}
					asrBuffer = nil
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

// ================= 6. 业务逻辑 =================

func performWakeUp() {
	log.Println("✨ 【本地唤醒成功】")
	performStop()

	statusMutex.Lock()
	isAwake = true
	lastActiveTime = time.Now()
	wakeUpTime = time.Now()
	statusMutex.Unlock()

	ttsManagerChan <- "我在"
	ttsManagerChan <- "[[END]]"
}

func updateActiveTime() {
	statusMutex.Lock()
	lastActiveTime = time.Now()
	statusMutex.Unlock()
}

func performStop() {
	log.Println("🧹 [物理清理]: 停止所有声音")
	ctxMutex.Lock()
	if sessionCancel != nil {
		sessionCancel()
	}
	ctxMutex.Unlock()

	flushChannel(ttsManagerChan)
	flushChannel(audioPcmChan)

	exec.Command("killall", "-9", "aplay").Run()
	musicMgr.Stop()

	playerMutex.Lock()
	if playerStdin != nil {
		playerStdin.Close()
	}
	playerCmd = nil
	playerStdin = nil
	playerMutex.Unlock()
}

func processASR(pcm []int16) {
	if float64(len(pcm))/16000.0 < 0.5 {
		return
	}

	updateActiveTime()

	pcmBytes := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	text := callASRWebSocket(pcmBytes)
	if text == "" {
		musicMgr.Unduck()
		return
	}
	log.Printf("✅ [用户]: %s", text)

	if isExit(text) {
		performStop()
		ttsManagerChan <- "再见"
		ttsManagerChan <- "[[END]]"
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}

	playerMutex.Lock()
	isTtsBusy := playerCmd != nil
	playerMutex.Unlock()
	if (isTtsBusy || musicMgr.IsPlaying()) && isInterrupt(text) {
		performStop()
		ttsManagerChan <- "我在"
		ttsManagerChan <- "[[END]]"
		return
	}

	enableSearch := strings.Contains(text, "天气") || strings.Contains(text, "新闻") || strings.Contains(text, "今天")

	ctxMutex.Lock()
	if sessionCancel != nil {
		sessionCancel()
	}
	sessionCtx, sessionCancel = context.WithCancel(context.Background())
	currentCtx := sessionCtx
	ctxMutex.Unlock()

	go callAgentStream(currentCtx, text, enableSearch)
}

func callAgentStream(ctx context.Context, prompt string, enableSearch bool) {
	flushChannel(ttsManagerChan)
	llmStart := time.Now()

	modelName := "qwen-turbo"
	if enableSearch {
		modelName = "qwen-max"
	}

	systemPrompt := "你是智能助手。简洁回复。点歌用 [PLAY:歌名]（随机 [PLAY:RANDOM]），停止用 [STOP]。"

	payload := map[string]interface{}{
		"model": modelName,
		"input": map[string]interface{}{
			"messages": []map[string]string{
				{"role": "system", "content": systemPrompt},
				{"role": "user", "content": prompt},
			},
		},
		"parameters": map[string]interface{}{
			"result_format": "text", "incremental_output": true, "enable_search": enableSearch,
		},
	}

	jsonBody, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", LLM_URL, bytes.NewReader(jsonBody))
	req.Header.Set("Authorization", "Bearer "+DASH_API_KEY)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := insecureClient.Do(req)
	if err != nil {
		log.Printf("❌ LLM请求失败: %v", err)
		musicMgr.Unduck()
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var fullTextBuilder strings.Builder
	var chunkBuffer strings.Builder

	fmt.Print("📝 [LLM]: ")

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var chunk struct{ Output struct{ Text string } }
		json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &chunk)

		clean := cleanText(chunk.Output.Text)
		if clean == "" {
			continue
		}

		fmt.Print(clean)
		fullTextBuilder.WriteString(clean)
		chunkBuffer.WriteString(clean)

		if strings.ContainsAny(clean, "，。！？,.!?\n") || chunkBuffer.Len() > 20 {
			sendChunk(&chunkBuffer)
		}
	}
	fmt.Println()
	log.Printf("⏱️ LLM耗时: %v", time.Since(llmStart))

	sendChunk(&chunkBuffer)
	ttsManagerChan <- "[[END]]"

	fullText := fullTextBuilder.String()
	if strings.Contains(fullText, "[STOP]") {
		musicMgr.Stop()
	}
	if matches := regexp.MustCompile(`(?i)\[PLAY:\s*(.*?)\]`).FindStringSubmatch(fullText); len(matches) > 1 {
		musicMgr.SearchAndPlay(strings.TrimSpace(matches[1]))
	}
}

// ================= 7. 辅助功能函数 =================

func sendChunk(buf *strings.Builder) {
	text := regexp.MustCompile(`\[.*?\]`).ReplaceAllString(buf.String(), "")
	if s := strings.TrimSpace(text); s != "" {
		ttsManagerChan <- s
	}
	buf.Reset()
}

func cleanText(text string) string { return strings.TrimSpace(emojiRegex.ReplaceAllString(text, "")) }
func isExit(text string) bool {
	for _, w := range EXIT_WORDS {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}
func isInterrupt(text string) bool {
	for _, w := range INTERRUPT_WORDS {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
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

func timeoutCheckLoop() {
	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		statusMutex.Lock()
		if isAwake && time.Since(lastActiveTime) > SESSION_TIMEOUT && !musicMgr.IsPlaying() {
			log.Println("💤 [超时] 待机")
			isAwake = false
			ttsManagerChan <- "退下"
			ttsManagerChan <- "[[END]]"
		}
		statusMutex.Unlock()
	}
}

func ttsManagerLoop() {
	var conn *websocket.Conn
	var wg sync.WaitGroup
	var taskID string

	recv := func(c *websocket.Conn) {
		defer wg.Done()
		for {
			mt, m, e := c.ReadMessage()
			if e != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				audioPcmChan <- m
			}
		}
	}

	for {
		msg, ok := <-ttsManagerChan
		if !ok {
			return
		}
		if msg == "[[END]]" {
			if conn != nil {
				conn.WriteJSON(map[string]interface{}{"header": map[string]interface{}{"task_id": taskID, "action": "finish-task", "streaming": "duplex"}})
				wg.Wait()
				conn.Close()
				conn = nil
			}
			continue
		}
		if conn == nil {
			dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
			headers := http.Header{"Authorization": []string{"Bearer " + DASH_API_KEY}}
			conn, _, _ = dialer.Dial(TTS_WS_URL, headers)
			taskID = uuid.New().String()
			wg.Add(1)
			go recv(conn)
			conn.WriteJSON(map[string]interface{}{"header": map[string]interface{}{"task_id": taskID, "action": "run-task", "streaming": "duplex"}, "payload": map[string]interface{}{"task_group": "audio", "task": "tts", "function": "SpeechSynthesizer", "model": "cosyvoice-v2", "parameters": map[string]interface{}{"text_type": "PlainText", "voice": "longhua_v2", "format": "pcm", "sample_rate": 22050, "volume": 50}}})
		}
		conn.WriteJSON(map[string]interface{}{"header": map[string]interface{}{"task_id": taskID, "action": "continue-task", "streaming": "duplex"}, "payload": map[string]interface{}{"input": map[string]interface{}{"text": msg}}})
	}
}

func audioPlayer() {
	// ★★★ 等待录音启动信号 ★★★
	<-recordStartedChan

	doStart := func() (*exec.Cmd, io.WriteCloser) {
		c := exec.Command("aplay", "-D", "default", "-t", "raw", "-r", "22050", "-f", "S16_LE", "-c", "1", "-B", "20000")
		s, _ := c.StdinPipe()
		c.Start()
		playerMutex.Lock()
		playerCmd = c
		playerStdin = s
		playerMutex.Unlock()
		return c, s
	}
	for pcmData := range audioPcmChan {
		if len(pcmData) == 0 {
			time.Sleep(500 * time.Millisecond)
			if playerStdin != nil {
				playerStdin.Close()
			}
			continue
		}
		if playerStdin == nil {
			doStart()
		}
		if playerStdin != nil {
			playerStdin.Write(pcmData)
		}
	}
}

func callASRWebSocket(data []byte) string {
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	headers := http.Header{"Authorization": []string{"Bearer " + DASH_API_KEY}}
	conn, _, err := dialer.Dial(WS_AS_URL, headers)
	if err != nil {
		return ""
	}
	defer conn.Close()
	id := fmt.Sprintf("%032x", rand.Int63())
	conn.WriteJSON(map[string]interface{}{"header": map[string]interface{}{"task_id": id, "action": "run-task", "streaming": "duplex"}, "payload": map[string]interface{}{"task_group": "audio", "task": "asr", "function": "recognition", "model": "paraformer-realtime-v2", "parameters": map[string]interface{}{"format": "pcm", "sample_rate": 16000}}})
	for i := 0; i < len(data); i += 3200 {
		end := i + 3200
		if end > len(data) {
			end = len(data)
		}
		conn.WriteMessage(websocket.BinaryMessage, data[i:end])
		time.Sleep(5 * time.Millisecond)
	}
	conn.WriteJSON(map[string]interface{}{"header": map[string]interface{}{"task_id": id, "action": "finish-task"}})
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

// ================= 8. 管理器实现 =================

type MusicManager struct {
	isPlaying     bool
	mu            sync.Mutex
	cmd           *exec.Cmd
	stopChan      chan struct{}
	volMutex      sync.Mutex
	targetVolume  float64
	currentVolume float64
}

func NewMusicManager() *MusicManager    { return &MusicManager{targetVolume: 1.0, currentVolume: 1.0} }
func (m *MusicManager) IsPlaying() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.isPlaying }
func (m *MusicManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isPlaying {
		select {
		case m.stopChan <- struct{}{}:
		default:
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
	time.Sleep(100 * time.Millisecond)
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := exec.Command("aplay", "-D", "default", "-q", "-t", "raw", "-r", "16000", "-c", "1", "-f", "S16_LE")
	file, err := os.Open(path)
	if err != nil {
		return
	}
	cmd.Stdin = file
	if err := cmd.Start(); err == nil {
		m.cmd = cmd
		m.isPlaying = true
		m.stopChan = make(chan struct{}, 1)
		go func() {
			cmd.Wait()
			file.Close()
			m.mu.Lock()
			if m.cmd == cmd {
				m.isPlaying = false
			}
			m.mu.Unlock()
		}()
	} else {
		file.Close()
	}
}
func (m *MusicManager) SearchAndPlay(query string) bool {
	files, _ := ioutil.ReadDir(MUSIC_DIR)
	var candidates []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".wav") {
			candidates = append(candidates, filepath.Join(MUSIC_DIR, f.Name()))
		}
	}
	if len(candidates) == 0 {
		return false
	}
	target := candidates[rand.Intn(len(candidates))]
	m.PlayFile(target)
	return true
}
func (m *MusicManager) Duck()   { m.volMutex.Lock(); m.targetVolume = 0.2; m.volMutex.Unlock() }
func (m *MusicManager) Unduck() { m.volMutex.Lock(); m.targetVolume = 1.0; m.volMutex.Unlock() }
