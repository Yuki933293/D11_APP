module ai_box

go 1.23.12

replace github.com/maxhawkins/go-webrtc-vad => ./libs/go-webrtc-vad

require (
	github.com/gorilla/websocket v1.5.3
	github.com/k2-fsa/sherpa-onnx-go v1.12.19
	github.com/maxhawkins/go-webrtc-vad v0.0.0-00010101000000-000000000000
)

require (
	github.com/k2-fsa/sherpa-onnx-go-linux v1.12.20 // indirect
	github.com/k2-fsa/sherpa-onnx-go-macos v1.12.20 // indirect
	github.com/k2-fsa/sherpa-onnx-go-windows v1.12.20 // indirect
)
