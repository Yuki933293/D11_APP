def flush_queue(q):
    while True:
        try:
            q.get_nowait()
        except Exception:
            return
