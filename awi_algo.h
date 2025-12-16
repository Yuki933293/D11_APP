#ifndef AWI_ALGO_H_
#define AWI_ALGO_H_

typedef struct
{
	void* ptr_algo;   // howling suppression
	float* ptr_mic_buf;

	float cfg_mic_num;
	float cfg_ref_num;
	int frame_size;
	unsigned int frame_counter;
	double frame_time_age;

} objDios_ssp;

extern objDios_ssp* adsp_srv;

void* luxnj_algo_init(int mic_num, int ref_num, int frm_len);

int luxnj_algo_process(void* ptr, float* input, int* doa);

int luxnj_algo_destory(void* adsp_srv);

void* awi_algo_init(int mic_num, int ref_num, int frm_len);

int awi_algo_deinit(void* algo);


#endif



