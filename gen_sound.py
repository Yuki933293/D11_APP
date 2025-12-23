import wave
import math
import struct

# 配置：16000Hz, 单声道, 16bit (完美适配 RK3308 aplay)
filename = "thinking.wav"
sample_rate = 16000
duration = 0.3  # 0.3秒
frequency = 800.0 # 800Hz (清脆的提示音)

print(f"正在生成 {filename} ...")

with wave.open(filename, 'w') as wav_file:
    # 设置参数: (nchannels, sampwidth, framerate, nframes, comptype, compname)
    wav_file.setparams((1, 2, sample_rate, int(sample_rate * duration), 'NONE', 'not compressed'))

    # 生成波形数据
    for i in range(int(sample_rate * duration)):
        # 计算时间点
        t = float(i) / sample_rate

        # 生成正弦波
        sample = math.sin(2 * math.pi * frequency * t)

        # 添加淡出效果 (Decay)，让声音听起来像 "叮" 而不是 "哔"
        decay = 1.0 - (t / duration)
        sample *= decay

        # 转换为 16-bit 整数范围 (-32768 to 32767)
        sample = int(sample * 32767.0)

        # 写入数据 (Little Endian)
        wav_file.writeframes(struct.pack('<h', sample))

print("✅ 完成！文件已生成: thinking.wav")


