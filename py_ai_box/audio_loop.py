import subprocess
import threading
from array import array

from . import state
from .aec import AECProcessor, FRAME_SIZE, INPUT_TOTAL_CH
from . import config_runtime
from .asr import process_asr


def audio_loop(aec_proc: AECProcessor, vad_engine) -> None:
    cmd = [
        "arecord",
        "-D", config_runtime.arecord_device,
        "-c", str(config_runtime.arecord_channels),
        "-r", str(config_runtime.arecord_rate),
        "-f", "S16_LE",
        "-t", "raw",
        f"--period-size={config_runtime.arecord_period_size}",
        f"--buffer-size={config_runtime.arecord_buffer_size}",
    ]
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if proc.stdout is None:
        raise RuntimeError("arecord 启动失败")
    print("🎤 麦克风已开启...")

    read_bytes = FRAME_SIZE * INPUT_TOTAL_CH * 2
    vad_acc = array("h")
    asr_buffer = array("h")
    silence_count = 0
    speech_count = 0
    triggered = False
    ducked = False
    fallback_mono = array("h", [0] * FRAME_SIZE)

    while True:
        if state.shutdown_event.is_set():
            break
        data = proc.stdout.read(read_bytes)
        if not data or len(data) < read_bytes:
            break
        raw = array("h")
        raw.frombytes(data)

        clean, _ = aec_proc.process(raw)
        if clean is None:
            for i in range(FRAME_SIZE):
                fallback_mono[i] = raw[i * INPUT_TOTAL_CH + 0]
            clean = fallback_mono

        vad_acc.extend(clean)

        while len(vad_acc) >= 320:
            frame = vad_acc[:320]
            del vad_acc[:320]

            active = vad_engine.is_speech(frame)
            if active:
                speech_count += 1
                silence_count = 0
            else:
                silence_count += 1
                speech_count = 0

            if speech_count > 2 and not ducked and state.music_mgr is not None:
                ducked = True
                state.music_mgr.duck()

            if speech_count > 10 and not triggered:
                triggered = True

            if triggered:
                asr_buffer.extend(frame)
                if silence_count > 10 or len(asr_buffer) > 16000 * 8:
                    if len(asr_buffer) > 4800:
                        final_data = array("h", asr_buffer)
                        threading.Thread(target=process_asr, args=(final_data,), daemon=True).start()
                    else:
                        if state.music_mgr is not None:
                            state.music_mgr.unduck()
                    asr_buffer = array("h")
                    triggered = False
                    ducked = False
                    silence_count = 0
            else:
                if len(asr_buffer) > 8000:
                    del asr_buffer[:320]
                asr_buffer.extend(frame)
