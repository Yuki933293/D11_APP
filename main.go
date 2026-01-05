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
	"math"
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

// ================= 1. 常量配置 =================
const DASH_API_KEY = "sk-fb64515c017945fc9282f9ace355cad3"

const TTS_WS_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"
const LLM_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text-generation/generation"
const WS_AS_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"

const MUSIC_DIR = "/userdata/AI_BOX/music"

// ================= 2. 双级打断词库 =================
var EXIT_WORDS = []string{
	"关闭系统", "关机", "退出程序", "再见", "退下",
	"拜拜", "结束吧", "结束程序", "停止运行", "关闭助手", "关闭",
}

var INTERRUPT_WORDS = []string{
	"闭嘴", "停止", "安静", "别说了", "暂停", "打断",
	"别唱了", "等一下", "换首歌", "下一首", "切歌", "不要说了",
}

// ================= 3. 并发控制与状态变量 =================
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

	emojiRegex *regexp.Regexp
	musicPunct = regexp.MustCompile(`[，。！？,.!?\s；;：:“”"'《》()（）【】\[\]、]`)
	musicMgr   *MusicManager
)

// ================= 4. 性能监控辅助变量 =================
var (
	tsLlmStart   time.Time
	tsTtsStart   time.Time
	tsFirstAudio time.Time
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
	log.Println("=== RK3308 AI 助手 (V160.21 物理资源锁定版) ===")

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
		// Duck 要“明显且快速”：先把目标压到 20%，并把当前音量立即拉到一个较低上限，
		// 避免因为缓慢平滑导致用户听感“没有降音量”。
		m.volMutex.Lock()
		m.targetVolume = 0.2
		if m.currentVolume > 0.35 {
			m.currentVolume = 0.35
		}
		m.volMutex.Unlock()
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

	// -B 是缓冲时间(us)：太小会在 CPU 抖动时 underrun（卡顿），太大会导致 Duck/切歌响应滞后。
	// 这里取一个折中值，配合下游“前置缓冲”控制，保证不卡顿且仍可及时 Duck。
	cmd := exec.Command("aplay", "-D", "default", "-q", "-t", "raw", "-r", "16000", "-c", "1", "-f", "S16_LE", "-B", "80000")
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
		// 关键：
		// - 不能“严格实时”地喂数据（每 20ms sleep 一次），否则在 RK3308 上只要调度抖动就会 underrun（听感卡顿）。
		// - 也不能一次性喂太快/太多，否则 Duck 的听感会滞后（因为旧音量的音频已经预灌进 aplay/管道）。
		//
		// 策略：维护一个小的“前置缓冲”（例如 120~180ms），既抗抖动又保证 Duck 仍然足够跟手。
		const (
			musicSampleRate = 16000
			chunkSamples    = 640 // 40ms：降低调度开销，同时仍有较好音量跟随
			targetAhead     = 120 * time.Millisecond
			maxAhead        = 180 * time.Millisecond
		)
		buf := make([]byte, chunkSamples*2)

		var (
			startWall    time.Time
			wroteSamples int64
			lastStepAt   time.Time
		)
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			n, err := f.Read(buf)
			if n > 0 {
				if startWall.IsZero() {
					startWall = time.Now()
					lastStepAt = startWall
				}

				// 读取当前目标音量，并按 dt 做平滑逼近（Duck 快、Unduck 慢）
				now := time.Now()
				dt := now.Sub(lastStepAt)
				lastStepAt = now

				m.volMutex.Lock()
				target := m.targetVolume
				current := m.currentVolume
				m.volMutex.Unlock()

				if target < 0 {
					target = 0
				} else if target > 1 {
					target = 1
				}
				if current < 0 {
					current = 0
				} else if current > 1 {
					current = 1
				}

				if dt <= 0 {
					current = target
				} else if current != target {
					var tau time.Duration
					if target < current {
						tau = 120 * time.Millisecond
					} else {
						tau = 900 * time.Millisecond
					}
					alpha := 1 - math.Exp(-float64(dt)/float64(tau))
					if alpha < 0 {
						alpha = 0
					} else if alpha > 1 {
						alpha = 1
					}
					current = current + (target-current)*alpha
				}

				m.volMutex.Lock()
				m.currentVolume = current
				m.volMutex.Unlock()

				// PCM16 振幅缩放 + 饱和裁剪
				for i := 0; i+1 < n; i += 2 {
					sample := int16(binary.LittleEndian.Uint16(buf[i : i+2]))
					v := int(float64(sample) * current)
					if v > 32767 {
						v = 32767
					} else if v < -32768 {
						v = -32768
					}
					binary.LittleEndian.PutUint16(buf[i:i+2], uint16(int16(v)))
				}

				if _, werr := pipe.Write(buf[:n]); werr != nil {
					return
				}

				// 维护“前置缓冲”：若已写入的音频时长领先于墙钟太多，则主动 sleep 让播放追上来。
				wroteSamples += int64(n / 2)
				audioDur := time.Duration(wroteSamples) * time.Second / musicSampleRate
				ahead := audioDur - time.Since(startWall)
				if ahead > maxAhead {
					sleepDur := ahead - targetAhead
					if sleepDur > 0 {
						select {
						case <-stopCh:
							return
						case <-time.After(sleepDur):
						}
					}
				}
			}

			if err != nil {
				break
			}
		}
		m.mu.Lock()
		if m.isPlaying && m.cmd == myCmd {
			m.isPlaying = false
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

func audioPlayer() {
	doStart := func() (*exec.Cmd, io.WriteCloser) {
		log.Println("🔍 [Audio-Link] 启动 aplay 物理进程...")
		c := exec.Command("aplay", "-D", "default", "-t", "raw", "-r", "22050", "-f", "S16_LE", "-c", "1", "-B", "20000")
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
			log.Println("🔍 [Audio-Link] 收到数据结束标志，执行物理保活...")
			time.Sleep(500 * time.Millisecond)
			if playerStdin != nil {
				playerStdin.Close()
			}
			if playerCmd != nil {
				go func(c *exec.Cmd) {
					if c != nil {
						_ = c.Wait()
					}
					playerMutex.Lock()
					playerCmd = nil
					playerStdin = nil
					playerMutex.Unlock()
					log.Println("✅ [Audio-Link] 物理播报完成，系统解锁")
				}(playerCmd)
			}
			continue
		}

		if playerStdin == nil {
			doStart()
		}
		if playerStdin != nil {
			_, err := playerStdin.Write(pcmData)
			if err != nil {
				playerMutex.Lock()
				playerCmd = nil
				playerStdin = nil
				playerMutex.Unlock()
			}
		}
	}
}

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
			event := header["event"].(string)
			if event == "task-started" {
				select {
				case taskStartedSignal <- struct{}{}:
				default:
				}
			}
			if event == "task-finished" || event == "task-failed" {
				return
			}
		}
	}

	for {
		msg, ok := <-ttsManagerChan
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

		if msg == "[[END]]" {
			if conn != nil {
				conn.WriteJSON(map[string]interface{}{
					"header":  map[string]interface{}{"task_id": currentTaskID, "action": "finish-task", "streaming": "duplex"},
					"payload": map[string]interface{}{"input": map[string]interface{}{}},
				})
				wg.Wait()
				conn.Close()
				conn = nil
			}
			continue
		}

		if strings.TrimSpace(msg) != "" {
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
				wg.Add(1)
				go receiveLoop(conn)
				conn.WriteJSON(map[string]interface{}{
					"header": map[string]interface{}{"task_id": currentTaskID, "action": "run-task", "streaming": "duplex"},
					"payload": map[string]interface{}{
						"task_group": "audio", "task": "tts", "function": "SpeechSynthesizer",
						"model":      "cosyvoice-v3-plus",
						"parameters": map[string]interface{}{"text_type": "PlainText", "voice": "longanhuan", "format": "pcm", "sample_rate": 22050, "volume": 50, "enable_ssml": false},
						"input":      map[string]interface{}{},
					},
				})
				select {
				case <-taskStartedSignal:
					time.Sleep(50 * time.Millisecond)
				case <-time.After(5 * time.Second):
					conn.Close()
					conn = nil
					continue
				}
			}
			conn.WriteJSON(map[string]interface{}{
				"header":  map[string]interface{}{"task_id": currentTaskID, "action": "continue-task", "streaming": "duplex"},
				"payload": map[string]interface{}{"input": map[string]interface{}{"text": msg}},
			})
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func callAgentStream(ctx context.Context, prompt string, enableSearch bool) {
	flushChannel(ttsManagerChan)
	llmStart := time.Now()

	// 策略：联网搜索用 Max(准确但慢)，普通闲聊用 Turbo(极快)
	modelName := "qwen-turbo-latest"
	if enableSearch {
		modelName = "qwen-max"
		log.Println("🌐 [LLM]: 检测到时效性需求，已动态开启联网搜索...")
	}

	systemPrompt := "你是智能助手。仅在用户【明确要求播放音乐】（如“放首歌”、“听周杰伦”）时，才在回复末尾添加 [PLAY: 歌名]（随机播放用 [PLAY: RANDOM]）。" +
		"如果用户要求停止，加上 [STOP]。" +
		"回答天气、新闻、闲聊等普通问题时，【严禁】添加任何播放指令。"
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
			"enable_search":      enableSearch, // 动态开关
		},
	}

	jsonBody, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", LLM_URL, bytes.NewReader(jsonBody))
	req.Header.Set("Authorization", "Bearer "+DASH_API_KEY)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := insecureClient.Do(req)
	if err != nil {
		log.Printf("❌ [LLM]: 请求失败: %v", err)
		musicMgr.Unduck()
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var fullTextBuilder strings.Builder
	var chunkBuffer strings.Builder
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

			// 动态调整首包断句阈值：联网搜索时降低阈值以减少用户焦虑
			threshold := 30
			if enableSearch {
				threshold = 15 // 搜索时只要有15个字或标点就立刻播报
			}

			if !firstChunkSent {
				if strings.ContainsAny(clean, "，。！？,.!?\n") || chunkBuffer.Len() > threshold {
					firstChunkSent = true
					sendChunk(&chunkBuffer)
				}
			} else {
				if strings.ContainsAny(clean, "，。！？,.!?\n") || chunkBuffer.Len() > 80 {
					sendChunk(&chunkBuffer)
				}
			}
		}
	}
	fmt.Println()
	log.Printf("⏱️ [性能] LLM 推理结束，总耗时: %v", time.Since(llmStart))

	// 处理剩余文本
	sendChunk(&chunkBuffer)
	ttsManagerChan <- "[[END]]"

	// 指令解析逻辑
	fullText := fullTextBuilder.String()
	if strings.Contains(fullText, "[STOP]") {
		musicMgr.Stop()
	}
	if matches := regexp.MustCompile(`(?i)\[PLAY:\s*(.*?)\]`).FindStringSubmatch(fullText); len(matches) > 1 {
		musicMgr.SearchAndPlay(strings.TrimSpace(matches[1]))
	}
}

