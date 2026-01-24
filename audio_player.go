package main

import (
	"io"
	"log"
	"os/exec"
	"time"
)

func audioPlayer() {
	doStart := func() (*exec.Cmd, io.WriteCloser) {
		log.Println("🔍 [Audio-Link] 启动 aplay 物理进程...")
		c := exec.Command("aplay", "-D", "default", "-t", "raw", "-r", "22050", "-f", "S16_LE", "-c", "1", "-B", "20000")
		s, err := c.StdinPipe()
		if err != nil {
			return nil, nil
		}
		if err := c.Start(); err != nil {
			return nil, nil
		}
		playerMutex.Lock()
		playerCmd = c
		playerStdin = s
		playerMutex.Unlock()
		return c, s
	}

	for pcmData := range audioPcmChan {
		if len(pcmData) == 0 {
			log.Println("[Audio-Link] 收到数据结束标志，执行物理保活...")
			time.Sleep(500 * time.Millisecond)
			if playerStdin != nil {
				playerStdin.Close()
			}
			if playerCmd != nil {
				go func(c *exec.Cmd) {
					if c != nil {
						_ = c.Wait()
					}
					playerMutex.Lock()
					playerCmd = nil
					playerStdin = nil
					playerMutex.Unlock()
					log.Println("[Audio-Link] 物理播报完成，系统解锁")
				}(playerCmd)
			}
			continue
		}

		if playerStdin == nil {
			doStart()
		}
		if playerStdin != nil {
			_, err := playerStdin.Write(pcmData)
			if err != nil {
				playerMutex.Lock()
				playerCmd = nil
				playerStdin = nil
				playerMutex.Unlock()
			}
		}
	}
}
