package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

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

	// ================= 伪唤醒门控（最小侵入） =================
	tail, hitWake, pureWake := stripWakeAndGetTail(text)

	if !awakeFlag.Load() {
		// 休眠态：只有命中唤醒词才进入后续处理，其余任何指令都忽略
		if !hitWake {
			log.Printf("[休眠] 未检测到唤醒词，忽略: [%s]", text)
			musicMgr.Unduck()
			return
		}

		awakeFlag.Store(true)
		touchActive()

		// 纯唤醒词：播报“我在”
		if pureWake {
			log.Println("[伪唤醒] 唤醒成功")
			speakWakeAck()
			musicMgr.Unduck()
			return
		}

		// 唤醒词后携带指令：直接处理（不播“我在”）
		if strings.TrimSpace(tail) != "" {
			log.Printf("[伪唤醒] 唤醒并转入指令: [%s]", tail)
			text = tail
		} else {
			// 理论不会出现：pureWake=false 但 tail 为空；兜底不改原 text
			log.Printf("[伪唤醒] 唤醒命中但未解析到后续指令，按原文处理: [%s]", text)
		}
	} else {
		// 唤醒态：刷新活跃时间；若仅唤醒词则回应“我在”，若携带指令则剥离后继续处理
		touchActive()
		if hitWake {
			if pureWake {
				log.Println("[伪唤醒] 收到唤醒词")
				speakWakeAck()
				musicMgr.Unduck()
				return
			}
			if strings.TrimSpace(tail) != "" && tail != text {
				text = tail
			}
		}
	}

	log.Printf("ASR识别结果: [%s]", text)

	// 1. 二级打断：退出判定
	if isExit(text) {
		log.Println("收到退出指令，关闭系统")
		performStop()
		os.Exit(0)
	}

	// 2. 获取物理占用状态
	playerMutex.Lock()
	isTtsBusy := playerCmd != nil && playerCmd.Process != nil
	playerMutex.Unlock()
	isMusicBusy := musicMgr.IsPlaying()

	// 2.5 音量指令处理（不打断播报/音乐）
	if handleVolumeCommand(text, isTtsBusy, isMusicBusy) {
		return
	}

	// 3. 核心改进：忙碌状态下的穿透逻辑
	if isTtsBusy || isMusicBusy {
		musicReq := hasMusicIntent(text)
		quickSwitch := isMusicBusy && isQuickSwitch(text)

		// 允许打断词或点歌意图“穿透”锁定
		if isInterrupt(text) || musicReq || quickSwitch {
			log.Printf("忙碌穿透: 指令 [%s] 合法，执行物理清理并重置意图", text)
			performStop()

			// 如果只是纯粹的“换一首/切歌”且不包含具体歌名，直接执行随机播放并返回
			// 这样可以避免 LLM 推理的延迟
			if quickSwitch {
				log.Printf("快速切歌触发: text=%q", text)
				musicMgr.SearchAndPlay("RANDOM")
				return
			}

			// 如果是“听庙堂之外”，执行完 performStop 后不 return，
			// 而是继续往下走，交给 LLM 解析出 [PLAY:庙堂之外]
		} else {
			// 真正的无关闲聊，在忙碌时依然拦截
			log.Printf("锁定拦截: 忽略非控制类指令: [%s]", text)
			musicMgr.Unduck()
			return
		}
	}

	// 4. 联网搜索判定
	enableSearch := false
	searchKeywords := []string{"天气", "今天", "星期几", "实时", "最新"}
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

func callASRWebSocket(data []byte) string {
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+dashAPIKey)
	conn, _, err := dialer.Dial(asrWsURL, headers)
	if err != nil {
		return ""
	}
	defer conn.Close()
	id := fmt.Sprintf("%032x", rand.Int63())
	conn.WriteJSON(map[string]interface{}{
		"header":  map[string]interface{}{"task_id": id, "action": "run-task", "streaming": "duplex"},
		"payload": map[string]interface{}{"task_group": "audio", "task": "asr", "function": "recognition", "model": asrModel, "parameters": map[string]interface{}{"format": "pcm", "sample_rate": asrSampleRate}, "input": map[string]interface{}{}},
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
