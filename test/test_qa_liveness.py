"""Unit guard for the qa_matrix liveness gate (#141b).

qa_matrix.py is a live PMS-driving harness — most of it can't run without a
cluster — but `_scan_liveness` is pure log parsing, so we exercise it with
synthetic worker-log fixtures here. This is the unit-level form of #141's
"revert #138 -> must FAIL" acceptance: a session that wrote a frame then froze
mid-stream (out_time_us not advancing) must be detectable from the log alone,
the class the throttled progress heartbeat (#166) was added to expose.

Run: python3 -m pytest test/test_qa_liveness.py  (or: python3 test/test_qa_liveness.py)
"""
import os
import sys

sys.path.insert(0, os.path.dirname(__file__))
import qa_matrix  # noqa: E402

SLUG = "ghostss02e01"


def _scan(monkey_lines):
    """Feed fixed log text through _scan_liveness regardless of the `since` arg."""
    qa_matrix.worker_logs = lambda since: monkey_lines
    return qa_matrix._scan_liveness(SLUG, "60s")


def _line(rest):
    return f"2026/06/01 12:00:00 session {SLUG}_mp4-1-abcd: {rest}"


def test_healthy_advancing():
    """First block + a heartbeat with a higher out_time_us = advancing PASS."""
    logs = "\n".join([
        _line("first progress block out_time_us=500000 total_size=1 speed=2x base_url=x"),
        _line("progress heartbeat ticks=12 out_time_us=5500000 speed=2x"),
    ])
    progressed, advanced, beats, exited = _scan(logs)
    assert progressed is True
    assert advanced is True
    assert beats == 1
    assert exited is None


def test_mid_stream_stall_frozen_out_time():
    """Encoded one frame, heartbeat fired but out_time_us never climbed: the
    #166 mid-stream stall. advanced must be False so drive_cell FAILs it."""
    logs = "\n".join([
        _line("first progress block out_time_us=500000 total_size=1 speed=0x base_url=x"),
        _line("progress heartbeat ticks=3 out_time_us=500000 speed=0x"),
    ])
    progressed, advanced, beats, exited = _scan(logs)
    assert progressed is True
    assert advanced is False
    assert beats == 1


def test_init_only_no_frame():
    """Init segment then nothing — no out_time_us at all. progressed False."""
    progressed, advanced, beats, exited = _scan(_line("first segment ready: init-stream0.m4s"))
    assert progressed is False
    assert advanced is None
    assert beats == 0


def test_single_sample_inconclusive():
    """One out_time_us sample (short soak / heavy throttle): can't prove a
    stall, so advanced is None — drive_cell must NOT FAIL on it."""
    progressed, advanced, beats, exited = _scan(
        _line("first progress block out_time_us=500000 total_size=1 speed=2x base_url=x"))
    assert progressed is True
    assert advanced is None
    assert beats == 0


def test_premature_exit_captured():
    """A clean exit-0 after the init segment is still a premature termination."""
    logs = "\n".join([
        _line("first progress block out_time_us=500000 total_size=1 speed=2x base_url=x"),
        _line("ffmpeg exit: exit status 0"),
    ])
    progressed, advanced, beats, exited = _scan(logs)
    assert exited is not None
    assert "exit status 0" in exited


def test_exit_145_family_captured():
    """libass/fontconfig exit 145 (the #141 escape) surfaces as `exited`."""
    logs = "\n".join([
        _line("first progress block out_time_us=500000 total_size=1 speed=2x base_url=x"),
        _line('ffmpeg exit: exit status 145 stderr_tail=Fontconfig error: Cannot load config'),
    ])
    _, _, _, exited = _scan(logs)
    assert exited is not None
    assert "exit status 145" in exited
    assert "stderr_tail" not in exited  # trimmed to the code, not the dump


if __name__ == "__main__":
    fns = [v for k, v in sorted(globals().items()) if k.startswith("test_") and callable(v)]
    for fn in fns:
        fn()
        print(f"ok  {fn.__name__}")
    print(f"\n{len(fns)} passed")
