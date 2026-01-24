package main

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
)

var (
	volumeSetRe         = regexp.MustCompile(`音量\s*调到\s*([0-9]{1,3})`)
	volumeSetCnRe       = regexp.MustCompile(`音量\s*调到\s*([一二三四五六七八九十百两零〇]{1,4})`)
	volumeIncreaseRe    = regexp.MustCompile(`(音量|声音).*(调大|调高|增大|提高)\s*([0-9]{1,3})`)
	volumeDecreaseRe    = regexp.MustCompile(`(音量|声音).*(调小|调低|降低|减小)\s*([0-9]{1,3})`)
	volumeIncreaseRe2   = regexp.MustCompile(`(调大|调高|增大|提高).*(音量|声音)\s*([0-9]{1,3})`)
	volumeDecreaseRe2   = regexp.MustCompile(`(调小|调低|降低|减小).*(音量|声音)\s*([0-9]{1,3})`)
	volumeIncreaseCnRe  = regexp.MustCompile(`(音量|声音).*(调大|调高|增大|提高)\s*([一二三四五六七八九十百两零〇]{1,4})`)
	volumeDecreaseCnRe  = regexp.MustCompile(`(音量|声音).*(调小|调低|降低|减小)\s*([一二三四五六七八九十百两零〇]{1,4})`)
	volumeIncreaseCnRe2 = regexp.MustCompile(`(调大|调高|增大|提高).*(音量|声音)\s*([一二三四五六七八九十百两零〇]{1,4})`)
	volumeDecreaseCnRe2 = regexp.MustCompile(`(调小|调低|降低|减小).*(音量|声音)\s*([一二三四五六七八九十百两零〇]{1,4})`)
)

// 处理语音音量指令：
// - "音量调到 40"
// - "音量加大一点 / 小一点"
// - "声音大点 / 小点"
func handleVolumeCommand(text string, isTtsBusy bool, isMusicBusy bool) bool {
	normalized := strings.TrimSpace(text)
	if normalized == "" {
		return false
	}

	if percent, ok := parseVolumeSetPercent(normalized); ok {
		if err := SetDeviceVolumePercent(percent); err != nil {
			log.Printf("音量调节失败: %v", err)
		} else {
			log.Printf("音量调节: 设置为 %d%%", percent)
		}
		maybeSpeakVolumeAck(isTtsBusy, isMusicBusy, fmt.Sprintf("好的，音量已调到%d%%", percent))
		musicMgr.Unduck()
		return true
	}

	if percent, up, down, ok := parseVolumeAdjustPercent(normalized); ok {
		if up {
			if err := AdjustDeviceVolumeByPercent(percent, true); err != nil {
				log.Printf("音量调节失败: %v", err)
			} else {
				log.Printf("音量调节: 加大 %d%%", percent)
			}
			maybeSpeakVolumeAck(isTtsBusy, isMusicBusy, fmt.Sprintf("好的，音量已调大%d%%", percent))
			musicMgr.Unduck()
			return true
		}
		if down {
			if err := AdjustDeviceVolumeByPercent(percent, false); err != nil {
				log.Printf("音量调节失败: %v", err)
			} else {
				log.Printf("音量调节: 调小 %d%%", percent)
			}
			maybeSpeakVolumeAck(isTtsBusy, isMusicBusy, fmt.Sprintf("好的，音量已调小%d%%", percent))
			musicMgr.Unduck()
			return true
		}
	}

	if up, down, ok := parseVolumeAdjust(normalized); ok {
		if up {
			if err := AdjustDeviceVolumeStep(true); err != nil {
				log.Printf("音量调节失败: %v", err)
			} else {
				log.Printf("音量调节: 加大 %d%%", volumeStepPercent)
			}
			maybeSpeakVolumeAck(isTtsBusy, isMusicBusy, fmt.Sprintf("好的，音量已调大%d%%", volumeStepPercent))
			musicMgr.Unduck()
			return true
		}
		if down {
			if err := AdjustDeviceVolumeStep(false); err != nil {
				log.Printf("音量调节失败: %v", err)
			} else {
				log.Printf("音量调节: 调小 %d%%", volumeStepPercent)
			}
			maybeSpeakVolumeAck(isTtsBusy, isMusicBusy, fmt.Sprintf("好的，音量已调小%d%%", volumeStepPercent))
			musicMgr.Unduck()
			return true
		}
	}
	return false
}

func parseVolumeSetPercent(text string) (int, bool) {
	matches := volumeSetRe.FindStringSubmatch(text)
	if len(matches) < 2 {
		if cm := volumeSetCnRe.FindStringSubmatch(text); len(cm) >= 2 {
			if n, ok := parseNumberToken(cm[1]); ok {
				return clampPercent(n), true
			}
		}
		return 0, false
	}
	n, ok := parseNumberToken(matches[1])
	if !ok {
		return 0, false
	}
	return clampPercent(n), true
}

