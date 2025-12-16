package vad

import (
	"math"
)

// Config VAD 配置
const (
	// 能量阈值 (根据 AEC 后的效果调整，通常 500-2000 之间)
	// AEC 处理后，噪音会变小，人声会突显
	EnergyThreshold = 1000.0
)

// Engine VAD 引擎
type Engine struct {
	// 可以在这里加计数器做平滑处理
}

func NewEngine() *Engine {
	return &Engine{}
}

// IsSpeech 检测是否包含人声
// input: 256个采样点的 int16 切片
func (e *Engine) IsSpeech(data []int16) bool {
	if len(data) == 0 {
		return false
	}

	// 1. 计算均方根能量 (RMS)
	var sumSquares float64
	for _, sample := range data {
		val := float64(sample)
		sumSquares += val * val
	}
	rms := math.Sqrt(sumSquares / float64(len(data)))

	// 2. 简单的能量门限判断
	// 如果需要更高级的 WebRTC VAD，可以在这里替换 CGO 调用
	return rms > EnergyThreshold
}
