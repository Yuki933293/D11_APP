import queue
import re
import threading
import time

# ================= 并发控制与状态变量 =================
session_cancel_event = threading.Event()
session_lock = threading.Lock()

current_session_id = ""
session_id_lock = threading.Lock()

tts_manager_queue: "queue.Queue[str]" = queue.Queue(maxsize=500)
audio_pcm_queue: "queue.Queue[bytes]" = queue.Queue(maxsize=4000)

player_lock = threading.Lock()
player_proc = None
player_stdin = None

emoji_regex = re.compile(r"[\U0001F600-\U0001F64F\U0001F300-\U0001F5FF\U0001F680-\U0001F6FF]")
music_punct = re.compile(r"[，。！？,.!?\s；;：:“”\"'《》()（）【】\[\]、]")

music_mgr = None

# 云端伪唤醒状态
awake_flag = False
awake_lock = threading.Lock()
last_active_unix_nano = 0

# 退出标记
shutdown_event = threading.Event()

# ================= 性能监控辅助变量 =================
ts_llm_start = None
ts_tts_start = None
ts_first_audio = None

def touch_active() -> None:
    global last_active_unix_nano
    last_active_unix_nano = int(time.time() * 1e9)
