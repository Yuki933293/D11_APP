import json
import threading
import time
import uuid

from . import state
from . import config_runtime
from .websocket_client import WebSocketClient


def tts_manager_loop() -> None:
    conn = None
    current_task_id = None
    local_session_id = None
    recv_thread = None
    task_started_event = threading.Event()
    first_packet_received = False

    def is_canceled(ev):
        return ev.is_set()

    def receive_loop(ws: WebSocketClient, ev):
        nonlocal first_packet_received
        try:
            while True:
                if is_canceled(ev):
                    return
                try:
                    msg_type, payload = ws.recv()
                except RuntimeError:
                    return
                if msg_type == "binary":
                    if not first_packet_received:
                        state.ts_first_audio = time.time()
                        first_packet_received = True
                        if state.ts_tts_start is not None:
                            print(f"TTS 首包: {state.ts_first_audio - state.ts_tts_start:.3f}s")
                    if not is_canceled(ev):
                        state.audio_pcm_queue.put(payload)
                    continue
                if msg_type != "text":
                    continue
                try:
                    resp = json.loads(payload)
                except Exception:
                    continue
                header = resp.get("header", {})
                event = header.get("event")
                if event == "task-started":
                    task_started_event.set()
                if event in ("task-finished", "task-failed"):
                    return
        finally:
            if not is_canceled(ev):
                state.audio_pcm_queue.put(b"")

    while True:
        if state.shutdown_event.is_set():
            if conn is not None:
                conn.close()
            return
        msg = state.tts_manager_queue.get()
        with state.session_id_lock:
            global_id = state.current_session_id

        if local_session_id != global_id:
            if conn is not None:
                conn.close()
                conn = None
            local_session_id = global_id

        if is_canceled(state.session_cancel_event):
            if conn is not None:
                conn.close()
                conn = None
            continue

        if msg == "[[END]]":
            if conn is not None:
                conn.send_text(json.dumps({
                    "header": {"task_id": current_task_id, "action": "finish-task", "streaming": "duplex"},
                    "payload": {"input": {}},
                }, ensure_ascii=False))
                if recv_thread is not None:
                    recv_thread.join(timeout=5)
                conn.close()
                conn = None
            continue

        if msg.strip():
            if conn is None:
                conn = WebSocketClient(config_runtime.tts_ws_url, headers={"Authorization": f"Bearer {config_runtime.dash_api_key}"})
                try:
                    conn.connect()
                except Exception:
                    conn = None
                    continue
                current_task_id = uuid.uuid4().hex
                first_packet_received = False
                state.ts_tts_start = time.time()
                task_started_event.clear()
                recv_thread = threading.Thread(
                    target=receive_loop,
                    args=(conn, state.session_cancel_event),
                    daemon=True,
                )
                recv_thread.start()
                conn.send_text(json.dumps({
                    "header": {"task_id": current_task_id, "action": "run-task", "streaming": "duplex"},
                    "payload": {
                        "task_group": "audio",
                        "task": "tts",
                        "function": "SpeechSynthesizer",
                        "model": config_runtime.tts_model,
                        "parameters": {
                            "text_type": "PlainText",
                            "voice": config_runtime.tts_voice,
                            "format": "pcm",
                            "sample_rate": config_runtime.tts_sample_rate,
                            "volume": config_runtime.tts_volume,
                            "enable_ssml": False,
                        },
                        "input": {},
                    },
                }, ensure_ascii=False))
                if not task_started_event.wait(timeout=5):
                    conn.close()
                    conn = None
                    continue
                time.sleep(0.05)

            conn.send_text(json.dumps({
                "header": {"task_id": current_task_id, "action": "continue-task", "streaming": "duplex"},
                "payload": {"input": {"text": msg}},
            }, ensure_ascii=False))
            time.sleep(0.05)