// 辅助函数：发送文本块到 TTS
func sendChunk(buf *strings.Builder) {
	text := regexp.MustCompile(`\[.*?\]`).ReplaceAllString(buf.String(), "")
	if strings.TrimSpace(text) != "" {
		ttsManagerChan <- strings.TrimSpace(text)
	}
	buf.Reset()
}

func performStop() {
	log.Println("🧹 [物理清理]: 强制切断所有声音源")
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

// 辅助判定：ASR 文本是否包含明确的点歌/换歌意图
func hasMusicIntent(text string) bool {
	// 包含这些动词通常意味着用户想操作音乐
	musicKeywords := []string{"播放", "点歌", "想要听", "要听", "唱一首", "换一首", "切歌", "下一首", "来一首"}
	for _, k := range musicKeywords {
		if strings.Contains(text, k) {
			return true
		}
	}
	return false
}

func processASR(pcm []int16) {
	if float64(len(pcm))/16000.0 < 0.5 {
		return
	}

	pcmBytes := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(v))
	}
	text := callASRWebSocket(pcmBytes)
	if text == "" {
		musicMgr.Unduck()
		return
	}
	log.Printf("✅ [ASR识别结果]: [%s]", text)

	// 1. 二级打断：退出判定
	if isExit(text) {
		log.Println("💀 收到退出指令，关闭系统")
		performStop()
		os.Exit(0)
	}

	// 2. 获取物理占用状态
	playerMutex.Lock()
	isTtsBusy := playerCmd != nil && playerCmd.Process != nil
	playerMutex.Unlock()
	isMusicBusy := musicMgr.IsPlaying()

	// 3. 核心改进：忙碌状态下的穿透逻辑
	if isTtsBusy || isMusicBusy {
		musicReq := hasMusicIntent(text)

		// 允许打断词或点歌意图“穿透”锁定
		if isInterrupt(text) || musicReq {
			log.Printf("🛑 [忙碌穿透]: 指令 [%s] 合法，执行物理清理并重置意图", text)
			performStop()

			// 如果只是纯粹的“换一首/切歌”且不包含具体歌名，直接执行随机播放并返回
			// 这样可以避免 LLM 推理的延迟
			isQuickSwitch := (strings.Contains(text, "换") || strings.Contains(text, "下") || strings.Contains(text, "切")) &&
				!strings.Contains(text, "播放") && !strings.Contains(text, "听")

			if isQuickSwitch {
				musicMgr.SearchAndPlay("RANDOM")
				return
			}

			// 如果是“听庙堂之外”，执行完 performStop 后不 return，
			// 而是继续往下走，交给 LLM 解析出 [PLAY:庙堂之外]
		} else {
			// 真正的无关闲聊，在忙碌时依然拦截
			log.Printf("🙉 [锁定拦截]: 忽略非控制类指令: [%s]", text)
			musicMgr.Unduck()
			return
		}
	}

	// 4. 联网搜索判定
	enableSearch := false
	searchKeywords := []string{"天气", "新闻", "今天", "几号", "星期几", "实时", "最新", "温度"}
	for _, k := range searchKeywords {
		if strings.Contains(text, k) {
			enableSearch = true
			break
		}
	}

	// 5. 开启会话并执行 LLM 推理
	ctxMutex.Lock()
	if sessionCancel != nil {
		sessionCancel()
	}
	sessionCtx, sessionCancel = context.WithCancel(context.Background())
	currentCtx := sessionCtx
	ctxMutex.Unlock()

	go callAgentStream(currentCtx, text, enableSearch)
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
	vadAccumulator := make([]int16, 0, 1024)
	var asrBuffer []int16
	silenceCount, speechCount := 0, 0
	triggered := false
	ducked := false
	fallbackMono := make([]int16, 256)

	for {
		if _, err := io.ReadFull(stdout, readBuf); err != nil {
			break
		}
		rawInt16 := make([]int16, 256*10)
		for i := 0; i < len(rawInt16); i++ {
			rawInt16[i] = int16(binary.LittleEndian.Uint16(readBuf[i*2 : i*2+2]))
		}
		clean, _ := aecProc.Process(rawInt16)
		if clean == nil {
			// AEC 异常回退：取第 0 通道直通，避免整段音频被丢弃导致“说了却识别不到”
			for i := 0; i < 256; i++ {
				fallbackMono[i] = rawInt16[i*10+0]
			}
			clean = fallbackMono
		}
		vadAccumulator = append(vadAccumulator, clean...)

		for len(vadAccumulator) >= 320 {
			frame := vadAccumulator[:320]
			vadAccumulator = vadAccumulator[320:]
			vadBuf := make([]byte, 640)
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

			// 先快速 Duck（听感上立刻压低背景音），再决定是否进入 ASR 录音段
			if speechCount > 2 && !ducked {
				ducked = true
				musicMgr.Duck()
			}

			if speechCount > 10 && !triggered {
				triggered = true
			}
			if triggered {
				asrBuffer = append(asrBuffer, frame...)
				if silenceCount > 10 || len(asrBuffer) > 16000*8 {
					if len(asrBuffer) > 4800 {
						finalData := make([]int16, len(asrBuffer))
						copy(finalData, asrBuffer)
						go processASR(finalData)
					} else {
						musicMgr.Unduck()
					}
					asrBuffer = []int16{}
					triggered = false
					ducked = false
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
	conn, _, err := dialer.Dial(WS_AS_URL, headers)
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
