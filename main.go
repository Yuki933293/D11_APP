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

const SAVE_DEBUG_AUDIO = true // ★ 开启录音保存，用于听听到底录了什么
//const DIGITAL_GAIN = 5.0      // ★ 数字增益倍数：将原始音量放大 5 倍

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
	"别唱了", "等一下", "不要说了",
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

type TTSState int

const (
	TTSIdle TTSState = iota
	TTSReserved
	TTSSpeaking
)

var (
	ttsStateMu sync.Mutex
	ttsState   TTSState = TTSIdle
)

// busy = Reserved 或 Speaking
func ttsIsBusy() bool {
	ttsStateMu.Lock()
	defer ttsStateMu.Unlock()
	return ttsState != TTSIdle
}

// 只有在 Idle 时才能抢占“本次允许播报”的资格
func ttsTryReserve() bool {
	ttsStateMu.Lock()
	defer ttsStateMu.Unlock()
	if ttsState != TTSIdle {
		return false
	}
	ttsState = TTSReserved
	return true
}

func ttsMarkSpeaking() {
	ttsStateMu.Lock()
	defer ttsStateMu.Unlock()
	ttsState = TTSSpeaking
}

func ttsRelease() {
	ttsStateMu.Lock()
	ttsState = TTSIdle
	ttsStateMu.Unlock()
}

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

	// 释放“播报门控”，避免状态卡死
	ttsRelease()

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
	sec := float64(len(pcm)) / 16000.0
	const MIN_SEC = 0.18
	if sec < MIN_SEC {
		musicMgr.Unduck()
		return
	}

	updateActiveTime()

	pcmBytes := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}

	text := callASRWebSocket(pcmBytes)
	text = strings.TrimSpace(text)
	if text == "" {
		musicMgr.Unduck()
		return
	}

	log.Printf("✅ [用户]: %s", text)

	// 1) 退出
	if isExit(text) {
		performStop()
		ttsManagerChan <- "好的，再见"
		ttsManagerChan <- "[[END]]"
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}

	// 2) 音乐控制：本地优先
	cmd, q := parseMusicCmd(text)
	trim := strings.TrimSpace(strings.Trim(text, "。！!？?，, "))
	if cmd == MusicCmdNone {
		if (trim == "停止" || trim == "暂停") && musicMgr.IsPlaying() {
			cmd = MusicCmdStop
		}
	}

	switch cmd {
	case MusicCmdStop:
		musicMgr.Stop()
		musicMgr.Unduck()
		return
	case MusicCmdNext:
		ok := musicMgr.SearchAndPlay("")
		if !ok {
			ttsManagerChan <- "我这边没有找到可播放的歌曲"
			ttsManagerChan <- "[[END]]"
		}
		musicMgr.Unduck()
		return
	case MusicCmdPlayRandom:
		ok := musicMgr.SearchAndPlay("")
		if !ok {
			ttsManagerChan <- "我这边没有找到可播放的歌曲"
			ttsManagerChan <- "[[END]]"
		}
		musicMgr.Unduck()
		return
	case MusicCmdPlayQuery:
		ok := musicMgr.SearchAndPlay(q)
		if !ok {
			ttsManagerChan <- fmt.Sprintf("没找到“%s”相关的歌曲", q)
			ttsManagerChan <- "[[END]]"
		}
		musicMgr.Unduck()
		return
	}

	// 3) “打断”仅用于打断播报
	if ttsIsBusy() && isInterrupt(text) {
		performStop()
		return
	}

	// 4) 正在播放音乐：聊天不理会
	if musicMgr.IsPlaying() {
		musicMgr.Unduck()
		return
	}

	// 5) 进入 LLM（天气/新闻才 enable_search）
	enableSearch := strings.Contains(text, "天气") || strings.Contains(text, "新闻")

	ctxMutex.Lock()
	if sessionCancel != nil {
		sessionCancel()
	}
	sessionCtx, sessionCancel = context.WithCancel(context.Background())
	currentCtx := sessionCtx
	ctxMutex.Unlock()

	// ===== 关键：播报门控 =====
	// 播报中识别到聊天：允许 LLM 推理/打印，但绝不送入 TTS
	allowTTS := !ttsIsBusy()

	go callAgentStream(currentCtx, text, enableSearch, allowTTS)
}

