import subprocess

from . import state
from .util import flush_queue


def perform_stop() -> None:
    print("物理清理: 强制切断所有声音源")
    with state.session_lock:
        state.session_cancel_event.set()

    flush_queue(state.tts_manager_queue)
    flush_queue(state.audio_pcm_queue)

    subprocess.run(["killall", "-9", "aplay"], check=False)
    if state.music_mgr is not None:
        state.music_mgr.stop()

    with state.player_lock:
        if state.player_stdin is not None:
            try:
                state.player_stdin.close()
            except Exception:
                pass
        state.player_proc = None
        state.player_stdin = None
