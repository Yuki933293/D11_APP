import ctypes
import os
from array import array


FRAME_SIZE = 256
MIC_CH = 8
REF_CH = 1
INPUT_TOTAL_CH = 10
INPUT_SIZE = FRAME_SIZE * INPUT_TOTAL_CH


class ObjDiosSsp(ctypes.Structure):
    _fields_ = [
        ("ptr_algo", ctypes.c_void_p),
        ("ptr_mic_buf", ctypes.POINTER(ctypes.c_float)),
        ("cfg_mic_num", ctypes.c_float),
        ("cfg_ref_num", ctypes.c_float),
        ("frame_size", ctypes.c_int),
        ("frame_counter", ctypes.c_uint),
        ("frame_time_age", ctypes.c_double),
    ]


def _load_lux_lib():
    candidates = []
    env_home = os.environ.get("AI_BOX_HOME", "").strip()
    if env_home:
        candidates.append(os.path.join(env_home, "libluxaudio.so"))
    candidates.append(os.path.join(os.getcwd(), "libluxaudio.so"))
    candidates.append(os.path.join(os.path.dirname(__file__), "..", "libluxaudio.so"))
    for p in candidates:
        if os.path.isfile(p):
            return ctypes.CDLL(p)
    return None


class AECProcessor:
    def __init__(self) -> None:
        self.lib = _load_lux_lib()
        if self.lib is None:
            self.available = False
            return
        self.available = True
        self.lib.luxnj_algo_init.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_int]
        self.lib.luxnj_algo_init.restype = ctypes.c_void_p
        self.lib.luxnj_algo_process.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_float), ctypes.POINTER(ctypes.c_int)]
        self.lib.luxnj_algo_process.restype = ctypes.c_int
        self.lib.luxnj_algo_init(MIC_CH, REF_CH, FRAME_SIZE)

    def process(self, input_int16: array):
        if not self.available:
            return None, 0
        if len(input_int16) != INPUT_SIZE:
            return None, 0
        try:
            adsp_ptr = ctypes.POINTER(ObjDiosSsp).in_dll(self.lib, "adsp_srv")
        except ValueError:
            return None, 0
        if not adsp_ptr:
            return None, 0
        adsp = adsp_ptr.contents
        if not adsp.ptr_mic_buf:
            return None, 0

        frame_size = adsp.frame_size
        internal_buf = adsp.ptr_mic_buf

        # Interleaved int16 -> Planar float (8 mic + 1 ref)
        for i in range(frame_size):
            base = i * INPUT_TOTAL_CH
            internal_buf[0 * frame_size + i] = float(input_int16[base + 0])
            internal_buf[1 * frame_size + i] = float(input_int16[base + 1])
            internal_buf[2 * frame_size + i] = float(input_int16[base + 2])
            internal_buf[3 * frame_size + i] = float(input_int16[base + 3])
            internal_buf[4 * frame_size + i] = float(input_int16[base + 4])
            internal_buf[5 * frame_size + i] = float(input_int16[base + 5])
            internal_buf[6 * frame_size + i] = float(input_int16[base + 6])
            internal_buf[7 * frame_size + i] = float(input_int16[base + 7])
            internal_buf[8 * frame_size + i] = float(input_int16[base + 8])

        doa = ctypes.c_int(0)
        ret = self.lib.luxnj_algo_process(adsp.ptr_algo, internal_buf, ctypes.byref(doa))
        if ret == -1:
            return None, 0

        output = array("h", [0] * frame_size)
        for i in range(frame_size):
            v = internal_buf[i]
            if v > 32767.0:
                v = 32767.0
            elif v < -32768.0:
                v = -32768.0
            output[i] = int(v)
        return output, int(doa.value)
