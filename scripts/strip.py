#!/usr/bin/env python3
"""Remove named top-level Go declarations (and their doc comments) from a file.

    python3 strip.py <file.go> Name1 Name2 ...

Only whole declarations are removed: for `func`/`type`/`var`/`const` the script
finds the opening brace and scans to its match, so nested braces and composite
literals are handled. Comments immediately above a declaration go with it, and
overlapping ranges are merged before anything is deleted.
"""
import re
import sys


def decl_start(lines, name):
    pat = re.compile(r"^(func|type|var|const)\s+" + re.escape(name) + r"\b")
    for i, line in enumerate(lines):
        if pat.match(line):
            return i
    return None


def end_of_decl(lines, start):
    depth = 0
    seen = False
    for i in range(start, len(lines)):
        for ch in lines[i]:
            if ch == "{":
                depth += 1
                seen = True
            elif ch == "}":
                depth -= 1
        if seen and depth <= 0:
            return i
        if not seen and lines[i].rstrip().endswith("}"):
            return i
    return len(lines) - 1


def doc_start(lines, start):
    i = start
    while i > 0 and lines[i - 1].strip().startswith("//"):
        i -= 1
    return i


def main():
    path = sys.argv[1]
    names = sys.argv[2:]
    lines = open(path, encoding="utf-8").read().splitlines(keepends=True)

    ranges = []
    for name in names:
        start = decl_start(lines, name)
        if start is None:
            print(f"  not found: {name}")
            continue
        ranges.append((doc_start(lines, start), end_of_decl(lines, start), name))

    merged = []
    for begin, end, name in sorted(ranges):
        if merged and begin <= merged[-1][1] + 1:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end), merged[-1][2] + "+" + name)
            continue
        merged.append((begin, end, name))

    for begin, end, name in reversed(merged):
        del lines[begin:end + 1]
        print(f"  removed {name}: lines {begin + 1}-{end + 1}")

    open(path, "w", encoding="utf-8", newline="").write("".join(lines))


if __name__ == "__main__":
    main()
