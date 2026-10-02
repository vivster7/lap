"""Prints the terminal size, then again after each SIGWINCH (up to 1)."""
import os
import signal
import sys
import time

got = []
signal.signal(signal.SIGWINCH, lambda *a: got.append(1))
sz = os.get_terminal_size(1)
print(f"size {sz.columns}x{sz.lines}", flush=True)
deadline = time.time() + 5
while not got and time.time() < deadline:
    time.sleep(0.01)
sz = os.get_terminal_size(1)
print(f"size {sz.columns}x{sz.lines}", flush=True)