func parseVolumeAdjustPercent(text string) (percent int, up bool, down bool, ok bool) {
	percent = 0
	if m := volumeIncreaseRe.FindStringSubmatch(text); len(m) >= 4 {
		if n, ok := parseNumberToken(m[3]); ok {
			return clampPercent(n), true, false, true
		}
	}
	if m := volumeIncreaseRe2.FindStringSubmatch(text); len(m) >= 4 {
		if n, ok := parseNumberToken(m[3]); ok {
			return clampPercent(n), true, false, true
		}
	}
	if m := volumeDecreaseRe.FindStringSubmatch(text); len(m) >= 4 {
		if n, ok := parseNumberToken(m[3]); ok {
			return clampPercent(n), false, true, true
		}
	}
	if m := volumeDecreaseRe2.FindStringSubmatch(text); len(m) >= 4 {
		if n, ok := parseNumberToken(m[3]); ok {
			return clampPercent(n), false, true, true
		}
	}
	if m := volumeIncreaseCnRe.FindStringSubmatch(text); len(m) >= 4 {
		if n, ok := parseNumberToken(m[3]); ok {
			return clampPercent(n), true, false, true
		}
	}
	if m := volumeIncreaseCnRe2.FindStringSubmatch(text); len(m) >= 4 {
		if n, ok := parseNumberToken(m[3]); ok {
			return clampPercent(n), true, false, true
		}
	}
	if m := volumeDecreaseCnRe.FindStringSubmatch(text); len(m) >= 4 {
		if n, ok := parseNumberToken(m[3]); ok {
			return clampPercent(n), false, true, true
		}
	}
	if m := volumeDecreaseCnRe2.FindStringSubmatch(text); len(m) >= 4 {
		if n, ok := parseNumberToken(m[3]); ok {
			return clampPercent(n), false, true, true
		}
	}
	return 0, false, false, false
}

func parseVolumeAdjust(text string) (up bool, down bool, ok bool) {
	if !(strings.Contains(text, "音量") || strings.Contains(text, "声音")) {
		return false, false, false
	}
	if strings.Contains(text, "增大音量") || strings.Contains(text, "音量调高") || strings.Contains(text, "音量调大") ||
		strings.Contains(text, "调大音量") || strings.Contains(text, "音量调高") || strings.Contains(text, "调高音量") ||
		strings.Contains(text, "声音调大") || strings.Contains(text, "调大声音") || strings.Contains(text, "增大声音") ||
		strings.Contains(text, "调高") || strings.Contains(text, "提高") || strings.Contains(text, "调大") ||
		strings.Contains(text, "增大") || strings.Contains(text, "加大") || strings.Contains(text, "大点") {
		return true, false, true
	}
	if strings.Contains(text, "降低音量") || strings.Contains(text, "音量调低") || strings.Contains(text, "音量调小") ||
		strings.Contains(text, "音量减小") ||
		strings.Contains(text, "调低音量") || strings.Contains(text, "调小音量") || strings.Contains(text, "降低声音") ||
		strings.Contains(text, "声音调小") || strings.Contains(text, "调小声音") || strings.Contains(text, "声音调低") ||
		strings.Contains(text, "调低声音") || strings.Contains(text, "减小声音") ||
		strings.Contains(text, "调低声音") || strings.Contains(text, "降低") || strings.Contains(text, "调低") ||
		strings.Contains(text, "调小") || strings.Contains(text, "减小") || strings.Contains(text, "小一点") || strings.Contains(text, "小点") {
		return false, true, true
	}
	return false, false, false
}

// 若当前正在播报或播放音乐，不抢占播报；否则给出简短确认
func maybeSpeakVolumeAck(isTtsBusy bool, isMusicBusy bool, ack string) {
	if isTtsBusy || isMusicBusy {
		return
	}
	if strings.TrimSpace(ack) == "" {
		return
	}
	ttsManagerChan <- ack
	ttsManagerChan <- "[[END]]"
}

func clampPercent(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func parseNumberToken(token string) (int, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(token); err == nil {
		return n, true
	}
	return parseChineseNumber(token)
}

func parseChineseNumber(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	s = strings.ReplaceAll(s, "两", "二")
	s = strings.ReplaceAll(s, "〇", "零")

	if s == "百" || s == "一百" {
		return 100, true
	}
	if strings.HasPrefix(s, "一百") {
		return 100, true
	}

	if strings.Contains(s, "十") {
		parts := strings.SplitN(s, "十", 2)
		tens := 1
		if parts[0] != "" {
			if v, ok := cnDigit(parts[0]); ok {
				tens = v
			} else {
				return 0, false
			}
		}
		ones := 0
		if len(parts) > 1 && parts[1] != "" {
			if v, ok := cnDigit(parts[1]); ok {
				ones = v
			} else {
				return 0, false
			}
		}
		return tens*10 + ones, true
	}

	if v, ok := cnDigit(s); ok {
		return v, true
	}
	return 0, false
}

func cnDigit(s string) (int, bool) {
	switch s {
	case "零":
		return 0, true
	case "一":
		return 1, true
	case "二":
		return 2, true
	case "三":
		return 3, true
	case "四":
		return 4, true
	case "五":
		return 5, true
	case "六":
		return 6, true
	case "七":
		return 7, true
	case "八":
		return 8, true
	case "九":
		return 9, true
	default:
		return 0, false
	}
}
