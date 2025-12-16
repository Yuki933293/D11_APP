/*
package webrtcvad

//#cgo CFLAGS: -I.
//#include "webrtc/common_audio/vad/include/webrtc_vad.h"
import "C"

import (
	"errors"
	"runtime"
	"unsafe"
)

func New() (*VAD, error) {
	var inst *C.struct_WebRtcVadInst

	ret := C.WebRtcVad_Create(&inst)
	if ret != 0 {
		return nil, errors.New("failed to create VAD")
	}

	vad := &VAD{inst}
	runtime.SetFinalizer(vad, free)

	ret = C.WebRtcVad_Init(inst)
	if ret != 0 {
		return nil, errors.New("default mode could not be set")
	}

	return vad, nil
}

func free(vad *VAD) {
	C.WebRtcVad_Free(vad.inst)
}

type VAD struct {
	inst *C.struct_WebRtcVadInst
}

func (v *VAD) SetMode(mode int) error {
	ret := C.WebRtcVad_set_mode(v.inst, C.int(mode))
	if ret != 0 {
		return errors.New("mode could not be set")
	}
	return nil
}

func (v *VAD) Process(fs int, audioFrame []byte) (activeVoice bool, err error) {
	if len(audioFrame)%2 != 0 {
		return false, errors.New("audio frames must be 16bit little endian unsigned integers")
	}

	audioFramePtr := (*C.int16_t)(unsafe.Pointer(&audioFrame[0]))
	frameLen := C.int(len(audioFrame) / 2)

	ret := C.WebRtcVad_Process(v.inst, C.int(fs), audioFramePtr, frameLen)
	switch ret {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, errors.New("processing error")
	}
}

func (v *VAD) ValidRateAndFrameLength(rate int, frameLength int) bool {
	ret := C.WebRtcVad_ValidRateAndFrameLength(C.int(rate), C.int(frameLength))
	if ret < 0 {
		return false
	}
	return true
}
*/

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