func callAgentStream(ctx context.Context, prompt string, enableSearch bool, allowTTS bool) {
	// 如果本次希望播报，先尝试 Reserve；失败则自动降级为“不播报”
	if allowTTS {
		if !ttsTryReserve() {
			allowTTS = false
		}
	}

	// 只有允许播报时才清空待播报队列；播报中绝不能 flush（否则会破坏当前播报）
	if allowTTS {
		dropped := flushChannelCount(ttsManagerChan)
		if dropped > 0 {
			log.Printf("⚠️ [TTS] 丢弃了 %d 条待播报文本", dropped)
		}
	}

	llmStart := time.Now()

	modelName := "qwen-turbo"
	if enableSearch {
		modelName = "qwen-max"
	}

	systemPrompt := `你是智能助手。仅在用户明确要求播放音乐（如“放首歌”“听周杰伦”“换首歌”“下一首”“切歌”“想听心跳”）时，才允许在回复末尾输出一次 [PLAY: 歌名]；随机用 [PLAY:RANDOM]。仅在用户明确要求停止/暂停音乐（如“停止音乐”“别唱了”“暂停音乐”）时，才允许输出 [STOP]。回答天气、新闻、闲聊等普通问题时，严禁输出任何 [PLAY] 或 [STOP]。回复保持简洁。`

	payload := map[string]interface{}{
		"model": modelName,
		"input": map[string]interface{}{
			"messages": []map[string]string{
				{"role": "system", "content": systemPrompt},
				{"role": "user", "content": prompt},
			},
		},
		"parameters": map[string]interface{}{
			"result_format":      "text",
			"incremental_output": true,
			"enable_search":      enableSearch,
		},
	}

	jsonBody, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", LLM_URL, bytes.NewReader(jsonBody))
	req.Header.Set("Authorization", "Bearer "+DASH_API_KEY)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := insecureClient.Do(req)
	if err != nil {
		// 如果已 Reserve 但没播报，要释放
		if allowTTS {
			ttsRelease()
		}
		musicMgr.Unduck()
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var fullTextBuilder strings.Builder
	var chunkBuffer strings.Builder
	ttsEnqueued := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		var chunk struct {
			Output struct {
				Text string `json:"text"`
			} `json:"output"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		clean := cleanText(chunk.Output.Text)
		if clean == "" {
			continue
		}

		fullTextBuilder.WriteString(clean)

		// 只有允许播报才把 chunk 送入 TTS 管道
		if allowTTS {
			chunkBuffer.WriteString(clean)
			if strings.ContainsAny(clean, "，。！？,.!?\n") || chunkBuffer.Len() > 20 {
				if sendChunk(&chunkBuffer) {
					ttsEnqueued = true
				}
			}
		}
	}

	fmt.Println()
	log.Printf("⏱️ LLM耗时: %v", time.Since(llmStart))

	// 允许播报时：flush 最后一段 + 发送 END
	if allowTTS {
		if sendChunk(&chunkBuffer) {
			ttsEnqueued = true
		}

		if ttsEnqueued {
			ttsManagerChan <- "[[END]]"
		} else {
			// 没有任何可播报文本，释放占用
			ttsRelease()
		}
	}

	// 打印 LLM 文本（不含 [PLAY]/[STOP]）
	fullText := fullTextBuilder.String()
	fullForlog := regexp.MustCompile(`\[.*?\]`).ReplaceAllString(fullText, "")
	fullForlog = strings.TrimSpace(fullForlog)
	if fullForlog != "" {
		log.Printf("📝 [LLM] 回复: %s", fullForlog)
	}

	// 如果当前处于“播报中”，本次就是 allowTTS=false：严格不做任何“送入 TTS”的动作
	// 但音乐 [PLAY]/[STOP] 仍只在“用户明确意图”下执行（你原本策略保持）
	userWantsPlay := isMusicPlayIntent(prompt)
	userWantsStop := isMusicStopIntent(prompt)

	if userWantsStop && strings.Contains(fullText, "[STOP]") {
		musicMgr.Stop()
	}
	if userWantsPlay {
		if matches := regexp.MustCompile(`(?i)\[PLAY:\s*(.*?)\]`).FindStringSubmatch(fullText); len(matches) > 1 {
			musicMgr.SearchAndPlay(strings.TrimSpace(matches[1]))
		}
	}
}

// ================= 7. 辅助功能函数 =================
func sendChunk(buf *strings.Builder) bool {
	text := regexp.MustCompile(`\[.*?\]`).ReplaceAllString(buf.String(), "")
	s := strings.TrimSpace(text)
	buf.Reset()

	if s == "" {
		return false
	}
	ttsManagerChan <- s
	return true
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

// ================= 7. TTS 管理：ttsManagerLoop（新增“播报文本日志”，并避免过早 close 导致截断） =================
func ttsManagerLoop() {
	type ttsResp struct {
		Header struct {
			TaskID       string `json:"task_id"`
			Event        string `json:"event"`
			ErrorCode    string `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		} `json:"header"`
	}

	var conn *websocket.Conn
	var taskID string
	var eventCh chan ttsResp
	var taskStarted bool

	var lastAudioMu sync.Mutex
	var lastAudioAt time.Time
	setLastAudio := func(t time.Time) {
		lastAudioMu.Lock()
		lastAudioAt = t
		lastAudioMu.Unlock()
	}
	getLastAudio := func() time.Time {
		lastAudioMu.Lock()
		defer lastAudioMu.Unlock()
		return lastAudioAt
	}

	startRecv := func(c *websocket.Conn, ch chan ttsResp) {
		go func() {
			for {
				mt, m, e := c.ReadMessage()
				if e != nil {
					return
				}
				switch mt {
				case websocket.BinaryMessage:
					if len(m) > 0 {
						setLastAudio(time.Now())
						audioPcmChan <- m
					}
				case websocket.TextMessage:
					var r ttsResp
					if err := json.Unmarshal(m, &r); err == nil && r.Header.Event != "" {
						if r.Header.Event == "task-started" || r.Header.Event == "task-finished" ||
							r.Header.Event == "task-failed" || r.Header.Event == "error" {
							if r.Header.Event == "task-failed" || r.Header.Event == "error" {
								log.Printf("❌ [TTS] %s: code=%s msg=%s", r.Header.Event, r.Header.ErrorCode, r.Header.ErrorMessage)
							} else {
								log.Printf("✅ [TTS] %s (task_id=%s)", r.Header.Event, r.Header.TaskID)
							}
						}
						select {
						case ch <- r:
						default:
						}
					}
				}
			}
		}()
	}

	closeConn := func() {
		if conn == nil {
			return
		}
		_ = conn.Close()
		conn = nil
		taskID = ""
		taskStarted = false
		eventCh = nil
	}

	waitForEvent := func(ch chan ttsResp, timeout time.Duration, want string) bool {
		deadline := time.NewTimer(timeout)
		defer deadline.Stop()
		for {
			select {
			case r := <-ch:
				if r.Header.Event == "task-failed" || r.Header.Event == "error" {
					return false
				}
				if r.Header.Event == want {
					return true
				}
			case <-deadline.C:
				return false
			}
		}
	}

	ensureConn := func() bool {
		if conn != nil {
			return true
		}

		dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		headers := http.Header{"Authorization": []string{"Bearer " + DASH_API_KEY}}

		c, _, err := dialer.Dial(TTS_WS_URL, headers)
		if err != nil {
			return false
		}

		conn = c
		taskID = uuid.New().String()
		eventCh = make(chan ttsResp, 64)
		taskStarted = false
		setLastAudio(time.Time{})

		startRecv(conn, eventCh)

		runMsg := map[string]interface{}{
			"header": map[string]interface{}{
				"task_id":   taskID,
				"action":    "run-task",
				"streaming": "duplex",
			},
			"payload": map[string]interface{}{
				"task_group": "audio",
				"task":       "tts",
				"function":   "SpeechSynthesizer",
				"model":      "cosyvoice-v2",
				"parameters": map[string]interface{}{
					"text_type":   "PlainText",
					"voice":       "longhua_v2",
					"format":      "pcm",
					"sample_rate": 22050,
					"volume":      50,
					"rate":        1,
					"pitch":       1,
				},
				"input": map[string]interface{}{},
			},
		}

		if err := conn.WriteJSON(runMsg); err != nil {
			closeConn()
			return false
		}

		if !waitForEvent(eventCh, 5*time.Second, "task-started") {
			closeConn()
			return false
		}

		taskStarted = true
		return true
	}

	sendContinue := func(text string) bool {
		if conn == nil || !taskStarted {
			return false
		}
		msg := map[string]interface{}{
			"header": map[string]interface{}{
				"task_id":   taskID,
				"action":    "continue-task",
				"streaming": "duplex",
			},
			"payload": map[string]interface{}{
				"input": map[string]interface{}{
					"text": text,
				},
			},
		}
		if err := conn.WriteJSON(msg); err != nil {
			closeConn()
			return false
		}
		return true
	}

	sendFinish := func() {
		// 无论如何，END 都意味着“本次播报结束”，要释放门控
		defer ttsRelease()

		if conn == nil {
			return
		}

		finish := map[string]interface{}{
			"header": map[string]interface{}{
				"task_id":   taskID,
				"action":    "finish-task",
				"streaming": "duplex",
			},
			"payload": map[string]interface{}{
				"input": map[string]interface{}{},
			},
		}

		_ = conn.WriteJSON(finish)

		finished := waitForEvent(eventCh, 30*time.Second, "task-finished")

		deadline := time.Now().Add(6 * time.Second)
		for {
			la := getLastAudio()
			if !la.IsZero() && time.Since(la) > 900*time.Millisecond {
				break
			}
			if finished && la.IsZero() {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		closeConn()
	}

	for {
		msg, ok := <-ttsManagerChan
		if !ok {
			return
		}

		if msg == "[[END]]" {
			sendFinish()
			continue
		}

		if strings.TrimSpace(msg) == "" {
			continue
		}

		if !ensureConn() {
			continue
		}

		// 走到这里就意味着“真的开始播报/继续播报”
		ttsMarkSpeaking()

		log.Printf("🔊 [TTS]: %s", msg)
		_ = sendContinue(msg)
	}
}

// ================= 7. 播放器：audioPlayer（避免无意义的 0-length 分支导致异常 close） =================
func audioPlayer() {
	<-recordStartedChan

	doStart := func() (*exec.Cmd, io.WriteCloser) {
		c := exec.Command("aplay", "-D", "default", "-t", "raw", "-r", "22050", "-f", "S16_LE", "-c", "1", "-B", "20000")
		s, _ := c.StdinPipe()
		_ = c.Start()
		playerMutex.Lock()
		playerCmd = c
		playerStdin = s
		playerMutex.Unlock()
		return c, s
	}

	for pcmData := range audioPcmChan {
		if len(pcmData) == 0 {
			continue
		}
		if playerStdin == nil {
			doStart()
		}
		if playerStdin != nil {
			_, _ = playerStdin.Write(pcmData)
		}
	}
}

func callASRWebSocket(pcmMono16k []byte) string {
	// Paraformer 实时 WS：/api-ws/v1/inference/
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+DASH_API_KEY)

	conn, _, err := dialer.Dial(WS_AS_URL, headers)
	if err != nil {
		log.Printf("❌ [ASR] WS 连接失败: %v", err)
		return ""
	}
	defer conn.Close()

	taskID := uuid.New().String()

	// 1) run-task：task/asr + function/recognition + input:{}（必填）
	runMsg := map[string]interface{}{
		"header": map[string]interface{}{
			"task_id":   taskID,
			"action":    "run-task",
			"streaming": "duplex",
		},
		"payload": map[string]interface{}{
			"task_group": "audio",
			"task":       "asr",
			"function":   "recognition",
			"model":      "paraformer-realtime-v2",
			"parameters": map[string]interface{}{
				"format":      "pcm",
				"sample_rate": 16000,
				// 如需标点/热词等参数，按文档加在这里；先保持最小可用集
			},
			"input": map[string]interface{}{}, // 文档要求固定 {}
		},
	}

	if err := conn.WriteJSON(runMsg); err != nil {
		log.Printf("❌ [ASR] run-task 发送失败: %v", err)
		return ""
	}

	// 2) 等待 task-started（必须！）
	type wsResp struct {
		Header struct {
			TaskID       string                 `json:"task_id"`
			Event        string                 `json:"event"`
			ErrorCode    string                 `json:"error_code"`
			ErrorMessage string                 `json:"error_message"`
			Attributes   map[string]interface{} `json:"attributes"`
		} `json:"header"`
		Payload struct {
			Output struct {
				Sentence struct {
					Text        string `json:"text"`
					SentenceEnd bool   `json:"sentence_end"`
					Heartbeat   *bool  `json:"heartbeat"`
					EndTime     *int   `json:"end_time"`
				} `json:"sentence"`
				Transcription struct {
					Text        string `json:"text"`
					SentenceEnd bool   `json:"sentence_end"`
					Heartbeat   *bool  `json:"heartbeat"`
					EndTime     *int   `json:"end_time"`
				} `json:"transcription"`
			} `json:"output"`
		} `json:"payload"`
	}

	waitStartedDeadline := time.Now().Add(3 * time.Second)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, msg, rerr := conn.ReadMessage()
		if rerr != nil {
			log.Printf("❌ [ASR] 等待 task-started 失败: %v", rerr)
			return ""
		}

		var r wsResp
		if err := json.Unmarshal(msg, &r); err != nil {
			continue
		}

		switch r.Header.Event {
		case "task-started":
			// OK：可以发音频/finish-task
			goto START_STREAM
		case "task-failed":
			log.Printf("❌ [ASR] 服务端 task-failed: code=%s msg=%s raw=%s",
				r.Header.ErrorCode, r.Header.ErrorMessage, string(msg))
			return ""
		}

		if time.Now().After(waitStartedDeadline) {
			log.Printf("❌ [ASR] 等待 task-started 超时")
			return ""
		}
	}

START_STREAM:
	// 3) 发送二进制音频：建议 100ms/包 + 100ms 间隔
	// 16kHz * 0.1s = 1600 samples；PCM16 => 3200 bytes
	const frameBytes = 3200
	for i := 0; i < len(pcmMono16k); i += frameBytes {
		end := i + frameBytes
		if end > len(pcmMono16k) {
			end = len(pcmMono16k)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, pcmMono16k[i:end]); err != nil {
			log.Printf("❌ [ASR] 发送音频失败: %v", err)
			return ""
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 4) finish-task（payload.input 也必须是 {}）
	finishMsg := map[string]interface{}{
		"header": map[string]interface{}{
			"task_id":   taskID,
			"action":    "finish-task",
			"streaming": "duplex",
		},
		"payload": map[string]interface{}{
			"input": map[string]interface{}{},
		},
	}
	if err := conn.WriteJSON(finishMsg); err != nil {
		log.Printf("❌ [ASR] finish-task 发送失败: %v", err)
		return ""
	}

	// 5) 读取 result-generated，直到 task-finished
	// 取“最后一句结束(sentence_end=true)”的 text；跳过 heartbeat=true
	var finalText string
	readDeadline := time.Now().Add(12 * time.Second)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, msg, rerr := conn.ReadMessage()
		if rerr != nil {
			// 超时/关闭都可能发生；如果已有结果就返回
			if finalText != "" {
				return strings.TrimSpace(finalText)
			}
			if time.Now().After(readDeadline) {
				log.Printf("☁️ [ASR] 等待结果超时，仍为空")
				return ""
			}
			continue
		}

		var r wsResp
		if err := json.Unmarshal(msg, &r); err != nil {
			continue
		}

		switch r.Header.Event {
		case "task-failed":
			log.Printf("❌ [ASR] 服务端 task-failed: code=%s msg=%s raw=%s",
				r.Header.ErrorCode, r.Header.ErrorMessage, string(msg))
			return ""
		case "result-generated":
			// heartbeat：sentence 或 transcription 任一条可能出现
			hb := false
			if r.Payload.Output.Sentence.Heartbeat != nil {
				hb = *r.Payload.Output.Sentence.Heartbeat
			}
			if r.Payload.Output.Transcription.Heartbeat != nil {
				hb = *r.Payload.Output.Transcription.Heartbeat
			}
			if hb {
				continue
			}

			txt := strings.TrimSpace(r.Payload.Output.Sentence.Text)
			if txt == "" {
				txt = strings.TrimSpace(r.Payload.Output.Transcription.Text)
			}
			if txt != "" {
				finalText = txt
			}

		case "task-finished":
			return strings.TrimSpace(finalText)
		}

		if time.Now().After(readDeadline) {
			return strings.TrimSpace(finalText)
		}
	}
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
	files, err := ioutil.ReadDir(MUSIC_DIR)
	if err != nil {
		log.Printf("⚠️ [MUSIC] 读取目录失败: %v", err)
		return false
	}

	// 收集所有 wav
	all := make([]string, 0, 128)
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(f.Name()), ".wav") {
			all = append(all, filepath.Join(MUSIC_DIR, f.Name()))
		}
	}
	if len(all) == 0 {
		log.Printf("⚠️ [MUSIC] 目录中没有 .wav 文件: %s", MUSIC_DIR)
		return false
	}

	q := strings.TrimSpace(query)
	// 空 query => 随机
	if q == "" || strings.EqualFold(q, "RANDOM") {
		target := all[rand.Intn(len(all))]
		m.PlayFile(target)
		return true
	}

	// 非空 query => 先过滤候选集（文件名包含 query）
	nq := normalizeName(q)

	candidates := make([]string, 0, 16)
	for _, p := range all {
		base := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		if strings.Contains(normalizeName(base), nq) {
			candidates = append(candidates, p)
		}
	}

	// 没匹配到：不降级随机（否则你又会遇到“点播却随机”）
	if len(candidates) == 0 {
		log.Printf("⚠️ [MUSIC] 未找到匹配歌曲: query=%q", q)
		return false
	}

	target := candidates[rand.Intn(len(candidates))]
	m.PlayFile(target)
	return true
}

