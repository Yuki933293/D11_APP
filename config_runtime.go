package main

import (
	"bufio"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// ================= 配置加载（最小改动方案） =================
// 目标：新设备一键部署时，不需要改代码/重编译，只需要下发一个 .env 配置文件。
// 约定：
// - 环境变量优先级最高；
// - 若未显式设置环境变量，则尝试加载 env 文件（AI_BOX_ENV_FILE 或默认路径）；
// - 程序内部只关心与运行相关的字段，其余字段（例如 WiFi）由安装脚本读取即可。

var (
	// DashScope
	dashAPIKey string

	// 云端接口
	ttsWsURL = TTS_WS_URL
	llmURL   = LLM_URL
	asrWsURL = WS_AS_URL

	// 模型配置
	llmModelFast   = "qwen-turbo-latest"
	llmModelSearch = "qwen-max"
	ttsModel       = "cosyvoice-v1"
	ttsVoice       = "longwan"
	ttsSampleRate  = 22050
	ttsVolume      = 50
	asrModel       = "paraformer-realtime-v2"
	asrSampleRate  = 16000

	// 路径
	musicDir = MUSIC_DIR

	// 录音参数（默认与现有逻辑一致）
	arecordDevice     = "hw:2,0"
	arecordChannels   = 10
	arecordRate       = 16000
	arecordPeriodSize = 256
	arecordBufferSize = 16384

	// 伪唤醒参数
	wakeIdleTimeout = WAKE_IDLE_TIMEOUT
	wakeAckText     = WAKE_ACK_TEXT
)

func initRuntimeConfig() {
	loadedEnv, err := loadEnvFileFromCandidates()
	if err != nil {
		log.Printf("⚠️ [配置] 读取 env 文件失败: %v", err)
	} else if loadedEnv != "" {
		log.Printf("🔧 [配置] 已加载 env 文件: %s", loadedEnv)
	}

	// API Key：支持两种变量名，方便迁移
	dashAPIKey = strings.TrimSpace(os.Getenv("AI_BOX_DASH_API_KEY"))
	if dashAPIKey == "" {
		dashAPIKey = strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY"))
	}
	if dashAPIKey == "" {
		dashAPIKey = strings.TrimSpace(DASH_API_KEY)
	}
	if dashAPIKey == "" {
		log.Fatal("❌ [配置] 未配置 DashScope API Key：请在 env 文件中设置 AI_BOX_DASH_API_KEY（参考 deploy/ai_box.env.example）")
	}

	ttsWsURL = getEnv("AI_BOX_TTS_WS_URL", ttsWsURL)
	llmURL = getEnv("AI_BOX_LLM_URL", llmURL)
	asrWsURL = getEnv("AI_BOX_ASR_WS_URL", asrWsURL)

	llmModelFast = getEnv("AI_BOX_LLM_MODEL_FAST", llmModelFast)
	llmModelSearch = getEnv("AI_BOX_LLM_MODEL_SEARCH", llmModelSearch)

	ttsModel = getEnv("AI_BOX_TTS_MODEL", ttsModel)
	ttsVoice = getEnv("AI_BOX_TTS_VOICE", ttsVoice)
	ttsSampleRate = getEnvInt("AI_BOX_TTS_SAMPLE_RATE", ttsSampleRate)
	ttsVolume = getEnvInt("AI_BOX_TTS_VOLUME", ttsVolume)

	asrModel = getEnv("AI_BOX_ASR_MODEL", asrModel)
	asrSampleRate = getEnvInt("AI_BOX_ASR_SAMPLE_RATE", asrSampleRate)

	musicDir = getEnv("AI_BOX_MUSIC_DIR", musicDir)

	arecordDevice = getEnv("AI_BOX_ARECORD_DEVICE", arecordDevice)
	arecordChannels = getEnvInt("AI_BOX_ARECORD_CHANNELS", arecordChannels)
	arecordRate = getEnvInt("AI_BOX_ARECORD_RATE", arecordRate)
	arecordPeriodSize = getEnvInt("AI_BOX_ARECORD_PERIOD_SIZE", arecordPeriodSize)
	arecordBufferSize = getEnvInt("AI_BOX_ARECORD_BUFFER_SIZE", arecordBufferSize)

	wakeAckText = getEnv("AI_BOX_WAKE_ACK_TEXT", wakeAckText)
	wakeIdleTimeout = getEnvDuration("AI_BOX_WAKE_IDLE_TIMEOUT", wakeIdleTimeout)

	if s := strings.TrimSpace(os.Getenv("AI_BOX_WAKE_WORDS")); s != "" {
		words := splitList(s)
		if len(words) > 0 {
			WAKE_WORDS = words
		}
	}

	log.Printf("🔧 [配置] LLM(fast=%s search=%s) | ASR(model=%s) | TTS(model=%s voice=%s) | musicDir=%s | wakeIdle=%s",
		llmModelFast, llmModelSearch, asrModel, ttsModel, ttsVoice, musicDir, wakeIdleTimeout)
}

func loadEnvFileFromCandidates() (string, error) {
	if p := strings.TrimSpace(os.Getenv("AI_BOX_ENV_FILE")); p != "" {
		if err := loadEnvFile(p); err != nil {
			return "", err
		}
		return p, nil
	}

	candidates := []string{
		"/userdata/AI_BOX/ai_box.env",
		"./ai_box.env",
	}
	for _, p := range candidates {
		if err := loadEnvFile(p); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
		return p, nil
	}
	return "", nil
}

// loadEnvFile 读取 KEY=VALUE 配置并写入到进程环境变量（仅在对应 key 未设置时才写入）。
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key == "" {
			continue
		}
		if _, ok := os.LookupEnv(key); ok {
			continue
		}
		val = unquoteEnvValue(val)
		_ = os.Setenv(key, val)
	}
	return scanner.Err()
}

func unquoteEnvValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		u, err := strconv.Unquote(v)
		if err == nil {
			return u
		}
		return strings.Trim(v, "\"")
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}

func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func splitList(s string) []string {
	s = strings.ReplaceAll(s, "，", ",")
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
