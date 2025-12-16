package aec

/*
#cgo LDFLAGS: -L../ -lluxaudio -lm
#cgo CFLAGS: -I../

#include <stdlib.h>
#include <stdint.h>

// ------------------------------------------------------------------
// 1. 在这里直接定义结构体，防止找不到头文件
// ------------------------------------------------------------------
typedef struct
{
    void* ptr_algo;      // 算法句柄
    float* ptr_mic_buf;  // ★ 重点：内部使用的是 float 缓冲区

    float cfg_mic_num;
    float cfg_ref_num;
    int frame_size;
    unsigned int frame_counter;
    double frame_time_age;
} objDios_ssp;

// 声明库里的全局变量
extern objDios_ssp* adsp_srv;

// 声明库函数
void* luxnj_algo_init(int mic_num, int ref_num, int frm_len);
int luxnj_algo_process(void* ptr, float* input, int* doa);
int luxnj_algo_destory(void* adsp_srv);

// ------------------------------------------------------------------
// 2. 编写一个 C 辅助函数 (Wrapper)
//    负责：类型转换 (short->float) + 数据重排 + 调用算法
// ------------------------------------------------------------------
int wrap_aec_process(short* input_raw, short* output_clean) {
    // 检查全局指针是否已初始化
    if (!adsp_srv || !adsp_srv->ptr_mic_buf) {
        return -1;
    }

    int frame_size = adsp_srv->frame_size;
    float* internal_buf = adsp_srv->ptr_mic_buf;
    int doa = 0;

    // --- 步骤 A: 数据输入 (Interleaved int16 -> Planar float) ---
    // 严格照搬 c_algodemo.c 的逻辑
    for (int i = 0; i < frame_size; i++) {
        // 提取 8路 Mic (input 通道 0-7)
        internal_buf[0 * frame_size + i] = (float)input_raw[10 * i + 0];
        internal_buf[1 * frame_size + i] = (float)input_raw[10 * i + 1];
        internal_buf[2 * frame_size + i] = (float)input_raw[10 * i + 2];
        internal_buf[3 * frame_size + i] = (float)input_raw[10 * i + 3];
        internal_buf[4 * frame_size + i] = (float)input_raw[10 * i + 4];
        internal_buf[5 * frame_size + i] = (float)input_raw[10 * i + 5];
        internal_buf[6 * frame_size + i] = (float)input_raw[10 * i + 6];
        internal_buf[7 * frame_size + i] = (float)input_raw[10 * i + 7];

        // 提取 1路 Ref (input 通道 8)
        // 注意：c_algodemo.c 里只取了第 8 路，忽略了第 9 路
        internal_buf[8 * frame_size + i] = (float)input_raw[10 * i + 8];
    }

    // --- 步骤 B: 调用核心算法 ---
    // 注意：demo 里是用 internal_buf 既当输入又当输出
    luxnj_algo_process(adsp_srv->ptr_algo, internal_buf, &doa);

    // --- 步骤 C: 数据输出 (Planar float -> int16) ---
    // 取第 0 通道作为降噪后的结果
    for (int i = 0; i < frame_size; i++) {
        output_clean[i] = (short)(internal_buf[0 * frame_size + i]);
    }

    return doa;
}
*/
import "C"
import (
	"unsafe"
)

const (
	FrameSize = 256
	MicCh     = 8 // 实际上配置参数
	RefCh     = 1 // Demo里写的是 1 (虽然输入是10通道，但Ref只用1路)

	// 输入数据仍然是 10 通道 (因为 arecord 录的是 10)
	InputTotalCh = 10
	InputSize    = FrameSize * InputTotalCh
)

type Processor struct {
	// 不需要存 handle 了，C 代码里用的是全局变量
}

func NewProcessor() *Processor {
	// 初始化
	// 注意：根据 c_algodemo.c，这里的 ref_num 传的是 1
	C.luxnj_algo_init(C.int(MicCh), C.int(RefCh), C.int(FrameSize))
	return &Processor{}
}

// Process 处理函数
// input: 256帧 * 10通道 (int16)
// return: 256帧 单声道 (int16), DOA角度
func (p *Processor) Process(input []int16) ([]int16, int) {
	if len(input) != InputSize {
		return nil, 0
	}

	// 准备输出缓冲区
	output := make([]int16, FrameSize)

	// 获取指针
	inPtr := (*C.short)(unsafe.Pointer(&input[0]))
	outPtr := (*C.short)(unsafe.Pointer(&output[0]))

	// 调用我们写的 C wrapper
	doa := C.wrap_aec_process(inPtr, outPtr)

	if doa == -1 {
		// 错误处理：如果未初始化
		return make([]int16, FrameSize), 0
	}

	return output, int(doa)
}