func (m *MusicManager) Duck()   { m.volMutex.Lock(); m.targetVolume = 0.2; m.volMutex.Unlock() }
func (m *MusicManager) Unduck() { m.volMutex.Lock(); m.targetVolume = 1.0; m.volMutex.Unlock() }

// ================= 新增 writeWav 辅助函数 =================
// 将 PCM 数据写入标准 WAV 文件头，方便在电脑上播放
func writeWav(filename string, data []int16, sampleRate int) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	// WAV Header
	// ChunkID "RIFF"
	f.Write([]byte("RIFF"))
	// ChunkSize (36 + data size)
	totalDataLen := len(data) * 2
	binary.Write(f, binary.LittleEndian, uint32(36+totalDataLen))
	// Format "WAVE"
	f.Write([]byte("WAVE"))

	// Subchunk1ID "fmt "
	f.Write([]byte("fmt "))
	// Subchunk1Size (16 for PCM)
	binary.Write(f, binary.LittleEndian, uint32(16))
	// AudioFormat (1 for PCM)
	binary.Write(f, binary.LittleEndian, uint16(1))
	// NumChannels (1)
	binary.Write(f, binary.LittleEndian, uint16(1))
	// SampleRate
	binary.Write(f, binary.LittleEndian, uint32(sampleRate))
	// ByteRate (SampleRate * NumChannels * BitsPerSample/8)
	binary.Write(f, binary.LittleEndian, uint32(sampleRate*2))
	// BlockAlign (NumChannels * BitsPerSample/8)
	binary.Write(f, binary.LittleEndian, uint16(2))
	// BitsPerSample (16)
	binary.Write(f, binary.LittleEndian, uint16(16))

	// Subchunk2ID "data"
	f.Write([]byte("data"))
	// Subchunk2Size
	binary.Write(f, binary.LittleEndian, uint32(totalDataLen))

	// Data
	for _, v := range data {
		binary.Write(f, binary.LittleEndian, v)
	}
	return nil
}

