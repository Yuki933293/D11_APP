package main

import (
	"crypto/tls"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func ttsManagerLoop() {
	var conn *websocket.Conn
	var wg sync.WaitGroup
	var currentTaskID string
	var localSessionID string
	taskStartedSignal := make(chan struct{}, 1)
	var firstPacketReceived bool

	isCanceled := func() bool {
		ctxMutex.Lock()
		defer ctxMutex.Unlock()
		return sessionCtx.Err() != nil
	}

	receiveLoop := func(c *websocket.Conn) {
		defer wg.Done()
		defer func() {
			if !isCanceled() {
				audioPcmChan <- []byte{}
			}
		}()
		for {
			if isCanceled() {
				return
			}
			msgType, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.BinaryMessage {
				if !firstPacketReceived {
					tsFirstAudio = time.Now()
					firstPacketReceived = true
					log.Printf("TTS 首包: %v", tsFirstAudio.Sub(tsTtsStart))
				}
				if !isCanceled() {
					audioPcmChan <- msg
				}
				continue
			}
			var resp map[string]interface{}
			if err := json.Unmarshal(msg, &resp); err != nil {
				continue
			}
			header, _ := resp["header"].(map[string]interface{})
			event := header["event"].(string)
			if event == "task-started" {
				select {
				case taskStartedSignal <- struct{}{}:
				default:
				}
			}
			if event == "task-finished" || event == "task-failed" {
				return
			}
		}
	}

	for {
		msg, ok := <-ttsManagerChan
		if !ok {
			return
		}

		sessionIDMutex.Lock()
		globalID := currentSessionID
		sessionIDMutex.Unlock()

		if localSessionID != globalID {
			if conn != nil {
				conn.Close()
				conn = nil
			}
			localSessionID = globalID
		}

		if isCanceled() {
			if conn != nil {
				conn.Close()
				conn = nil
			}
			continue
		}

		if msg == "[[END]]" {
			if conn != nil {
				conn.WriteJSON(map[string]interface{}{
					"header":  map[string]interface{}{"task_id": currentTaskID, "action": "finish-task", "streaming": "duplex"},
					"payload": map[string]interface{}{"input": map[string]interface{}{}},
				})
				wg.Wait()
				conn.Close()
				conn = nil
			}
			continue
		}

		if strings.TrimSpace(msg) != "" {
			if conn == nil {
				dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
				headers := http.Header{}
				headers.Add("Authorization", "Bearer "+dashAPIKey)
				c, _, err := dialer.Dial(ttsWsURL, headers)
				if err != nil {
					continue
				}
				conn = c
				currentTaskID = uuid.New().String()
				firstPacketReceived = false
				tsTtsStart = time.Now()
				wg.Add(1)
				go receiveLoop(conn)
				conn.WriteJSON(map[string]interface{}{
					"header": map[string]interface{}{"task_id": currentTaskID, "action": "run-task", "streaming": "duplex"},
					"payload": map[string]interface{}{
						"task_group": "audio", "task": "tts", "function": "SpeechSynthesizer",
						"model":      ttsModel,
						"parameters": map[string]interface{}{"text_type": "PlainText", "voice": ttsVoice, "format": "pcm", "sample_rate": ttsSampleRate, "volume": ttsVolume, "enable_ssml": false},
						"input":      map[string]interface{}{},
					},
				})
				select {
				case <-taskStartedSignal:
					time.Sleep(50 * time.Millisecond)
				case <-time.After(5 * time.Second):
					conn.Close()
					conn = nil
					continue
				}
			}
			conn.WriteJSON(map[string]interface{}{
				"header":  map[string]interface{}{"task_id": currentTaskID, "action": "continue-task", "streaming": "duplex"},
				"payload": map[string]interface{}{"input": map[string]interface{}{"text": msg}},
			})
			time.Sleep(50 * time.Millisecond)
		}
	}
}
