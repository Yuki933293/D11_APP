package main

import (
	"log"
	"os/exec"
)

func performStop() {
	log.Println("物理清理: 强制切断所有声音源")
	ctxMutex.Lock()
	if sessionCancel != nil {
		sessionCancel()
	}
	ctxMutex.Unlock()

	flushChannel(ttsManagerChan)
	flushChannel(audioPcmChan)

	exec.Command("killall", "-9", "aplay").Run()
	musicMgr.Stop()

	playerMutex.Lock()
	if playerStdin != nil {
		playerStdin.Close()
	}
	playerCmd = nil
	playerStdin = nil
	playerMutex.Unlock()
}
