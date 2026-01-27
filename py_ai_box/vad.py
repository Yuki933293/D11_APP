import ctypes
import math
import os
from array import array

class WebRtcVAD:
    def __init__(self, lib, mode: int) -> None:
        self.lib = lib
        self.inst = ctypes.c_void_p()
        if self.lib.WebRtcVad_Create(ctypes.byref(self.inst)) != 0:
            raise RuntimeError("failed to create VAD")
        if self.lib.WebRtcVad_Init(self.inst) != 0:
            raise RuntimeError("failed to init VAD")
        self.lib.WebRtcVad_set_mode(self.inst, mode)

    def is_speech(self, data) -> bool:
        if not data:
            return False
        if isinstance(data, array):
            buf = data
        else:
            buf = array("h", data)
        ptr = (ctypes.c_int16 * len(buf)).from_buffer(buf)
        ret = self.lib.WebRtcVad_Process(self.inst, 16000, ptr, len(buf))
        return ret == 1


def _load_webrtc_lib():
    candidates = []
    env_home = os.environ.get("AI_BOX_HOME", "").strip()
    if env_home:
        candidates.append(os.path.join(env_home, "libwebrtcvad.so"))
    candidates.append(os.path.join(os.getcwd(), "libwebrtcvad.so"))
    for p in candidates:
        if os.path.isfile(p):
            lib = ctypes.CDLL(p)
            lib.WebRtcVad_Create.argtypes = [ctypes.POINTER(ctypes.c_void_p)]
            lib.WebRtcVad_Create.restype = ctypes.c_int
            lib.WebRtcVad_Init.argtypes = [ctypes.c_void_p]
            lib.WebRtcVad_Init.restype = ctypes.c_int
            lib.WebRtcVad_set_mode.argtypes = [ctypes.c_void_p, ctypes.c_int]
            lib.WebRtcVad_set_mode.restype = ctypes.c_int
            lib.WebRtcVad_Process.argtypes = [ctypes.c_void_p, ctypes.c_int, ctypes.POINTER(ctypes.c_int16), ctypes.c_int]
            lib.WebRtcVad_Process.restype = ctypes.c_int
            return lib
    return None


def _parse_mode(val: str, default: int) -> int:
    try:
        n = int(val)
    except Exception:
        return default
    if n < 0:
        return 0
    if n > 3:
        return 3
    return n


def create_vad_engine():
    mode = _parse_mode(os.environ.get("AI_BOX_VAD_MODE", "").strip(), 3)
    lib = _load_webrtc_lib()
    if lib is not None:
        return WebRtcVAD(lib, mode)
    raise RuntimeError("libwebrtcvad.so not found; WebRTC VAD is required")
