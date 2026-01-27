# -*- mode: python ; coding: utf-8 -*-
from PyInstaller.utils.hooks import collect_submodules

hiddenimports = []
hiddenimports += collect_submodules('py_ai_box')
hiddenimports += [
    'py_ai_box.music_manager',
    'py_ai_box.aec',
    'py_ai_box.asr',
    'py_ai_box.audio_loop',
    'py_ai_box.audio_player',
    'py_ai_box.config_runtime',
    'py_ai_box.constraints',
    'py_ai_box.control',
    'py_ai_box.intent',
    'py_ai_box.llm',
    'py_ai_box.main',
    'py_ai_box.state',
    'py_ai_box.tts',
    'py_ai_box.util',
    'py_ai_box.vad',
    'py_ai_box.volume_control',
    'py_ai_box.volume_intent',
    'py_ai_box.wake',
    'py_ai_box.websocket_client',
]


a = Analysis(
    ['py_ai_box/main.py'],
    pathex=['.'],
    binaries=[],
    datas=[('py_ai_box', 'py_ai_box')],
    hiddenimports=hiddenimports,
    hookspath=[],
    hooksconfig={},
    runtime_hooks=[],
    excludes=[],
    noarchive=False,
    optimize=0,
)
pyz = PYZ(a.pure)

exe = EXE(
    pyz,
    a.scripts,
    a.binaries,
    a.datas,
    [],
    name='ai_box_py',
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    upx=False,
    upx_exclude=[],
    runtime_tmpdir=None,
    console=True,
    disable_windowed_traceback=False,
    argv_emulation=False,
    target_arch=None,
    codesign_identity=None,
    entitlements_file=None,
)
