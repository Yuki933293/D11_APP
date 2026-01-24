package main

import (
	"encoding/binary"
	"io"
	"log"
	"os/exec"
	"strconv"

	"ai_box/aec"

	vado "github.com/maxhawkins/go-webrtc-vad"
)

func audioLoop(aecProc *aec.Processor, vadEng *vado.VAD) {
	cmd := exec.Command("arecord",
		"-D", arecordDevice,
		"-c", strconv.Itoa(arecordChannels),
		"-r", strconv.Itoa(arecordRate),
		"-f", "S16_LE",
		"-t", "raw",
		"--period-size="+strconv.Itoa(arecordPeriodSize),
		"--buffer-size="+strconv.Itoa(arecordBufferSize),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	log.Println("🎤 麦克风已开启...")

	readBuf := make([]byte, 256*10*2)
	vadAccumulator := make([]int16, 0, 1024)
	var asrBuffer []int16
	silenceCount, speechCount := 0, 0
	triggered := false
	ducked := false
	fallbackMono := make([]int16, 256)

	for {
		if _, err := io.ReadFull(stdout, readBuf); err != nil {
			break
		}
		rawInt16 := make([]int16, 256*10)
		for i := 0; i < len(rawInt16); i++ {
			rawInt16[i] = int16(binary.LittleEndian.Uint16(readBuf[i*2 : i*2+2]))
		}
		clean, _ := aecProc.Process(rawInt16)
		if clean == nil {
			// AEC 异常回退：取第 0 通道直通，避免整段音频被丢弃导致“说了却识别不到”
			for i := 0; i < 256; i++ {
				fallbackMono[i] = rawInt16[i*10+0]
			}
			clean = fallbackMono
		}
		vadAccumulator = append(vadAccumulator, clean...)

		for len(vadAccumulator) >= 320 {
			frame := vadAccumulator[:320]
			vadAccumulator = vadAccumulator[320:]
			vadBuf := make([]byte, 640)
			for i, v := range frame {
				binary.LittleEndian.PutUint16(vadBuf[i*2:], uint16(v))
			}
			active, _ := vadEng.Process(16000, vadBuf)
			if active {
				speechCount++
				silenceCount = 0
			} else {
				silenceCount++
				speechCount = 0
			}

			// 先快速 Duck（听感上立刻压低背景音），再决定是否进入 ASR 录音段
			if speechCount > 2 && !ducked {
				ducked = true
				musicMgr.Duck()
			}

			if speechCount > 10 && !triggered {
				triggered = true
			}
			if triggered {
				asrBuffer = append(asrBuffer, frame...)
				if silenceCount > 10 || len(asrBuffer) > 16000*8 {
					if len(asrBuffer) > 4800 {
						finalData := make([]int16, len(asrBuffer))
						copy(finalData, asrBuffer)
						go processASR(finalData)
					} else {
						musicMgr.Unduck()
					}
					asrBuffer = []int16{}
					triggered = false
					ducked = false
					silenceCount = 0
				}
			} else {
				if len(asrBuffer) > 8000 {
					asrBuffer = asrBuffer[320:]
				}
				asrBuffer = append(asrBuffer, frame...)
			}
		}
	}
}
