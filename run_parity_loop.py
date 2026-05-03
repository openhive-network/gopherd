#!/usr/bin/env python3
"""
Loops `claude --print` against the same task until it either reports the
work is finished or hits a credit/quota error. First turn seeds the prompt;
subsequent turns use `--continue` to resume the same session.

Uses --output-format=stream-json so we can pretty-print live progress
(tool calls, partial assistant text, rate-limit events) instead of waiting
for the final --print result blob.
"""

import json
import os
import re
import subprocess
import sys
import time
from datetime import datetime

PROMPT_INITIAL = (
    "Continue implementing features until you have exact feature parity "
    "with the @../hive/ c++ implementation and can replay the block_log fully. "
    "You also need to have full unit tests parity. "
    "NOTE: this session is being driven by a self-restart loop (run_parity_loop.py); "
    "if you see a background task running this loop, leave it alone — it is the "
    "harness that will re-invoke you with `--continue` after each turn. Do not "
    "TaskStop it, do not ask the user for direction; just keep working."
)

PROMPT_CONTINUE = (
    "Continue the parity work from where you left off. Do not stop, do not ask "
    "for confirmation. The background task running this loop is the harness "
    "that re-invoked you — do not stop it."
)

LOG_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "parity_loop_logs")
os.makedirs(LOG_DIR, exist_ok=True)

CREDIT_RE = re.compile(
    r"rate.?limit|usage.?limit|quota|out of (tokens|credits)|"
    r"too many requests|insufficient (credits|quota)|"
    r"credit balance is too low|max[_-]?budget|payment required",
    re.IGNORECASE,
)

DONE_RE = re.compile(
    r"\bfeature parity (?:is )?(?:complete|achieved|done)\b|"
    r"\bfull (?:feature )?parity (?:is )?(?:complete|achieved|reached)\b|"
    r"\ball (?:tasks|features|tests) (?:are )?complete\b|"
    r"\bnothing (?:left|more) to (?:do|implement)\b|"
    r"\btask(?:s)? (?:is|are) complete\b|"
    r"\breplay(?:s)? (?:fully|completely) (?:successfully|matches)\b",
    re.IGNORECASE,
)


class Renderer:
    """Stateful pretty-printer for stream-json events.

    Streams text_delta chunks live (no duplication from final `assistant`
    events). Shows [thinking…] markers when claude opens a thinking block
    (Opus 4.x emits encrypted signatures, so we cannot show the prose).
    Tool calls are suppressed by request.
    """

    def __init__(self) -> None:
        self.in_text = False
        self.in_thinking = False
        self.text_buffer = ""
        self.line_partial = ""  # for word-wrap-friendly streaming

    def _write(self, s: str) -> None:
        sys.stdout.write(s)
        sys.stdout.flush()

    def _emit_line(self, s: str) -> None:
        if self.line_partial:
            self._write("\n")
            self.line_partial = ""
        self._write(s + "\n")

    def _flush_partial(self) -> None:
        if self.line_partial:
            self._write("\n")
            self.line_partial = ""

    def feed(self, ev: dict) -> tuple[bool, str]:
        """Returns (rate_limited, text_appended_for_pattern_matching)."""
        t = ev.get("type")
        rate_limited = False

        if t == "system" and ev.get("subtype") == "init":
            sid = ev.get("session_id", "?")[:8]
            self._emit_line(f"  [system] session={sid} model={ev.get('model')}")
            return False, ""

        if t == "rate_limit_event":
            info = ev.get("rate_limit_info", {})
            self._emit_line(
                f"  [rate-limit] {info.get('rateLimitType')} "
                f"status={info.get('status')} util={info.get('utilization')}"
            )
            status = (info.get("status") or "").lower()
            if status in ("blocked", "exceeded", "throttled") or info.get("isUsingOverage"):
                rate_limited = True
            return rate_limited, ""

        if t == "stream_event":
            inner = ev.get("event", {})
            it = inner.get("type")
            if it == "content_block_start":
                cb = inner.get("content_block", {})
                cbt = cb.get("type")
                if cbt == "thinking":
                    self.in_thinking = True
                    self._emit_line("  [thinking…]")
                elif cbt == "text":
                    self.in_text = True
                    self._write("  ")
                # tool_use start: suppressed
            elif it == "content_block_delta":
                d = inner.get("delta", {})
                dt = d.get("type")
                if dt == "text_delta" and self.in_text:
                    chunk = d.get("text", "")
                    self.text_buffer += chunk
                    # stream into stdout, indenting newlines
                    parts = chunk.split("\n")
                    self._write(parts[0])
                    self.line_partial += parts[0]
                    for piece in parts[1:]:
                        self._write("\n  ")
                        self.line_partial = piece
                        self._write(piece)
                    return False, chunk
                # thinking_delta has no plaintext for opus 4.x (signed only)
            elif it == "content_block_stop":
                if self.in_text:
                    self._flush_partial()
                self.in_text = False
                self.in_thinking = False
            return False, ""

        if t == "result":
            self._flush_partial()
            self._emit_line(
                f"  [result] subtype={ev.get('subtype')} "
                f"cost=${ev.get('total_cost_usd', 0):.4f} "
                f"turns={ev.get('num_turns')} dur={ev.get('duration_ms', 0)}ms"
            )
            return False, ""

        # assistant/user events would duplicate streaming text — suppress
        return False, ""


