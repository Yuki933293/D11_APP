import time

from . import state
from .constraints import WAKE_WORDS


def normalize_wake_text(text: str) -> str:
    s = text.lower().strip()
    s = state.music_punct.sub("", s)
    return s


def strip_wake_and_get_tail(text: str):
    normalized = normalize_wake_text(text)
    for w in WAKE_WORDS:
        nw = normalize_wake_text(w)
        idx = normalized.find(nw)
        if idx < 0:
            continue
        tail_norm = normalized[idx + len(nw):].strip()
        if tail_norm == "":
            return "", True, True
        pos = text.find(w)
        if pos >= 0:
            raw_tail = text[pos + len(w):].strip()
            raw_tail = state.music_punct.sub("", raw_tail).strip()
            if raw_tail:
                return raw_tail, True, False
        return text, True, False
    return "", False, False


def speak_wake_ack():
    from .util import flush_queue
    from .config_runtime import wake_ack_text
    flush_queue(state.tts_manager_queue)
    state.tts_manager_queue.put(wake_ack_text)
    state.tts_manager_queue.put("[[END]]")


def is_physical_busy() -> bool:
    with state.player_lock:
        is_tts_busy = state.player_proc is not None and state.player_proc.poll() is None
    is_music_busy = False
    if state.music_mgr is not None:
        is_music_busy = state.music_mgr.is_playing()
    return is_tts_busy or is_music_busy


def wake_idle_monitor():
    while True:
        time.sleep(2.0)
        with state.awake_lock:
            if not state.awake_flag:
                continue
        if is_physical_busy():
            continue
        last = state.last_active_unix_nano
        if last == 0:
            continue
        idle_s = (time.time() * 1e9 - last) / 1e9
        from .config_runtime import wake_idle_timeout
        if idle_s <= wake_idle_timeout:
            continue
        with state.awake_lock:
            state.awake_flag = False
        print("😴 [伪唤醒] 长时间无交互，进入休眠态，等待唤醒词...")
