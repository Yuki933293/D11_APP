import time

# ================= 1. 常量配置 =================
# 注意：不要把真实 Key 写死在代码里，统一通过环境变量/配置文件注入（见 deploy/ai_box.env）。
DASH_API_KEY = ""

TTS_WS_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"
LLM_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text-generation/generation"
WS_ASR_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"

MUSIC_DIR = "/userdata/AI_BOX/music"

# ================= 1.5 云端伪唤醒配置 =================
WAKE_IDLE_TIMEOUT = 90.0  # seconds
WAKE_ACK_TEXT = "我在"

# ================= 2. 双级打断词库 =================
EXIT_WORDS = [
    "关闭系统", "关机", "退出程序", "再见", "退下",
    "拜拜", "结束吧", "结束程序", "停止运行", "关闭助手", "关闭",
]

INTERRUPT_WORDS = [
    "闭嘴", "停止", "安静", "别说了", "暂停", "打断", "等一下", "不要说了",
]

# ================= 2.5 云端伪唤醒词库 =================
WAKE_WORDS = [
    "你好小瑞", "你好小睿", "你好晓瑞",
    "小瑞", "小睿", "晓瑞",
]

def seconds_to_duration(seconds: float) -> float:
    # 统一使用秒数，保持与 Go 逻辑一致
    return float(seconds)
