#!/usr/bin/env python3
"""Verify that a gateway model actually reaches Claude Code's /model picker.

The picker is the one part of this gateway that no unit test can cover: it lives
inside Claude Code, is driven by an undocumented cache file, and only renders in
a real terminal. This script drives the real TUI in a pty, opens /model, and
reports whether an expected row is there.

    scripts/verify-picker.py http://127.0.0.1:8787 "GPT-5.6"

Exits 0 when the row is found, 1 when it is not, so it also serves as the
negative control: remove the cache with `ccgw sync-picker -remove`, run again,
and the same command should now fail. Only the pair of results proves anything —
a row that is present in both runs did not come from the cache.

Notes for anyone changing this:

  * The window size is load-bearing. pty.fork() leaves the terminal at its
    default, and Claude Code will not lay out the picker overlay without a real
    TIOCSWINSZ, which is why a naive version of this script captures a booted
    UI and no picker.
  * It cancels with Esc rather than Enter. Enter would select the highlighted
    row and persist it as the user's default model.
  * Run it from a directory Claude Code already trusts, or it stops on the
    folder-trust prompt instead of reaching the picker.
"""
import os
import pty
import sys
import time
import select
import re
import signal
import fcntl
import termios
import struct

COLS, ROWS = 200, 55
BOOT_SECONDS = 18
PICKER_SECONDS = 12

CSI = re.compile(rb"\x1b\[[0-9;?]*[ -/]*[@-~]")
OSC = re.compile(rb"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)")
OTHER = re.compile(rb"\x1b[()][B0]|\x1b[=>]|\x1b[PX^_][^\x1b]*\x1b\\")


def plain(raw: bytes) -> str:
    """Strip the escape sequences a TUI uses, leaving the visible text."""
    return OTHER.sub(b"", CSI.sub(b"", OSC.sub(b"", raw))).decode("utf-8", "replace")


def squeeze(text: str) -> str:
    """Drop all whitespace, so a match survives the TUI's layout.

    The picker lays rows out in columns and wraps them at the window edge, so a
    label can arrive split across a line break with its spaces collapsed:
    "GPT-5.6 Luna (Codex)" renders as "…8. GPT-5.6Luna(Codex)OpenAI GPT-5.6…".
    An exact substring search then reports a row that is plainly on screen as
    missing — which, in the negative-control half of the Makefile targets, reads
    as proof that the mechanism under test does not work.
    """
    return "".join(text.split())


def child_env(base_url: str) -> dict:
    env = dict(os.environ)
    # A nested Claude Code session inherits markers that change its behaviour,
    # and any credential override would defeat the point of the check.
    for key in list(env):
        if key.startswith(("CLAUDE_CODE_", "CLAUDE_")) or key in (
            "CLAUDECODE",
            "ANTHROPIC_API_KEY",
            "ANTHROPIC_AUTH_TOKEN",
        ):
            env.pop(key, None)
    env.update(
        {
            "ANTHROPIC_BASE_URL": base_url,
            "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
            "TERM": "xterm-256color",
            "COLUMNS": str(COLS),
            "LINES": str(ROWS),
        }
    )
    return env


def capture(base_url: str) -> str:
    pid, fd = pty.fork()
    if pid == 0:
        os.execvpe("claude", ["claude"], child_env(base_url))

    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
    os.set_blocking(fd, False)
    buf = b""

    def pump(seconds: float) -> None:
        nonlocal buf
        end = time.time() + seconds
        while time.time() < end:
            ready, _, _ = select.select([fd], [], [], 0.2)
            if not ready:
                continue
            try:
                chunk = os.read(fd, 65536)
            except OSError:
                return
            if not chunk:
                return
            buf += chunk

    try:
        pump(BOOT_SECONDS)
        os.write(fd, b"/model")
        pump(4)  # let the slash-command autocomplete settle
        os.write(fd, b"\r")
        pump(PICKER_SECONDS)
        os.write(fd, b"\x1b")  # cancel; never select, never write a setting
        pump(2)
    finally:
        try:
            os.kill(pid, signal.SIGTERM)
            time.sleep(1)
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass

    return plain(buf)


def main() -> int:
    if len(sys.argv) < 3:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    base_url, expected = sys.argv[1], sys.argv[2]

    text = capture(base_url)
    if "Is this a project you created or one you trust" in text:
        print("stopped at the folder-trust prompt; run from a trusted directory", file=sys.stderr)
        return 2

    rows = re.findall(r"\d\.\s*[^\n]{0,90}", text)
    if rows:
        print("picker rows:")
        for row in dict.fromkeys(r.strip() for r in rows):
            print("   ", row[:100])
    else:
        print("no picker rows found; the TUI may not have reached /model", file=sys.stderr)

    if squeeze(expected) in squeeze(text):
        print(f"\nOK: {expected!r} is in the picker")
        return 0
    print(f"\nMISSING: {expected!r} is not in the picker", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
