module ai_box

go 1.23

require (
	github.com/gorilla/websocket v1.5.3
	github.com/maxhawkins/go-webrtc-vad v0.0.0
)

// ★★★ 强制替换为本地 libs 目录 ★★★
replace github.com/maxhawkins/go-webrtc-vad => ./libs/go-webrtc-vad
