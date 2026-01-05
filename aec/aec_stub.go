//go:build !linux || !cgo
// +build !linux !cgo

package aec

// 说明：
// - 本文件用于在非 Linux 或未启用 CGO 的环境下编译通过（例如 macOS 本地开发）。
// - 真实板端（rk3308b）使用 `aec.go`（linux+cgo）加载 libluxaudio 做 AEC/降噪。

const (
	FrameSize = 256

	// 输入数据仍然是 10 通道 (因为 arecord 录的是 10)
	InputTotalCh = 10
	InputSize    = FrameSize * InputTotalCh
)

type Processor struct{}

func NewProcessor() *Processor { return &Processor{} }

// Process 直通回退：默认取第 0 通道作为“干净单声道”输出
func (p *Processor) Process(input []int16) ([]int16, int) {
	if len(input) != InputSize {
		return nil, 0
	}
	out := make([]int16, FrameSize)
	for i := 0; i < FrameSize; i++ {
		out[i] = input[i*InputTotalCh+0]
	}
	return out, 0
}
