"""Progress-bar fixture: CR-redrawn bar with colors and erase-line, a split
UTF-8 character across two writes, then an error line printed after it."""
import os
import sys
import time

out = sys.stdout.buffer
for pct in range(0, 101, 10):
    bar = "#" * (pct // 10) + "." * (10 - pct // 10)
    out.write(b"\r\x1b[2K\x1b[36mbuilding\x1b[0m [" + bar.encode() + b"] " + str(pct).encode() + b"%")
    out.flush()
    time.sleep(0.005)
out.write(b"\n")
# "caf\xc3\xa9" with the 2-byte char split across two writes.
out.write(b"caf\xc3")
out.flush()
time.sleep(0.02)
out.write(b"\xa9 done\n")
out.flush()
sys.stderr.write("\x1b[1;31mERROR\x1b[0m: tests failed: 3\n")
sys.stderr.flush()