func isMusicPlayIntent(t string) bool {
	t = strings.TrimSpace(t)
	// 明确播放/点歌/切歌
	keys := []string{"放首歌", "放歌", "播放", "来一首", "听", "想听", "换首歌", "下一首", "切歌", "随机来一首"}
	for _, k := range keys {
		if strings.Contains(t, k) {
			return true
		}
	}
	return false
}

func isMusicStopIntent(t string) bool {
	keys := []string{"停止音乐", "暂停音乐", "别唱了", "停一下", "停止播放", "别放了"}
	for _, k := range keys {
		if strings.Contains(t, k) {
			return true
		}
	}
	return false
}

type MusicCmd int

const (
	MusicCmdNone MusicCmd = iota
	MusicCmdStop
	MusicCmdNext
	MusicCmdPlayRandom
	MusicCmdPlayQuery
)

func containsAny(s string, keys []string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// 用于匹配文件名/查询的“弱归一化”：去空白、常见标点、小写化
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	replacer := strings.NewReplacer(
		" ", "", "\t", "", "\n", "",
		"。", "", "，", "", "！", "", "？", "",
		".", "", ",", "", "!", "", "?", "",
		"《", "", "》", "", "\"", "", "'", "", "“", "", "”", "",
		"(", "", ")", "", "（", "", "）", "",
		"[", "", "]", "", "【", "", "】", "",
		"-", "", "_", "",
	)
	return replacer.Replace(s)
}

