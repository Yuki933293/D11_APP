import json
import os
import threading
import time
import uuid

from array import array

from . import state
from . import config_runtime
from .control import perform_stop
from .intent import has_music_intent, is_exit, is_interrupt, is_quick_switch
from .llm import call_agent_stream
from .volume_intent import handle_volume_command
from .wake import strip_wake_and_get_tail, speak_wake_ack
from .websocket_client import WebSocketClient


def process_asr(pcm: array) -> None:
    if state.shutdown_event.is_set():
        return
    if len(pcm) / 16000.0 < 0.5:
        return

    pcm_bytes = pcm.tobytes()
    text = call_asr_websocket(pcm_bytes)
    if not text:
        if state.music_mgr is not None:
            state.music_mgr.unduck()
        return

    tail, hit_wake, pure_wake = strip_wake_and_get_tail(text)

    with state.awake_lock:
        awake = state.awake_flag

    if not awake:
        if not hit_wake:
            print(f"[休眠] 未检测到唤醒词，忽略: [{text}]")
            if state.music_mgr is not None:
                state.music_mgr.unduck()
            return

        with state.awake_lock:
            state.awake_flag = True
        state.touch_active()

        if pure_wake:
            print("[伪唤醒] 唤醒成功")
            speak_wake_ack()
            if state.music_mgr is not None:
                state.music_mgr.unduck()
            return

        if tail.strip():
            print(f"[伪唤醒] 唤醒并转入指令: [{tail}]")
            text = tail
        else:
            print(f"[伪唤醒] 唤醒命中但未解析到后续指令，按原文处理: [{text}]")
    else:
        state.touch_active()
        if hit_wake:
            if pure_wake:
                print("[伪唤醒] 收到唤醒词")
                speak_wake_ack()
                if state.music_mgr is not None:
                    state.music_mgr.unduck()
                return
            if tail.strip() and tail != text:
                text = tail

    print(f"ASR识别结果: [{text}]")

    if is_exit(text):
        print("收到退出指令，关闭系统")
        state.shutdown_event.set()
        perform_stop()
        os._exit(0)

    with state.player_lock:
        is_tts_busy = state.player_proc is not None and state.player_proc.poll() is None
    is_music_busy = state.music_mgr.is_playing() if state.music_mgr is not None else False

    if handle_volume_command(text, is_tts_busy, is_music_busy):
        return

    if is_tts_busy or is_music_busy:
        music_req = has_music_intent(text)
        quick_switch = is_music_busy and is_quick_switch(text)

        if is_interrupt(text) or music_req or quick_switch:
            print(f"忙碌穿透: 指令 [{text}] 合法，执行物理清理并重置意图")
            perform_stop()
            if quick_switch:
                print(f"快速切歌触发: text={text!r}")
                if state.music_mgr is not None:
                    state.music_mgr.search_and_play("RANDOM")
                return
        else:
            print(f"锁定拦截: 忽略非控制类指令: [{text}]")
            if state.music_mgr is not None:
                state.music_mgr.unduck()
            return

    enable_search = any(k in text for k in ["天气", "今天", "星期几", "实时", "最新"])

    with state.session_lock:
        old_event = state.session_cancel_event
        old_event.set()
        state.session_cancel_event = threading.Event()
        new_event = state.session_cancel_event

    threading.Thread(
        target=call_agent_stream,
        args=(new_event, text, enable_search),
        daemon=True,
    ).start()


def call_asr_websocket(data: bytes) -> str:
    headers = {"Authorization": f"Bearer {config_runtime.dash_api_key}"}
    ws = WebSocketClient(config_runtime.asr_ws_url, headers=headers)
    try:
        ws.connect()
    except Exception:
        return ""

    task_id = f"{random_hex()}"
    run_payload = {
        "header": {"task_id": task_id, "action": "run-task", "streaming": "duplex"},
        "payload": {
            "task_group": "audio",
            "task": "asr",
            "function": "recognition",
            "model": config_runtime.asr_model,
            "parameters": {"format": "pcm", "sample_rate": config_runtime.asr_sample_rate},
            "input": {},
        },
    }
    ws.send_text(json.dumps(run_payload, ensure_ascii=False))

    for i in range(0, len(data), 3200):
        ws.send_binary(data[i:i + 3200])
        time.sleep(0.005)

    finish_payload = {"header": {"task_id": task_id, "action": "finish-task"}, "payload": {"input": {}}}
    ws.send_text(json.dumps(finish_payload, ensure_ascii=False))

    res = ""
    while True:
        msg_type, payload = ws.recv()
        if msg_type == "close":
            break
        if msg_type != "text":
            continue
        try:
            resp = json.loads(payload)
        except Exception:
            continue
        header = resp.get("header", {})
        event = header.get("event")
        if event == "result-generated":
            output = resp.get("payload", {}).get("output", {})
            sentence = output.get("sentence", {})
            text = sentence.get("text")
            if isinstance(text, str):
                res = text
        if event == "task-finished":
            break
    ws.close()
    return res


def random_hex() -> str:
    return uuid.uuid4().hex
