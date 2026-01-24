package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// 音量控制模块：基于 amixer 设置 ALSA 音量
// 说明：
// - 控件名随板子声卡不同可能变化（如 Master/PCM/Capture/Speaker），可按需调整
// - 这里仅提供通用封装，具体控制名由调用方决定

const (
	volumeCardIndex      = 1
	volumeControlSimple  = "aw_dev_0_rx_volume"
	volumeControlIndexed = "aw_dev_0_rx_volume,0"
	volumeStepPercent    = 5
	volumeRawMin         = 0
	volumeRawMax         = 1023
)

// SetAlsaVolumePercent 设置指定控件的音量百分比（0~100）
// 注意：当前板子控件为反向含义：raw 值越大声音越小，因此需要反向映射。
func SetAlsaVolume(control string, percent int) error {
	if strings.TrimSpace(control) == "" {
		return fmt.Errorf("控件名不能为空")
	}
	if percent < 0 || percent > 100 {
		return fmt.Errorf("音量范围必须在 0~100 之间")
	}
	raw := percentToRaw(percent)
	return runAmixerWithCard(volumeCardIndex, "sset", control, strconv.Itoa(raw))
}

// SetDeviceVolumePercent 设置默认设备音量（基于当前板子控制名）
func SetDeviceVolumePercent(percent int) error {
	return SetAlsaVolume(volumeControlIndexed, percent)
}

// AdjustDeviceVolumeStep 按固定步进调整音量（+5%/-5%）
// 注意：up=true 表示“更大声”，对应 raw 值减小。
func AdjustDeviceVolumeStep(up bool) error {
	currentRaw, err := getCurrentRawVolume(volumeControlSimple)
	if err != nil {
		return err
	}
	stepRaw := rawStepFromPercent(volumeStepPercent)
	if up {
		currentRaw -= stepRaw
	} else {
		currentRaw += stepRaw
	}
	currentRaw = clampRaw(currentRaw)
	return runAmixerWithCard(volumeCardIndex, "sset", volumeControlIndexed, strconv.Itoa(currentRaw))
}

// AdjustDeviceVolumeByPercent 按指定百分比增减音量（基于当前值）
// 注意：up=true 表示“更大声”，对应 raw 值减小。
func AdjustDeviceVolumeByPercent(percent int, up bool) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	currentRaw, err := getCurrentRawVolume(volumeControlSimple)
	if err != nil {
		return err
	}
	stepRaw := rawStepFromPercent(percent)
	if up {
		currentRaw -= stepRaw
	} else {
		currentRaw += stepRaw
	}
	currentRaw = clampRaw(currentRaw)
	return runAmixerWithCard(volumeCardIndex, "sset", volumeControlIndexed, strconv.Itoa(currentRaw))
}

func runAmixerWithCard(card int, args ...string) error {
	cmdArgs := args
	if card >= 0 {
		cmdArgs = append([]string{"-c", strconv.Itoa(card)}, args...)
	}
	cmd := exec.Command("amixer", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("amixer 执行失败: %v, 输出=%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func percentToRaw(percent int) int {
	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}
	rangeRaw := volumeRawMax - volumeRawMin
	raw := volumeRawMax - (rangeRaw*percent)/100
	return clampRaw(raw)
}

func rawStepFromPercent(step int) int {
	if step <= 0 {
		return 0
	}
	if step > 100 {
		step = 100
	}
	rangeRaw := volumeRawMax - volumeRawMin
	val := (rangeRaw * step) / 100
	if val < 1 {
		val = 1
	}
	return val
}

func clampRaw(val int) int {
	if val < volumeRawMin {
		return volumeRawMin
	}
	if val > volumeRawMax {
		return volumeRawMax
	}
	return val
}

func getCurrentRawVolume(control string) (int, error) {
	if strings.TrimSpace(control) == "" {
		return 0, fmt.Errorf("控件名不能为空")
	}
	out, err := runAmixerOutput(volumeCardIndex, "cget", fmt.Sprintf("name='%s'", control))
	if err != nil {
		return 0, err
	}
	// 示例行：": values=250"
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, ": values=") {
			valStr := strings.TrimPrefix(line, ": values=")
			valStr = strings.TrimSpace(valStr)
			n, convErr := strconv.Atoi(valStr)
			if convErr != nil {
				return 0, fmt.Errorf("解析当前音量失败: %v", convErr)
			}
			return clampRaw(n), nil
		}
	}
	return 0, fmt.Errorf("未能解析当前音量")
}

func runAmixerOutput(card int, args ...string) (string, error) {
	cmdArgs := args
	if card >= 0 {
		cmdArgs = append([]string{"-c", strconv.Itoa(card)}, args...)
	}
	cmd := exec.Command("amixer", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("amixer 执行失败: %v, 输出=%s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
