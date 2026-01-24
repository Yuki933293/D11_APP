package main

import (
	"context"
	"crypto/tls"
	"log"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	vado "github.com/maxhawkins/go-webrtc-vad"

	"ai_box/aec"
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

	// 一键部署配置加载（环境变量优先，其次读取 env 文件）
	initRuntimeConfig()

	ttsManagerChan = make(chan string, 500)
	audioPcmChan = make(chan []byte, 4000)

	sessionCtx, sessionCancel = context.WithCancel(context.Background())
	currentSessionID = uuid.New().String()

	musicMgr = NewMusicManager()

	awakeFlag.Store(false)
	lastActiveUnixNano.Store(0)
	log.Println("😴 [伪唤醒] 初始为休眠态，仅响应唤醒词（例如：你好小瑞）")

	go audioPlayer()
	go ttsManagerLoop()
	go wakeIdleMonitor()

	aecProc := aec.NewProcessor()
	vadEng, err := vado.New()
	if err != nil {
		log.Fatal("❌ VAD 初始化失败:", err)
	}
	vadEng.SetMode(3)

	go audioLoop(aecProc, vadEng)

	select {}
}
