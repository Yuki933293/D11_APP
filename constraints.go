package main

import "time"

// ================= 1. 常量配置 =================
// 注意：不要把真实 Key 写死在代码里，统一通过环境变量/配置文件注入（见 deploy/ai_box.env.example）。
const DASH_API_KEY = ""

const TTS_WS_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"
const LLM_URL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text-generation/generation"
const WS_AS_URL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference/"

const MUSIC_DIR = "/userdata/AI_BOX/music"

// ================= 1.5 云端伪唤醒配置 =================
// 说明：
// - “伪唤醒”指：仍使用云端 ASR 做文本识别，但在业务层加一层门控状态机；
// - 休眠态只响应唤醒词，其余任何指令（包含 EXIT/INTERRUPT）都忽略；
// - 唤醒后进入 AWAKE 态，超过一定时间无交互且无播放占用时回到休眠态。
const WAKE_IDLE_TIMEOUT = 90 * time.Second
const WAKE_ACK_TEXT = "我在"

// ================= 2. 双级打断词库 =================
var EXIT_WORDS = []string{
	"关闭系统", "关机", "退出程序", "再见", "退下",
	"拜拜", "结束吧", "结束程序", "停止运行", "关闭助手", "关闭",
}

var INTERRUPT_WORDS = []string{
	"闭嘴", "停止", "安静", "别说了", "暂停", "打断", "等一下", "不要说了",
}

// ================= 2.5 云端伪唤醒词库 =================
// 注意：这里放一些常见同音/误识别变体，尽量提高“唤醒命中率”。
var WAKE_WORDS = []string{
	"你好小瑞", "你好小睿", "你好晓瑞",
	"小瑞", "小睿", "晓瑞",
}
