import subprocess

VOLUME_CARD_INDEX = 1
VOLUME_CONTROL_SIMPLE = "aw_dev_0_rx_volume"
VOLUME_CONTROL_INDEXED = "aw_dev_0_rx_volume,0"
VOLUME_STEP_PERCENT = 5
VOLUME_RAW_MIN = 0
VOLUME_RAW_MAX = 1023


def set_alsa_volume(control: str, percent: int) -> None:
    if not control.strip():
        raise ValueError("控件名不能为空")
    if percent < 0 or percent > 100:
        raise ValueError("音量范围必须在 0~100 之间")
    raw = percent_to_raw(percent)
    run_amixer_with_card(VOLUME_CARD_INDEX, "sset", control, str(raw))


def set_device_volume_percent(percent: int) -> None:
    set_alsa_volume(VOLUME_CONTROL_INDEXED, percent)


def adjust_device_volume_step(up: bool) -> None:
    current_raw = get_current_raw_volume(VOLUME_CONTROL_SIMPLE)
    step_raw = raw_step_from_percent(VOLUME_STEP_PERCENT)
    if up:
        current_raw -= step_raw
    else:
        current_raw += step_raw
    current_raw = clamp_raw(current_raw)
    run_amixer_with_card(VOLUME_CARD_INDEX, "sset", VOLUME_CONTROL_INDEXED, str(current_raw))


def adjust_device_volume_by_percent(percent: int, up: bool) -> None:
    percent = max(0, min(100, percent))
    current_raw = get_current_raw_volume(VOLUME_CONTROL_SIMPLE)
    step_raw = raw_step_from_percent(percent)
    if up:
        current_raw -= step_raw
    else:
        current_raw += step_raw
    current_raw = clamp_raw(current_raw)
    run_amixer_with_card(VOLUME_CARD_INDEX, "sset", VOLUME_CONTROL_INDEXED, str(current_raw))


def run_amixer_with_card(card: int, *args: str) -> None:
    cmd_args = list(args)
    if card >= 0:
        cmd_args = ["-c", str(card)] + cmd_args
    result = subprocess.run(["amixer"] + cmd_args, capture_output=True, text=True)
    if result.returncode != 0:
        raise RuntimeError(f"amixer 执行失败: {result.stderr.strip()} {result.stdout.strip()}")


def run_amixer_output(card: int, *args: str) -> str:
    cmd_args = list(args)
    if card >= 0:
        cmd_args = ["-c", str(card)] + cmd_args
    result = subprocess.run(["amixer"] + cmd_args, capture_output=True, text=True)
    if result.returncode != 0:
        raise RuntimeError(f"amixer 执行失败: {result.stderr.strip()} {result.stdout.strip()}")
    return result.stdout


def percent_to_raw(percent: int) -> int:
    percent = max(0, min(100, percent))
    range_raw = VOLUME_RAW_MAX - VOLUME_RAW_MIN
    raw = VOLUME_RAW_MAX - (range_raw * percent) // 100
    return clamp_raw(raw)


def raw_step_from_percent(step: int) -> int:
    if step <= 0:
        return 0
    step = min(step, 100)
    range_raw = VOLUME_RAW_MAX - VOLUME_RAW_MIN
    val = (range_raw * step) // 100
    return max(1, val)


def clamp_raw(val: int) -> int:
    if val < VOLUME_RAW_MIN:
        return VOLUME_RAW_MIN
    if val > VOLUME_RAW_MAX:
        return VOLUME_RAW_MAX
    return val


def get_current_raw_volume(control: str) -> int:
    if not control.strip():
        raise ValueError("控件名不能为空")
    out = run_amixer_output(VOLUME_CARD_INDEX, "cget", f"name='{control}'")
    for line in out.splitlines():
        line = line.strip()
        if line.startswith(": values="):
            val_str = line[len(": values="):].strip()
            return clamp_raw(int(val_str))
    raise RuntimeError("未能解析当前音量")
