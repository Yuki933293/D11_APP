from . import state
from .constraints import EXIT_WORDS, INTERRUPT_WORDS


def clean_text(text: str) -> str:
    return state.emoji_regex.sub("", text).strip()


def is_exit(text: str) -> bool:
    return any(w in text for w in EXIT_WORDS)


def is_interrupt(text: str) -> bool:
    return any(w in text for w in INTERRUPT_WORDS)


def has_music_intent(text: str) -> bool:
    music_keywords = ["播放", "想要听", "要听"]
    return any(k in text for k in music_keywords)


def is_quick_switch(text: str) -> bool:
    normalized = state.music_punct.sub("", text.lower().strip())
    switch_words = ["换首歌", "下一首", "切歌"]
    return any(w in normalized for w in switch_words)
