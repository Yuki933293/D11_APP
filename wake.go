package main

import (
	"log"
	"strings"
	"time"
)

func touchActive() {
	lastActiveUnixNano.Store(time.Now().UnixNano())
}

func normalizeWakeText(text string) string {
	// 去掉标点/空白，便于匹配“你好，小瑞”等变体
	s := strings.ToLower(strings.TrimSpace(text))
	s = musicPunct.ReplaceAllString(s, "")
	return s
}

// stripWakeAndGetTail 解析“唤醒词 + 后续指令”：
// - 命中唤醒词且后续为空：pureWake=true
// - 命中唤醒词且后续非空：返回 tail（尽量取唤醒词之后的原始文本）
// - 未命中：hit=false
func stripWakeAndGetTail(text string) (tail string, hit bool, pureWake bool) {
	normalized := normalizeWakeText(text)
	for _, w := range WAKE_WORDS {
		nw := normalizeWakeText(w)
		idx := strings.Index(normalized, nw)
		if idx < 0 {
			continue
		}

		// 以“唤醒词之后”的内容来判断是否还有指令（避免把唤醒词前的噪声/口头禅当成指令）
		tailNorm := strings.TrimSpace(normalized[idx+len(nw):])
		if tailNorm == "" {
			return "", true, true
		}

		// 尽量从原始文本中截取“唤醒词之后”的指令
		if pos := strings.Index(text, w); pos >= 0 {
			rawTail := strings.TrimSpace(text[pos+len(w):])
			rawTail = strings.TrimSpace(musicPunct.ReplaceAllString(rawTail, ""))
			if rawTail != "" {
				return rawTail, true, false
			}
		}

		// 若无法可靠剥离（例如中间被插入标点/空格），退化为把原文本交给后续意图处理
		return text, true, false
	}
	return "", false, false
}

func speakWakeAck() {
	// 仅唤醒词时不走 LLM，直接云端 TTS 播报一句“我在”
	flushChannel(ttsManagerChan)
	ttsManagerChan <- wakeAckText
	ttsManagerChan <- "[[END]]"
}

func isPhysicalBusy() bool {
	playerMutex.Lock()
	isTtsBusy := playerCmd != nil && playerCmd.Process != nil
	playerMutex.Unlock()
	isMusicBusy := false
	if musicMgr != nil {
		isMusicBusy = musicMgr.IsPlaying()
	}
	return isTtsBusy || isMusicBusy
}

func wakeIdleMonitor() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if !awakeFlag.Load() {
			continue
		}
		// 正在播报/放歌时不进入休眠，避免“音乐无人能停”的体验
		if isPhysicalBusy() {
			continue
		}

		last := time.Unix(0, lastActiveUnixNano.Load())
		if last.IsZero() {
			continue
		}
		if time.Since(last) <= wakeIdleTimeout {
			continue
		}

		awakeFlag.Store(false)
		log.Println("😴 [伪唤醒] 长时间无交互，进入休眠态，等待唤醒词...")
	}
}
