"""Summarize docs/embedded-bench-*.txt into a median comparison table."""

import re
import statistics
import sys
from pathlib import Path

LINE = re.compile(
    r"^(?P<name>\S+?)(?:-\d+)?\s+\d+\s+(?P<ns>[\d.]+) ns/op"
    r"(?:\s+(?P<extra>[\d.]+) \w+/op)?\s+(?P<b>[\d.]+) B/op\s+(?P<allocs>[\d.]+) allocs/op"
)


def load(path):
    cases = {}
    for raw in Path(path).read_text(encoding="utf-8").splitlines():
        match = LINE.match(raw)
        if match:
            name = re.sub(r"-\d+$", "", match.group("name"))
            cases.setdefault(name, []).append(
                (
                    float(match.group("ns")),
                    float(match.group("b")),
                    float(match.group("allocs")),
                )
            )
    return cases


def median(values):
    return statistics.median(values)


def main():
    before = load(sys.argv[1])
    after = load(sys.argv[2])
    names = list(dict.fromkeys(list(before) + list(after)))
    print(f"{'case':44} {'before ms':>10} {'after ms':>10} {'delta':>8} "
          f"{'before B':>10} {'after B':>10} {'before all':>11} {'after all':>10}")
    for name in names:
        b, a = before.get(name, []), after.get(name, [])
        if not b or not a:
            continue
        bns, ans = median([x[0] for x in b]), median([x[0] for x in a])
        bbytes, abytes = median([x[1] for x in b]), median([x[1] for x in a])
        ball, aall = median([x[2] for x in b]), median([x[2] for x in a])
        delta = (ans - bns) / bns * 100
        print(f"{name:44} {bns / 1e6:10.2f} {ans / 1e6:10.2f} {delta:+7.1f}% "
              f"{bbytes:10.0f} {abytes:10.0f} {ball:11.0f} {aall:10.0f}")


if __name__ == "__main__":
    main()
