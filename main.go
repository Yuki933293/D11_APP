package main

/*
#cgo LDFLAGS: -lasound
#include <alsa/asoundlib.h>
#include <stdlib.h>

snd_pcm_t* open_device(char* name, int stream, int channels, int rate) {
    snd_pcm_t *handle;
    int err;
    snd_pcm_hw_params_t *hw_params;

    if ((err = snd_pcm_open(&handle, name, stream, 0)) < 0) return NULL;
    if ((err = snd_pcm_hw_params_malloc(&hw_params)) < 0) return NULL;
    if ((err = snd_pcm_hw_params_any(handle, hw_params)) < 0) return NULL;

    if ((err = snd_pcm_hw_params_set_access(handle, hw_params, SND_PCM_ACCESS_RW_INTERLEAVED)) < 0) return NULL;
    if ((err = snd_pcm_hw_params_set_format(handle, hw_params, SND_PCM_FORMAT_S16_LE)) < 0) return NULL;

    unsigned int r = rate;
    if ((err = snd_pcm_hw_params_set_rate_near(handle, hw_params, &r, 0)) < 0) return NULL;
    if ((err = snd_pcm_hw_params_set_channels(handle, hw_params, channels)) < 0) return NULL;

    if ((err = snd_pcm_hw_params(handle, hw_params)) < 0) return NULL;
    snd_pcm_hw_params_free(hw_params);
    if ((err = snd_pcm_prepare(handle)) < 0) return NULL;

    return handle;
}
*/
import "C"
import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
)

// === 配置常量 ===
const (
	AudioDeviceName = "plug:dsnoop_hw"
	AudioPlayName   = "plug:dmix_hw"
	RecSampleRate   = 16000
	RecChannels     = 2
	PlaySampleRate  = 48000
	PlayChannels    = 2
	FrameSizeMS     = 30
)

// ... (VADEngine 保持不变) ...
type VADEngine struct {
	EnergyThresh float64
	ActiveCount  int
	SilenceCount int
	IsSpeaking   bool
}

func NewVADEngine() *VADEngine {
	return &VADEngine{EnergyThresh: 1500.0}
}

func (v *VADEngine) Process(pcm []byte) int {
	var sum float64
	for i := 0; i < len(pcm); i += 2 {
		if i+1 >= len(pcm) {
			break
		}
		sample := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8)
		sum += float64(sample) * float64(sample)
	}
	rms := math.Sqrt(sum / (float64(len(pcm)) / 2.0))
	isActive := rms > v.EnergyThresh
	if isActive {
		v.ActiveCount++
		v.SilenceCount = 0
	} else {
		v.SilenceCount++
		v.ActiveCount = 0
	}
	if !v.IsSpeaking {
		if v.ActiveCount >= 8 {
			v.IsSpeaking = true
			return 1
		}
	} else {
		if v.SilenceCount >= 20 {
			v.IsSpeaking = false
			return 2
		}
	}
	return 0
}

// ... (AudioEngine 保持不变) ...
type AudioEngine struct {
	recHandle  *C.snd_pcm_t
	playHandle *C.snd_pcm_t
	mu         sync.Mutex
}

func NewAudioEngine() *AudioEngine { return &AudioEngine{} }

func (e *AudioEngine) Start() error {
	recName := C.CString(AudioDeviceName)
	defer C.free(unsafe.Pointer(recName))
	e.recHandle = C.open_device(recName, 1, C.int(RecChannels), C.int(RecSampleRate))
	if e.recHandle == nil {
		return fmt.Errorf("无法打开录音设备")
	}

	playName := C.CString(AudioPlayName)
	defer C.free(unsafe.Pointer(playName))
	e.playHandle = C.open_device(playName, 0, C.int(PlayChannels), C.int(PlaySampleRate))
	if e.playHandle == nil {
		return fmt.Errorf("无法打开播放设备")
	}
	return nil
}

