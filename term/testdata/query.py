"""Terminal query fixture.

Prints a marker, sends DSR 6 (cursor position report) and waits for the
reply the way TUI libraries do: raw mode on the terminal, then read until 'R'.

argv[1]: timeout in seconds, or "none" to block forever (like code that
assumes a real terminal always answers).
"""
import os
import select
import sys
import termios
import tty

timeout = None if sys.argv[1] == "none" else float(sys.argv[1])
fd = 0 if os.isatty(0) else os.open("/dev/tty", os.O_RDWR)
old = termios.tcgetattr(fd)
sys.stdout.write("before")
sys.stdout.flush()
tty.setraw(fd)
try:
    os.write(1, b"\x1b[6n")
    buf = b""
    while not buf.endswith(b"R"):
        r, _, _ = select.select([fd], [], [], timeout)
        if not r:
            break
        buf += os.read(fd, 32)
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, old)
if buf.endswith(b"R"):
    print("\nREPLY " + buf[2:-1].decode())
else:
    print("\nNO-REPLY")
