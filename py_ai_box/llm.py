import json
import re
import ssl
import time
import urllib.request

from . import state
from . import config_runtime
from .intent import clean_text


def call_agent_stream(cancel_event, prompt: str, enable_search: bool) -> None:
    from .util import flush_queue
    flush_queue(state.tts_manager_queue)
    llm_start = time.time()

    model_name = config_runtime.llm_model_fast
    if enable_search:
        model_name = config_runtime.llm_model_search
        print("LLM: 检测到时效性需求，已动态开启联网搜索...")

    system_prompt = (
        "你是智能助手。仅在用户【明确要求播放音乐】（如“放首歌”、“听周杰伦”）时，才在回复末尾添加 [PLAY: 歌名]（随机播放用 [PLAY: RANDOM]）。"
        "如果用户要求停止，加上 [STOP]。"
        "回答天气、新闻、闲聊等普通问题时，【严禁】添加任何播放指令。"
    )
    payload = {
        "model": model_name,
        "input": {
            "messages": [
                {"role": "system", "content": system_prompt},
                {"role": "user", "content": prompt},
            ],
        },
        "parameters": {
            "result_format": "text",
            "incremental_output": True,
            "enable_search": enable_search,
        },
    }

    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        config_runtime.llm_url,
        data=data,
        headers={
            "Authorization": f"Bearer {config_runtime.dash_api_key}",
            "Content-Type": "application/json",
            "X-DashScope-SSE": "enable",
        },
    )
    ctx = ssl._create_unverified_context()
    try:
        resp = urllib.request.urlopen(req, context=ctx, timeout=60)
    except Exception as err:
        print(f"❌ LLM: 请求失败: {err}")
        if state.music_mgr is not None:
            state.music_mgr.unduck()
        return

    full_text = []
    chunk_buffer = []
    first_chunk_sent = False
    print("LLM 推理: ", end="", flush=True)

    for raw in resp:
        if cancel_event.is_set():
            resp.close()
            return
        line = raw.decode("utf-8", errors="ignore").strip()
        if not line.startswith("data:"):
            continue
        data_str = line[len("data:"):].strip()
        if data_str == "[DONE]":
            break
        try:
            chunk = json.loads(data_str)
        except Exception:
            continue
        text = chunk.get("output", {}).get("text", "")
        if not text:
            continue
        clean = clean_text(text)
        if not clean:
            continue
        print(clean, end="", flush=True)
        full_text.append(clean)
        chunk_buffer.append(clean)

        threshold = 15 if enable_search else 30
        buf_text = "".join(chunk_buffer)
        if not first_chunk_sent:
            if re.search(r"[，。！？,.!?\n]", clean) or len(buf_text) > threshold:
                first_chunk_sent = True
                send_chunk(buf_text)
                chunk_buffer = []
        else:
            if re.search(r"[，。！？,.!?\n]", clean) or len(buf_text) > 80:
                send_chunk(buf_text)
                chunk_buffer = []

    print()
    print(f"⏱LLM 推理结束，总耗时: {time.time() - llm_start:.3f}s")

    if chunk_buffer:
        send_chunk("".join(chunk_buffer))
    state.tts_manager_queue.put("[[END]]")

    full_text_str = "".join(full_text)
    if "[STOP]" in full_text_str and state.music_mgr is not None:
        state.music_mgr.stop()
    m = re.search(r"(?i)\[PLAY:\s*(.*?)\]", full_text_str)
    if m and state.music_mgr is not None:
        play_target = m.group(1).strip()
        print(f"[MUSIC-DBG] play_token={play_target}")
        state.music_mgr.search_and_play(play_target)
    resp.close()


def send_chunk(text: str) -> None:
    text = re.sub(r"\[.*?\]", "", text).strip()
    if text:
        state.tts_manager_queue.put(text)