// 从用户 ASR 文本里解析“音乐控制指令”
// - 先判定 stop/next/random
// - 再抽取 “想听X/播放X/听X/放X” 的 X 作为 query
func parseMusicCmd(raw string) (MusicCmd, string) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return MusicCmdNone, ""
	}

	// 1) 明确停止（优先级最高）
	stopKeys := []string{"停止音乐", "暂停音乐", "停止播放", "别唱了", "别放了"}
	if containsAny(t, stopKeys) {
		return MusicCmdStop, ""
	}

	// 单字“停/暂停/停止”在你系统里也常用于“打断TTS”
	// 这里不直接当作 stop music，留给 processASR 做“如果正在播歌则停歌”的条件处理。

	// 2) 切歌 / 下一首
	nextKeys := []string{"换首歌", "下一首", "切歌", "换一首", "换首", "切一首"}
	if containsAny(t, nextKeys) {
		return MusicCmdNext, ""
	}

	// 3) 随机放歌
	randomKeys := []string{"随机来一首", "随机放一首", "随便来一首", "来一首", "放首歌", "放歌", "播放音乐"}
	if containsAny(t, randomKeys) {
		return MusicCmdPlayRandom, ""
	}

	// 4) 点播：想听/听/播放/放 + 内容
	// 例：我想要听心跳 / 想听 周杰伦 / 播放心跳 / 听心跳
	re := regexp.MustCompile(`(?:我想要听|我想听|想要听|想听|要听|听|播放|放)(.+)`)
	m := re.FindStringSubmatch(t)
	if len(m) > 1 {
		q := strings.TrimSpace(m[1])
		q = strings.Trim(q, "。！，,!?？ \t\r\n")
		q = strings.TrimPrefix(q, "一首")
		q = strings.TrimSpace(q)

		// “听歌/听音乐”这类泛化请求，当作随机
		if q == "" || q == "歌" || q == "音乐" || q == "一首歌" {
			return MusicCmdPlayRandom, ""
		}
		return MusicCmdPlayQuery, q
	}

	return MusicCmdNone, ""
}

// =============== 新增：flush 并返回丢弃条数（用于定位“播报被清空”） ===============
func flushChannelCount[T any](c chan T) (n int) {
	for {
		select {
		case <-c:
			n++
		default:
			return n
		}
	}
}
