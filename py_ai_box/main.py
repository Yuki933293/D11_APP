import os
import sys
import threading
import time
import uuid

# 兼容 PyInstaller 运行时导入路径
if getattr(sys, "frozen", False):
    base_dir = getattr(sys, "_MEIPASS", None)
    if base_dir and base_dir not in sys.path:
        sys.path.insert(0, base_dir)

if __package__ in (None, ""):
    __package__ = "py_ai_box"

from py_ai_box import state
from py_ai_box import config_runtime
from py_ai_box.aec import AECProcessor
from py_ai_box.audio_loop import audio_loop
from py_ai_box.audio_player import audio_player
from py_ai_box.control import perform_stop
from py_ai_box.music_manager import init_music_manager
from py_ai_box.tts import tts_manager_loop
from py_ai_box.vad import create_vad_engine
from py_ai_box.wake import wake_idle_monitor


def main() -> None:
    print("[BUILD] py_ai_box 2026-01-26T1919Z")
    print("=== RK3308 AI 助手 (Python 版本) ===")

    config_runtime.init_runtime_config()
    state.session_cancel_event = threading.Event()
    with state.session_id_lock:
        state.current_session_id = uuid.uuid4().hex

    init_music_manager()

    with state.awake_lock:
        state.awake_flag = False
    state.last_active_unix_nano = 0
    print("😴 [伪唤醒] 初始为休眠态，仅响应唤醒词（例如：你好小瑞）")

    threading.Thread(target=audio_player, daemon=True).start()
    threading.Thread(target=tts_manager_loop, daemon=True).start()
    threading.Thread(target=wake_idle_monitor, daemon=True).start()

    aec_proc = AECProcessor()
    vad_eng = create_vad_engine()

    threading.Thread(target=audio_loop, args=(aec_proc, vad_eng), daemon=True).start()

    try:
        while True:
            if state.shutdown_event.is_set():
                print("收到退出指令，程序退出...")
                return
            time.sleep(1.0)
    except KeyboardInterrupt:
        print("收到中断信号，执行清理退出...")
        perform_stop()
        return


if __name__ == "__main__":
    main()
