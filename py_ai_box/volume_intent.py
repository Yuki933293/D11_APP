import re

from . import state
from .volume_control import (
    adjust_device_volume_by_percent,
    adjust_device_volume_step,
    set_device_volume_percent,
    VOLUME_STEP_PERCENT,
)

volume_set_re = re.compile(r"音量\s*调到\s*([0-9]{1,3})")
volume_set_cn_re = re.compile(r"音量\s*调到\s*([一二三四五六七八九十百两零〇]{1,4})")
volume_increase_re = re.compile(r"(音量|声音).*(调大|调高|增大|提高)\s*([0-9]{1,3})")
volume_decrease_re = re.compile(r"(音量|声音).*(调小|调低|降低|减小)\s*([0-9]{1,3})")
volume_increase_re2 = re.compile(r"(调大|调高|增大|提高).*(音量|声音)\s*([0-9]{1,3})")
volume_decrease_re2 = re.compile(r"(调小|调低|降低|减小).*(音量|声音)\s*([0-9]{1,3})")
volume_increase_cn_re = re.compile(r"(音量|声音).*(调大|调高|增大|提高)\s*([一二三四五六七八九十百两零〇]{1,4})")
volume_decrease_cn_re = re.compile(r"(音量|声音).*(调小|调低|降低|减小)\s*([一二三四五六七八九十百两零〇]{1,4})")
volume_increase_cn_re2 = re.compile(r"(调大|调高|增大|提高).*(音量|声音)\s*([一二三四五六七八九十百两零〇]{1,4})")
volume_decrease_cn_re2 = re.compile(r"(调小|调低|降低|减小).*(音量|声音)\s*([一二三四五六七八九十百两零〇]{1,4})")


def handle_volume_command(text: str, is_tts_busy: bool, is_music_busy: bool) -> bool:
    normalized = text.strip()
    if not normalized:
        return False

    percent, ok = parse_volume_set_percent(normalized)
    if ok:
        try:
            set_device_volume_percent(percent)
            print(f"音量调节: 设置为 {percent}%")
        except Exception as err:
            print(f"音量调节失败: {err}")
        maybe_speak_volume_ack(is_tts_busy, is_music_busy, f"好的，音量已调到{percent}%")
        if state.music_mgr is not None:
            state.music_mgr.unduck()
        return True

    percent, up, down, ok = parse_volume_adjust_percent(normalized)
    if ok:
        if up:
            try:
                adjust_device_volume_by_percent(percent, True)
                print(f"音量调节: 加大 {percent}%")
            except Exception as err:
                print(f"音量调节失败: {err}")
            maybe_speak_volume_ack(is_tts_busy, is_music_busy, f"好的，音量已调大{percent}%")
            if state.music_mgr is not None:
                state.music_mgr.unduck()
            return True
        if down:
            try:
                adjust_device_volume_by_percent(percent, False)
                print(f"音量调节: 调小 {percent}%")
            except Exception as err:
                print(f"音量调节失败: {err}")
            maybe_speak_volume_ack(is_tts_busy, is_music_busy, f"好的，音量已调小{percent}%")
            if state.music_mgr is not None:
                state.music_mgr.unduck()
            return True

    up, down, ok = parse_volume_adjust(normalized)
    if ok:
        if up:
            try:
                adjust_device_volume_step(True)
                print(f"音量调节: 加大 {VOLUME_STEP_PERCENT}%")
            except Exception as err:
                print(f"音量调节失败: {err}")
            maybe_speak_volume_ack(is_tts_busy, is_music_busy, f"好的，音量已调大{VOLUME_STEP_PERCENT}%")
            if state.music_mgr is not None:
                state.music_mgr.unduck()
            return True
        if down:
            try:
                adjust_device_volume_step(False)
                print(f"音量调节: 调小 {VOLUME_STEP_PERCENT}%")
            except Exception as err:
                print(f"音量调节失败: {err}")
            maybe_speak_volume_ack(is_tts_busy, is_music_busy, f"好的，音量已调小{VOLUME_STEP_PERCENT}%")
            if state.music_mgr is not None:
                state.music_mgr.unduck()
            return True

    return False


def parse_volume_set_percent(text: str):
    matches = volume_set_re.search(text)
    if not matches:
        cm = volume_set_cn_re.search(text)
        if cm:
            n, ok = parse_number_token(cm.group(1))
            return clamp_percent(n), ok
        return 0, False
    n, ok = parse_number_token(matches.group(1))
    return clamp_percent(n), ok


def parse_volume_adjust_percent(text: str):
    for reg in (volume_increase_re, volume_increase_re2, volume_increase_cn_re, volume_increase_cn_re2):
        m = reg.search(text)
        if m:
            n, ok = parse_number_token(m.group(3))
            return clamp_percent(n), True, False, ok
    for reg in (volume_decrease_re, volume_decrease_re2, volume_decrease_cn_re, volume_decrease_cn_re2):
        m = reg.search(text)
        if m:
            n, ok = parse_number_token(m.group(3))
            return clamp_percent(n), False, True, ok
    return 0, False, False, False


def parse_volume_adjust(text: str):
    if "音量" not in text and "声音" not in text:
        return False, False, False
    if any(k in text for k in ["增大音量", "音量调高", "音量调大", "调大音量", "调高音量", "声音调大",
                               "调大声音", "增大声音", "调高", "提高", "调大", "增大", "加大", "大点"]):
        return True, False, True
    if any(k in text for k in ["降低音量", "音量调低", "音量调小", "音量减小", "调低音量",
                               "调小音量", "降低声音", "声音调小", "调小声音", "声音调低",
                               "调低声音", "减小声音", "降低", "调低", "调小", "减小", "小一点", "小点"]):
        return False, True, True
    return False, False, False


def maybe_speak_volume_ack(is_tts_busy: bool, is_music_busy: bool, ack: str) -> None:
    if is_tts_busy or is_music_busy:
        return
    if not ack.strip():
        return
    state.tts_manager_queue.put(ack)
    state.tts_manager_queue.put("[[END]]")


def clamp_percent(n: int) -> int:
    return max(0, min(100, n))


def parse_number_token(token: str):
    token = token.strip()
    if not token:
        return 0, False
    try:
        return int(token), True
    except ValueError:
        return parse_chinese_number(token)


def parse_chinese_number(s: str):
    s = s.strip().replace("两", "二").replace("〇", "零")
    if s in ("百", "一百") or s.startswith("一百"):
        return 100, True
    if "十" in s:
        parts = s.split("十", 1)
        tens = 1
        if parts[0]:
            tens = cn_digit(parts[0])
            if tens < 0:
                return 0, False
        ones = 0
        if len(parts) > 1 and parts[1]:
            ones = cn_digit(parts[1])
            if ones < 0:
                return 0, False
        return tens * 10 + ones, True
    v = cn_digit(s)
    if v >= 0:
        return v, True
    return 0, False


def cn_digit(s: str) -> int:
    mapping = {"零": 0, "一": 1, "二": 2, "三": 3, "四": 4, "五": 5, "六": 6, "七": 7, "八": 8, "九": 9}
    return mapping.get(s, -1)
