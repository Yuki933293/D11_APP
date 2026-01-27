import subprocess
import time

from . import state


def audio_player() -> None:
    def do_start():
        print("🔍 [Audio-Link] 启动 aplay 物理进程...")
        proc = subprocess.Popen(
            ["aplay", "-D", "default", "-t", "raw", "-r", "22050", "-f", "S16_LE", "-c", "1", "-B", "20000"],
            stdin=subprocess.PIPE,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        if proc.stdin is None:
            return None, None
        with state.player_lock:
            state.player_proc = proc
            state.player_stdin = proc.stdin
        return proc, proc.stdin

    while True:
        if state.shutdown_event.is_set():
            return
        pcm_data = state.audio_pcm_queue.get()
        if not pcm_data:
            print("[Audio-Link] 收到数据结束标志，执行物理保活...")
            time.sleep(0.5)
            with state.player_lock:
                if state.player_stdin is not None:
                    try:
                        state.player_stdin.close()
                    except Exception:
                        pass
                proc = state.player_proc
            if proc is not None:
                def wait_close(p):
                    try:
                        p.wait()
                    except Exception:
                        pass
                    with state.player_lock:
                        state.player_proc = None
                        state.player_stdin = None
                    print("[Audio-Link] 物理播报完成，系统解锁")

                import threading
                threading.Thread(target=wait_close, args=(proc,), daemon=True).start()
            continue

        with state.player_lock:
            stdin = state.player_stdin
        if stdin is None:
            _, stdin = do_start()
        if stdin is not None:
            try:
                stdin.write(pcm_data)
                stdin.flush()
            except Exception:
                with state.player_lock:
                    state.player_proc = None
                    state.player_stdin = None