func (e *AudioEngine) Read(buf []byte) int {
	if e.recHandle == nil {
		return 0
	}
	frames := C.snd_pcm_uframes_t(len(buf) / (2 * RecChannels))
	ptr := unsafe.Pointer(&buf[0])
	ret := C.snd_pcm_readi(e.recHandle, ptr, frames)
	if ret == -C.EPIPE {
		C.snd_pcm_prepare(e.recHandle)
		return 0
	}
	return int(ret)
}

func (e *AudioEngine) Write(buf []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.playHandle == nil {
		return
	}
	frames := C.snd_pcm_uframes_t(len(buf) / (2 * PlayChannels))
	ptr := unsafe.Pointer(&buf[0])
	ret := C.snd_pcm_writei(e.playHandle, ptr, frames)
	if ret == -C.EPIPE {
		C.snd_pcm_prepare(e.playHandle)
	}
}

func (e *AudioEngine) StopImmediate() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.playHandle != nil {
		C.snd_pcm_drop(e.playHandle)
		C.snd_pcm_prepare(e.playHandle)
	}
}

func (e *AudioEngine) Close() {
	if e.recHandle != nil {
		C.snd_pcm_close(e.recHandle)
	}
	if e.playHandle != nil {
		C.snd_pcm_close(e.playHandle)
	}
}

// ==========================================
// 3. 主程序逻辑 (最终修正版)
// ==========================================
func main() {
	serverAddr := flag.String("addr", "127.0.0.1:50010", "服务器地址")
	flag.Parse()

	// 1. 初始化音频
	audio := NewAudioEngine()
	if err := audio.Start(); err != nil {
		log.Fatal(err)
	}
	defer audio.Close()

	// 2. 连接服务器
	// ✅ 确认无疑的正确路径 (根据服务器日志)
	path := "/ws/voice_assistant"
	u := url.URL{Scheme: "ws", Host: *serverAddr, Path: path}

	log.Printf("🚀 正在连接服务器: %s", u.String())

	// ✅ 强制设置，防止各种环境干扰
	dialer := websocket.Dialer{
		Proxy:            nil, // 强制不走代理
		HandshakeTimeout: 5 * time.Second,
	}
	headers := http.Header{}
	headers.Add("Origin", "http://localhost") // 伪装成浏览器

	c, resp, err := dialer.Dial(u.String(), headers)
	if err != nil {
		log.Printf("❌ 连接失败: %v", err)
		if resp != nil {
			body, _ := ioutil.ReadAll(resp.Body)
			log.Printf("   服务器回复 (%s): %s", resp.Status, string(body))
			log.Println("⚠️  如果是 404，请立刻检查 Mac 上的 socat 命令端口是否写错成了 50000/50001？")
		}
		return
	}
	defer c.Close()
	log.Println("✅ WebSocket 连接成功！")

	// ... (以下 VAD 和音频循环逻辑保持不变，直接复制之前的即可) ...

	vad := NewVADEngine()
	audioIn := make(chan []byte, 100)
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	log.Println("🎙️ 系统启动完成！请说话...")

	// 接收播放
	go func() {
		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				log.Println("读取断开:", err)
				return
			}
			if len(message) > 0 {
				audio.Write(message)
			}
		}
	}()

	// 发送录音
	go func() {
		for chunk := range audioIn {
			err := c.WriteMessage(websocket.BinaryMessage, chunk)
			if err != nil {
				return
			}
		}
	}()

	// 主循环
	bufferSize := int(RecSampleRate * FrameSizeMS / 1000 * 2 * RecChannels)
	recBuffer := make([]byte, bufferSize)

	for {
		select {
		case <-interrupt:
			return
		default:
			n := audio.Read(recBuffer)
			if n <= 0 {
				continue
			}
			chunk := make([]byte, len(recBuffer))
			copy(chunk, recBuffer)

			status := vad.Process(chunk)
			if status == 1 {
				log.Println("🗣️ [VAD] 打断")
				audio.StopImmediate()
			}
			audioIn <- chunk
		}
	}
}
