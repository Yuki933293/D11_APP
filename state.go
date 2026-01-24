package main

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// ================= 3. 并发控制与状态变量 =================
var (
	sessionCtx    context.Context
	sessionCancel context.CancelFunc
	ctxMutex      sync.Mutex

	currentSessionID string
	sessionIDMutex   sync.Mutex

	insecureClient *http.Client

	ttsManagerChan chan string
	audioPcmChan   chan []byte

	playerStdin io.WriteCloser
	playerCmd   *exec.Cmd
	playerMutex sync.Mutex

	emojiRegex *regexp.Regexp
	musicPunct = regexp.MustCompile(`[，。！？,.!?\s；;：:“”"'《》()（）【】\[\]、]`)
	musicMgr   *MusicManager

	// 云端伪唤醒状态：默认休眠，命中唤醒词后进入唤醒态
	awakeFlag          atomic.Bool
	lastActiveUnixNano atomic.Int64
)

// ================= 4. 性能监控辅助变量 =================
var (
	tsLlmStart   time.Time
	tsTtsStart   time.Time
	tsFirstAudio time.Time
)
