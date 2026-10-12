#!/usr/bin/env python3
"""Turn an ocgo trace into sanitised replay fixtures.

    python3 scripts/makefixtures.py <trace.jsonl> [more.jsonl ...] testdata/fixtures

Emits, per inbound request:

    responses/<n>_<kind>.json     the Responses request Codex sent
    chat_stream/<n>_<kind>.sse    the chat-completions SSE the upstream returned
    codex_events/<n>_<kind>.sse   the Responses SSE the bridge sent back

Sanitisation is deterministic: every UUID becomes a fixed fake UUID, the
recorded workspace path becomes /work/repo, and installation ids are zeroed, so
fixtures are stable across runs and contain no host or account identifiers.
"""
import json
import pathlib
import re
import sys

FAKE_UUIDS = [
    "11111111-1111-4111-8111-111111111111",
    "22222222-2222-4222-8222-222222222222",
    "33333333-3333-4333-8333-333333333333",
    "44444444-4444-4444-8444-444444444444",
]
UUID_RE = re.compile(r"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b")
PATH_RE = re.compile(r"/home/[A-Za-z0-9_.-]+/ocgo-capture/repo|/home/[A-Za-z0-9_.-]+/ocgo-compact/repo")
INSTALL_RE = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")


class Sanitizer:
    def __init__(self):
        self.uuids = {}

    def uuid(self, value):
        if value not in self.uuids:
            self.uuids[value] = FAKE_UUIDS[len(self.uuids) % len(FAKE_UUIDS)]
        return self.uuids[value]

    def text(self, s):
        s = UUID_RE.sub(lambda m: self.uuid(m.group(0)), s)
        s = PATH_RE.sub("/work/repo", s)
        return s

    def obj(self, v):
        if isinstance(v, str):
            return self.text(v)
        if isinstance(v, list):
            return [self.obj(x) for x in v]
        if isinstance(v, dict):
            out = {}
            for k, val in v.items():
                if k == "x-codex-installation-id" or k == "installation_id":
                    out[k] = "00000000-0000-4000-8000-000000000000"
                else:
                    out[k] = self.obj(val)
            return out
        return v


def load(path):
    return [json.loads(l) for l in path.read_text().splitlines() if l.strip()]


def kind_of(body):
    cm = body.get("client_metadata") or {}
    raw = cm.get("x-codex-turn-metadata")
    if isinstance(raw, str):
        try:
            return json.loads(raw).get("request_kind") or "turn"
        except Exception:
            pass
    return "turn"


def main():
    args = sys.argv[1:]
    if len(args) < 2:
        print(__doc__)
        return 2
    outdir = pathlib.Path(args[-1])
    traces = [pathlib.Path(p) for p in args[:-1]]

    for sub in ("responses", "chat_stream", "codex_events"):
        (outdir / sub).mkdir(parents=True, exist_ok=True)

    san = Sanitizer()
    n = 0
    for trace in traces:
        recs = load(trace)
        # Positions, not values: list.index compares records by equality and
        # would collapse identical-looking exchanges onto the first match.
        starts = [i for i, r in enumerate(recs) if r["kind"] == "inbound_request"]
        for i, start in enumerate(starts):
            r = recs[start]
            body = r["body"]
            if isinstance(body, str):
                body = json.loads(body)
            kind = kind_of(body)
            name = f"{n:03d}_{kind}"
            n += 1

            (outdir / "responses" / f"{name}.json").write_text(
                json.dumps(san.obj(body), indent=1) + "\n")

            # Everything between this inbound request and the next belongs to
            # the same exchange.
            end = starts[i + 1] if i + 1 < len(starts) else len(recs)
            window = recs[start:end]

            chunks = [x["text"] for x in window if x["kind"] == "upstream_chunk"]
            events = [x["text"] for x in window if x["kind"] == "outbound_event"]
            (outdir / "chat_stream" / f"{name}.sse").write_text(
                san.text("\n\n".join(chunks)) + "\n")
            (outdir / "codex_events" / f"{name}.sse").write_text(
                san.text("\n\n".join(events)) + "\n")

    print(f"wrote {n} fixture sets to {outdir}")
    print("uuid map:", json.dumps(san.uuids, indent=1))
    return 0


if __name__ == "__main__":
    sys.exit(main())
