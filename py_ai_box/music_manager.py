import math
import os
import random
import subprocess
import threading
import time
from array import array
from pathlib import Path

from . import state
from . import config_runtime


class MusicManager:
    def __init__(self) -> None:
        self._is_playing = False
        self._mu = threading.Lock()
        self._cmd = None
        self._stdin = None
        self._stop_event = threading.Event()
        self._target_volume = 1.0
        self._current_volume = 1.0
        self._vol_lock = threading.Lock()

    def is_playing(self) -> bool:
        with self._mu:
            return self._is_playing

    def _set_target_volume(self, vol: float) -> None:
        with self._vol_lock:
            self._target_volume = vol

    def duck(self) -> None:
        if self.is_playing():
            with self._vol_lock:
                self._target_volume = 0.2
                if self._current_volume > 0.35:
                    self._current_volume = 0.35

    def unduck(self) -> None:
        if self.is_playing():
            self._set_target_volume(1.0)

    def stop(self) -> None:
        with self._mu:
            if self._is_playing:
                print("🛑 [MUSIC] 停止播放")
                self._stop_event.set()
                if self._stdin is not None:
                    try:
                        self._stdin.close()
                    except Exception:
                        pass
                if self._cmd is not None and self._cmd.poll() is None:
                    try:
                        self._cmd.kill()
                        self._cmd.wait()
                    except Exception:
                        pass
                self._is_playing = False
                self._stop_event = threading.Event()

    def play_file(self, path: str) -> None:
        self.stop()
        time.sleep(0.2)

        with self._mu:
            try:
                f = open(path, "rb")
            except Exception:
                print(f"[MUSIC-DBG] open_failed path={path}")
                return

            cmd = subprocess.Popen(
                ["aplay", "-D", "default", "-q", "-t", "raw", "-r", "16000", "-c", "1", "-f", "S16_LE", "-B", "80000"],
                stdin=subprocess.PIPE,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            if cmd.stdin is None:
                f.close()
                print(f"[MUSIC-DBG] aplay_start_failed path={path}")
                return

            self._cmd = cmd
            self._stdin = cmd.stdin
            self._is_playing = True
            self._target_volume = 1.0
            self._current_volume = 1.0
            print(f"🎵 [MUSIC] 开始播放: {Path(path).name}")

            thread = threading.Thread(
                target=self._stream_file,
                args=(f, cmd, self._stop_event),
                daemon=True,
            )
            thread.start()

    def _stream_file(self, f, cmd, stop_event: threading.Event) -> None:
        try:
            f.seek(44)
            music_sample_rate = 16000
            chunk_samples = 640  # 40ms
            target_ahead = 0.12
            max_ahead = 0.18
            buf_bytes = chunk_samples * 2

            start_wall = None
            wrote_samples = 0
            last_step_at = None

            while not stop_event.is_set():
                data = f.read(buf_bytes)
                if not data:
                    break

                if start_wall is None:
                    start_wall = time.time()
                    last_step_at = start_wall

                now = time.time()
                dt = max(0.0, now - (last_step_at or now))
                last_step_at = now

                with self._vol_lock:
                    target = min(1.0, max(0.0, self._target_volume))
                    current = min(1.0, max(0.0, self._current_volume))

                if dt == 0:
                    current = target
                elif current != target:
                    tau = 0.12 if target < current else 0.9
                    alpha = 1 - math.exp(-dt / tau)
                    current = current + (target - current) * alpha

                with self._vol_lock:
                    self._current_volume = current

                samples = array("h")
                samples.frombytes(data)
                for i in range(len(samples)):
                    v = int(samples[i] * current)
                    if v > 32767:
                        v = 32767
                    elif v < -32768:
                        v = -32768
                    samples[i] = v

                try:
                    self._stdin.write(samples.tobytes())
                    self._stdin.flush()
                except Exception:
                    break

                wrote_samples += len(samples)
                audio_dur = wrote_samples / music_sample_rate
                ahead = audio_dur - (time.time() - start_wall)
                if ahead > max_ahead:
                    sleep_dur = ahead - target_ahead
                    if sleep_dur > 0:
                        time.sleep(sleep_dur)
        finally:
            f.close()
            with self._mu:
                if self._is_playing and self._cmd == cmd:
                    self._is_playing = False
                    try:
                        cmd.wait(timeout=1)
                    except Exception:
                        pass

    def search_and_play(self, query: str) -> bool:
        try:
            files = os.listdir(config_runtime.music_dir)
        except Exception as err:
            print(f"[MUSIC-DBG] list_dir_failed dir={config_runtime.music_dir} err={err}")
            return False
        candidates = []
        for name in files:
            if name.lower().endswith(".wav"):
                candidates.append(str(Path(config_runtime.music_dir) / name))
        if not candidates:
            print(f"[MUSIC-DBG] no_wav dir={config_runtime.music_dir}")
            return False
        target = ""
        if query == "RANDOM":
            target = random.choice(candidates)
        else:
            q = query.lower()
            for path in candidates:
                if q in Path(path).name.lower():
                    target = path
                    break
            if not target:
                print(f"[MUSIC-DBG] no_match query={query}")
                return False
        print(f"[MUSIC-DBG] match file={Path(target).name}")
        self.play_file(target)
        return True


def init_music_manager() -> MusicManager:
    mgr = MusicManager()
    state.music_mgr = mgr
    return mgr
