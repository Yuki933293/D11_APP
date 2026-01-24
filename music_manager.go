package main

import (
	"encoding/binary"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ================= 🎵 音乐管理器 =================
type MusicManager struct {
	isPlaying     bool
	mu            sync.Mutex
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stopChan      chan struct{}
	targetVolume  float64
	currentVolume float64
	volMutex      sync.Mutex
}

func NewMusicManager() *MusicManager {
	return &MusicManager{targetVolume: 1.0, currentVolume: 1.0}
}

func (m *MusicManager) IsPlaying() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isPlaying
}

func (m *MusicManager) setTargetVolume(vol float64) {
	m.volMutex.Lock()
	m.targetVolume = vol
	m.volMutex.Unlock()
}

func (m *MusicManager) Duck() {
	if m.IsPlaying() {
		// Duck 要“明显且快速”：先把目标压到 20%，并把当前音量立即拉到一个较低上限，
		// 避免因为缓慢平滑导致用户听感“没有降音量”。
		m.volMutex.Lock()
		m.targetVolume = 0.2
		if m.currentVolume > 0.35 {
			m.currentVolume = 0.35
		}
		m.volMutex.Unlock()
	}
}

func (m *MusicManager) Unduck() {
	if m.IsPlaying() {
		m.setTargetVolume(1.0)
	}
}

func (m *MusicManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isPlaying {
		log.Println("🛑 [MUSIC] 停止播放")
		select {
		case m.stopChan <- struct{}{}:
		default:
		}
		if m.stdin != nil {
			m.stdin.Close()
		}
		if m.cmd != nil && m.cmd.Process != nil {
			m.cmd.Process.Kill()
			m.cmd.Wait()
		}
		m.isPlaying = false
	}
}

func (m *MusicManager) PlayFile(path string) {
	m.Stop()
	time.Sleep(200 * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	file, err := os.Open(path)
	if err != nil {
		return
	}

	// -B 是缓冲时间(us)：太小会在 CPU 抖动时 underrun（卡顿），太大会导致 Duck/切歌响应滞后。
	// 这里取一个折中值，配合下游“前置缓冲”控制，保证不卡顿且仍可及时 Duck。
	cmd := exec.Command("aplay", "-D", "default", "-q", "-t", "raw", "-r", "16000", "-c", "1", "-f", "S16_LE", "-B", "80000")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		file.Close()
		return
	}
	if err := cmd.Start(); err != nil {
		file.Close()
		return
	}

	m.cmd = cmd
	m.stdin = stdin
	m.isPlaying = true
	m.stopChan = make(chan struct{}, 1)
	m.targetVolume = 1.0
	m.currentVolume = 1.0

	log.Printf("🎵 [MUSIC] 开始播放: %s", filepath.Base(path))

	go func(f *os.File, pipe io.WriteCloser, myCmd *exec.Cmd, stopCh chan struct{}) {
		defer f.Close()
		defer pipe.Close()
		f.Seek(44, 0)
		// 关键：
		// - 不能“严格实时”地喂数据（每 20ms sleep 一次），否则在 RK3308 上只要调度抖动就会 underrun（听感卡顿）。
		// - 也不能一次性喂太快/太多，否则 Duck 的听感会滞后（因为旧音量的音频已经预灌进 aplay/管道）。
		//
		// 策略：维护一个小的“前置缓冲”（例如 120~180ms），既抗抖动又保证 Duck 仍然足够跟手。
		const (
			musicSampleRate = 16000
			chunkSamples    = 640 // 40ms：降低调度开销，同时仍有较好音量跟随
			targetAhead     = 120 * time.Millisecond
			maxAhead        = 180 * time.Millisecond
		)
		buf := make([]byte, chunkSamples*2)

		var (
			startWall    time.Time
			wroteSamples int64
			lastStepAt   time.Time
		)
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			n, err := f.Read(buf)
			if n > 0 {
				if startWall.IsZero() {
					startWall = time.Now()
					lastStepAt = startWall
				}

				// 读取当前目标音量，并按 dt 做平滑逼近（Duck 快、Unduck 慢）
				now := time.Now()
				dt := now.Sub(lastStepAt)
				lastStepAt = now

				m.volMutex.Lock()
				target := m.targetVolume
				current := m.currentVolume
				m.volMutex.Unlock()

				if target < 0 {
					target = 0
				} else if target > 1 {
					target = 1
				}
				if current < 0 {
					current = 0
				} else if current > 1 {
					current = 1
				}

				if dt <= 0 {
					current = target
				} else if current != target {
					var tau time.Duration
					if target < current {
						tau = 120 * time.Millisecond
					} else {
						tau = 900 * time.Millisecond
					}
					alpha := 1 - math.Exp(-float64(dt)/float64(tau))
					if alpha < 0 {
						alpha = 0
					} else if alpha > 1 {
						alpha = 1
					}
					current = current + (target-current)*alpha
				}

				m.volMutex.Lock()
				m.currentVolume = current
				m.volMutex.Unlock()

				// PCM16 振幅缩放 + 饱和裁剪
				for i := 0; i+1 < n; i += 2 {
					sample := int16(binary.LittleEndian.Uint16(buf[i : i+2]))
					v := int(float64(sample) * current)
					if v > 32767 {
						v = 32767
					} else if v < -32768 {
						v = -32768
					}
					binary.LittleEndian.PutUint16(buf[i:i+2], uint16(int16(v)))
				}

				if _, werr := pipe.Write(buf[:n]); werr != nil {
					return
				}

				// 维护“前置缓冲”：若已写入的音频时长领先于墙钟太多，则主动 sleep 让播放追上来。
				wroteSamples += int64(n / 2)
				audioDur := time.Duration(wroteSamples) * time.Second / musicSampleRate
				ahead := audioDur - time.Since(startWall)
				if ahead > maxAhead {
					sleepDur := ahead - targetAhead
					if sleepDur > 0 {
						select {
						case <-stopCh:
							return
						case <-time.After(sleepDur):
						}
					}
				}
			}

			if err != nil {
				break
			}
		}
		m.mu.Lock()
		if m.isPlaying && m.cmd == myCmd {
			m.isPlaying = false
			go myCmd.Wait()
		}
		m.mu.Unlock()
	}(file, stdin, cmd, m.stopChan)
}

func (m *MusicManager) SearchAndPlay(query string) bool {
	files, err := ioutil.ReadDir(musicDir)
	if err != nil {
		return false
	}
	var candidates []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(strings.ToLower(f.Name()), ".wav") {
			candidates = append(candidates, filepath.Join(musicDir, f.Name()))
		}
	}
	if len(candidates) == 0 {
		return false
	}
	target := ""
	if query == "RANDOM" {
		target = candidates[rand.Intn(len(candidates))]
	} else {
		q := strings.ToLower(query)
		for _, path := range candidates {
			if strings.Contains(strings.ToLower(filepath.Base(path)), q) {
				target = path
				break
			}
		}
		if target == "" {
			return false
		}
	}
	m.PlayFile(target)
	return true
}
