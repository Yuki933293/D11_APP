import ast
import os
import time

from . import constraints

# ================= 配置加载（最小改动方案） =================

dash_api_key = ""

tts_ws_url = constraints.TTS_WS_URL
llm_url = constraints.LLM_URL
asr_ws_url = constraints.WS_ASR_URL

llm_model_fast = "qwen-turbo-latest"
llm_model_search = "qwen-max"
tts_model = "cosyvoice-v3-plus"
tts_voice = "longanhuan"
tts_sample_rate = 22050
tts_volume = 50
asr_model = "paraformer-realtime-v2"
asr_sample_rate = 16000

music_dir = constraints.MUSIC_DIR

arecord_device = "hw:2,0"
arecord_channels = 10
arecord_rate = 16000
arecord_period_size = 256
arecord_buffer_size = 16384

wake_idle_timeout = constraints.WAKE_IDLE_TIMEOUT
wake_ack_text = constraints.WAKE_ACK_TEXT


def init_runtime_config() -> None:
    loaded_env, err = load_env_file_from_candidates()
    if err:
        print(f"⚠️ [配置] 读取 env 文件失败: {err}")
    elif loaded_env:
        print(f"🔧 [配置] 已加载 env 文件: {loaded_env}")

    global dash_api_key
    dash_api_key = os.environ.get("AI_BOX_DASH_API_KEY", "").strip()
    if not dash_api_key:
        dash_api_key = os.environ.get("DASHSCOPE_API_KEY", "").strip()
    if not dash_api_key:
        dash_api_key = constraints.DASH_API_KEY.strip()
    if not dash_api_key:
        raise RuntimeError("❌ [配置] 未配置 DashScope API Key：请在 env 文件中设置 AI_BOX_DASH_API_KEY（参考 deploy/ai_box.env）")

    global tts_ws_url, llm_url, asr_ws_url
    tts_ws_url = get_env("AI_BOX_TTS_WS_URL", tts_ws_url)
    llm_url = get_env("AI_BOX_LLM_URL", llm_url)
    asr_ws_url = get_env("AI_BOX_ASR_WS_URL", asr_ws_url)

    global llm_model_fast, llm_model_search
    llm_model_fast = get_env("AI_BOX_LLM_MODEL_FAST", llm_model_fast)
    llm_model_search = get_env("AI_BOX_LLM_MODEL_SEARCH", llm_model_search)

    global tts_model, tts_voice, tts_sample_rate, tts_volume
    tts_model = get_env("AI_BOX_TTS_MODEL", tts_model)
    tts_voice = get_env("AI_BOX_TTS_VOICE", tts_voice)
    tts_sample_rate = get_env_int("AI_BOX_TTS_SAMPLE_RATE", tts_sample_rate)
    tts_volume = get_env_int("AI_BOX_TTS_VOLUME", tts_volume)

    global asr_model, asr_sample_rate
    asr_model = get_env("AI_BOX_ASR_MODEL", asr_model)
    asr_sample_rate = get_env_int("AI_BOX_ASR_SAMPLE_RATE", asr_sample_rate)

    global music_dir
    music_dir = get_env("AI_BOX_MUSIC_DIR", music_dir)

    global arecord_device, arecord_channels, arecord_rate, arecord_period_size, arecord_buffer_size
    arecord_device = get_env("AI_BOX_ARECORD_DEVICE", arecord_device)
    arecord_channels = get_env_int("AI_BOX_ARECORD_CHANNELS", arecord_channels)
    arecord_rate = get_env_int("AI_BOX_ARECORD_RATE", arecord_rate)
    arecord_period_size = get_env_int("AI_BOX_ARECORD_PERIOD_SIZE", arecord_period_size)
    arecord_buffer_size = get_env_int("AI_BOX_ARECORD_BUFFER_SIZE", arecord_buffer_size)

    global wake_ack_text, wake_idle_timeout
    wake_ack_text = get_env("AI_BOX_WAKE_ACK_TEXT", wake_ack_text)
    wake_idle_timeout = get_env_duration("AI_BOX_WAKE_IDLE_TIMEOUT", wake_idle_timeout)

    wake_words = os.environ.get("AI_BOX_WAKE_WORDS", "").strip()
    if wake_words:
        from .constraints import WAKE_WORDS
        words = split_list(wake_words)
        if words:
            WAKE_WORDS[:] = words

    print(
        f"🔧 [配置] LLM(fast={llm_model_fast} search={llm_model_search}) | "
        f"ASR(model={asr_model}) | TTS(model={tts_model} voice={tts_voice}) | "
        f"musicDir={music_dir} | wakeIdle={wake_idle_timeout}s"
    )


def load_env_file_from_candidates():
    path = os.environ.get("AI_BOX_ENV_FILE", "").strip()
    if path:
        try:
            load_env_file(path)
            return path, None
        except Exception as err:
            return "", err

    candidates = ["/userdata/AI_BOX/ai_box.env", "./ai_box.env"]
    for p in candidates:
        try:
            load_env_file(p)
            return p, None
        except FileNotFoundError:
            continue
        except Exception as err:
            return "", err
    return "", None


def load_env_file(path: str) -> None:
    with open(path, "r", encoding="utf-8") as f:
        for raw in f:
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("export "):
                line = line[len("export "):].strip()
            if "=" not in line:
                continue
            key, val = line.split("=", 1)
            key = key.strip()
            val = val.strip()
            if not key:
                continue
            if key in os.environ:
                continue
            os.environ[key] = unquote_env_value(val)


def unquote_env_value(v: str) -> str:
    v = v.strip()
    if len(v) >= 2 and v[0] == '"' and v[-1] == '"':
        try:
            return ast.literal_eval(v)
        except Exception:
            return v[1:-1]
    if len(v) >= 2 and v[0] == "'" and v[-1] == "'":
        try:
            return ast.literal_eval(v)
        except Exception:
            return v[1:-1]
    return v


def get_env(key: str, default: str) -> str:
    val = os.environ.get(key, "").strip()
    return val if val else default


def get_env_int(key: str, default: int) -> int:
    val = os.environ.get(key, "").strip()
    if not val:
        return default
    try:
        return int(val)
    except ValueError:
        return default


def parse_duration(value: str) -> float:
    v = value.strip()
    if not v:
        return 0.0
    if v.endswith("ms"):
        return float(v[:-2]) / 1000.0
    if v.endswith("s"):
        return float(v[:-1])
    if v.endswith("m"):
        return float(v[:-1]) * 60.0
    if v.endswith("h"):
        return float(v[:-1]) * 3600.0
    return float(v)


def get_env_duration(key: str, default_seconds: float) -> float:
    val = os.environ.get(key, "").strip()
    if not val:
        return default_seconds
    try:
        return parse_duration(val)
    except ValueError:
        return default_seconds


def split_list(s: str):
    s = s.replace("，", ",")
    parts = [p.strip() for p in s.split(",")]
    return [p for p in parts if p]