def run_iteration(iteration: int, fresh: bool) -> tuple[int, str, bool]:
    """Returns (return_code, accumulated_assistant_text, rate_limited)."""
    cmd = [
        "claude",
        "--verbose",
        "--permission-mode", "auto",
        "--output-format", "stream-json",
        "--include-partial-messages",
        "--print",
    ]
    if fresh:
        cmd.append(PROMPT_INITIAL)
    else:
        cmd.extend(["--continue", PROMPT_CONTINUE])

    ts = datetime.now().strftime("%Y%m%d_%H%M%S")
    log_path = os.path.join(LOG_DIR, f"iter_{iteration:04d}_{ts}.log")

    print(f"\n[{ts}] iteration {iteration} -> {log_path}")
    print(f"  cmd: claude --print {'(continue)' if not first else '(initial)'} stream-json")

    assistant_text = ""
    rate_limited = False
    renderer = Renderer()

    with open(log_path, "w") as logf:
        logf.write(f"# iteration {iteration} @ {ts}\n# cmd: {cmd}\n\n")
        logf.flush()
        proc = subprocess.Popen(
            cmd,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
        )
        assert proc.stdout is not None
        for line in proc.stdout:
            logf.write(line)
            logf.flush()
            stripped = line.rstrip("\n")
            if not stripped:
                continue
            try:
                ev = json.loads(stripped)
            except json.JSONDecodeError:
                sys.stdout.write(line)
                sys.stdout.flush()
                continue
            rl, txt = renderer.feed(ev)
            if rl:
                rate_limited = True
            if txt:
                assistant_text += txt
        rc = proc.wait()

    return rc, assistant_text, rate_limited


def main() -> int:
    fresh_start = os.environ.get("FRESH_START") == "1"
    print(">>> run_parity_loop.py — claude stream-json loop, permission-mode=auto")
    print(f">>> mode: {'FRESH (new session)' if fresh_start else 'RESUME (--continue from existing session)'}")
    print(">>> stdin closed (DEVNULL); ctrl-c to stop.")

    iteration = 0
    consecutive_failures = 0
    max_consecutive_failures = 3

    while True:
        iteration += 1
        try:
            # Only the very first iteration of a fresh start uses the initial prompt.
            rc, text, rate_limited = run_iteration(
                iteration, fresh=(fresh_start and iteration == 1)
            )
        except KeyboardInterrupt:
            print("\ninterrupted by user")
            return 130

        if rate_limited or CREDIT_RE.search(text):
            print(f"\n>>> credit/quota exhaustion detected on iteration {iteration}; stopping.")
            return 0

        if rc != 0:
            consecutive_failures += 1
            print(
                f">>> claude exited with rc={rc} "
                f"(failure {consecutive_failures}/{max_consecutive_failures})"
            )
            if consecutive_failures >= max_consecutive_failures:
                print(">>> too many consecutive failures; stopping.")
                return rc
            time.sleep(10)
            continue
        else:
            consecutive_failures = 0

        if DONE_RE.search(text):
            print(f"\n>>> completion signal detected on iteration {iteration}; stopping.")
            return 0

        time.sleep(2)


if __name__ == "__main__":
    sys.exit(main())
