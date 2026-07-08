#!/usr/bin/env python3
"""Modal ephemerality e2e: prove harness session durability survives an abrupt
sandbox kill on a Modal Volume.

This is an on-demand test (NOT run in CI). It:

  1. builds a linux/amd64 harness binary (CGO_ENABLED=0),
  2. launches a Modal Sandbox (golang:1.25 + the binary) running `harness serve`
     behind an encrypted-ports tunnel, backed by a named Modal Volume,
  3. creates a session and drives one tiny real prompt through the tunnel using
     the real ANTHROPIC_API_KEY from the environment (a bash `echo`),
  4. records the message count N and max event seq,
  5. `sb.terminate()`s the sandbox abruptly (no graceful shutdown),
  6. relaunches a fresh sandbox on the SAME Volume and asserts the message count
     is still N, the event journal's seq continues above the prior max (no reset),
     and the session is still promptable with one more tiny prompt.

Volumes v2 (`version=2`) sync continuously and must retain everything. The
optional --classic-volume flag repeats the flow on a version=1 Volume as an
INFORMATIONAL negative control: classic Volumes commit in the background and can
lose the tail of a log on an abrupt kill. The control never affects the exit
code — it just reports the delta.

Prereqs:
  - Modal auth in ~/.modal.toml
  - ANTHROPIC_API_KEY exported in the environment
  - a Python with the `modal` package (e.g. miniforge base)

Usage:
  python scripts/modal-e2e.py                 # v2 durability test (the real one)
  python scripts/modal-e2e.py --classic-volume  # also run the v1 negative control
"""

from __future__ import annotations

import argparse
import json
import os
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

import modal

REPO_ROOT = Path(__file__).resolve().parent.parent
PORT = 8080
SANDBOX_TIMEOUT = 15 * 60  # 15 minutes
MODEL = "anthropic/claude-haiku-4-5"  # cheap, fast real model for the live prompt
CPU = 1.0


# --- binary build ------------------------------------------------------


def build_binary() -> str:
    """Build a static linux/amd64 harness binary; return its path."""
    out = tempfile.mkdtemp(prefix="harness-modal-e2e-")
    bin_path = os.path.join(out, "harness")
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
    print(f"building linux/amd64 harness → {bin_path}")
    subprocess.run(
        ["go", "build", "-o", bin_path, "./cmd/harness"],
        cwd=str(REPO_ROOT),
        env=env,
        check=True,
    )
    return bin_path


# --- HTTP helpers (stdlib only) ----------------------------------------


class Client:
    def __init__(self, base_url: str, token: str):
        self.base = base_url.rstrip("/")
        self.token = token

    def _req(self, method: str, path: str, body: dict | None, auth: bool) -> tuple[int, bytes]:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method)
        if auth:
            req.add_header("Authorization", "Bearer " + self.token)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                return resp.status, resp.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def wait_healthy(self, deadline_s: float = 120.0) -> None:
        """Poll /health until 200 or deadline (bounded poll — real remote boot)."""
        deadline = time.monotonic() + deadline_s
        last = None
        while time.monotonic() < deadline:
            try:
                status, _ = self._req("GET", "/health", None, auth=False)
                if status == 200:
                    return
                last = status
            except (urllib.error.URLError, ConnectionError, TimeoutError) as e:
                last = repr(e)
            time.sleep(1.0)
        raise RuntimeError(f"server did not become healthy (last={last})")

    def create_session(self, model: str) -> str:
        status, body = self._req("POST", "/session", {"model": model}, auth=True)
        if status != 201:
            raise RuntimeError(f"create session: {status} {body!r}")
        return json.loads(body)["id"]

    def prompt(self, sid: str, text: str) -> None:
        body = {"parts": [{"type": "text", "text": text}]}
        status, resp = self._req("POST", f"/session/{sid}/prompt_async", body, auth=True)
        if status != 202:
            raise RuntimeError(f"prompt_async: {status} {resp!r}")

    def session(self, sid: str) -> dict:
        status, body = self._req("GET", f"/session/{sid}", None, auth=True)
        if status != 200:
            raise RuntimeError(f"get session: {status} {body!r}")
        return json.loads(body)

    def messages(self, sid: str) -> list:
        status, body = self._req("GET", f"/session/{sid}/message", None, auth=True)
        if status != 200:
            raise RuntimeError(f"get messages: {status} {body!r}")
        return json.loads(body)

    def wait_idle(self, sid: str, min_messages: int, deadline_s: float = 180.0) -> dict:
        """Poll the session until it is idle with >= min_messages messages, or a
        deadline. Real async prompt completion over the tunnel: bounded poll."""
        deadline = time.monotonic() + deadline_s
        last = None
        while time.monotonic() < deadline:
            s = self.session(sid)
            last = s
            if s.get("status") == "idle" and s.get("messages", 0) >= min_messages:
                return s
            time.sleep(2.0)
        raise RuntimeError(f"session not idle with >={min_messages} msgs (last={last})")


# --- sandbox lifecycle -------------------------------------------------


class Runner:
    def __init__(self, image, app, volume):
        self.image = image
        self.app = app
        self.volume = volume
        self.token = secrets.token_hex(16)
        self.sandboxes: list = []

    def launch(self) -> Client:
        """Launch a serve sandbox on the volume and return a healthy client."""
        api_key = os.environ["ANTHROPIC_API_KEY"]
        sb = modal.Sandbox.create(
            "sh",
            "-c",
            f"chmod +x /harness && exec /harness serve -addr 0.0.0.0:{PORT}",
            image=self.image,
            app=self.app,
            cpu=CPU,
            timeout=SANDBOX_TIMEOUT,
            encrypted_ports=[PORT],
            volumes={"/sessions": self.volume},
            env={
                "GOMAXPROCS": str(int(CPU)),
                "HARNESS_SESSION_DIR": "/sessions",
                "HARNESS_RUN_TOKEN": self.token,
                "ANTHROPIC_API_KEY": api_key,
            },
        )
        self.sandboxes.append(sb)
        tunnel = sb.tunnels()[PORT]
        url = tunnel.url
        print(f"  sandbox {sb.object_id} up; tunnel {url}")
        client = Client(url, self.token)
        client.wait_healthy()
        return client

    def terminate_all(self) -> None:
        for sb in self.sandboxes:
            try:
                sb.terminate()
            except Exception as e:  # noqa: BLE001 - best-effort cleanup
                print(f"  (cleanup) terminate {getattr(sb, 'object_id', '?')}: {e}")
        self.sandboxes.clear()


def run_flow(image, app, volume, label: str, strict: bool) -> bool:
    """Run the kill/relaunch durability flow on one volume.

    strict=True  → assertions fail the test (v2 durability path).
    strict=False → informational negative control (classic v1); never fails.
    Returns True on PASS (or, for the control, always returns True).
    """
    print(f"\n=== {label} ===")
    runner = Runner(image, app, volume)
    try:
        # Phase 1: create + prompt on the first sandbox.
        c1 = runner.launch()
        sid = c1.create_session(MODEL)
        print(f"  session {sid}")
        nonce = secrets.token_hex(3)
        c1.prompt(sid, f"Use the bash tool to run: echo e2e-{nonce}. Then confirm the output.")
        s1 = c1.wait_idle(sid, min_messages=2)
        n1 = len(c1.messages(sid))
        seq1 = int(s1.get("seq", 0))
        print(f"  after first prompt: messages={n1} max_seq={seq1}")

        # Phase 2: abrupt kill (no graceful shutdown).
        print("  terminating sandbox abruptly...")
        runner.sandboxes[-1].terminate()
        runner.sandboxes.pop()

        # Phase 3: relaunch on the SAME volume and check durability.
        c2 = runner.launch()
        ids = [s["id"] for s in json.loads(c2._req("GET", "/session", None, auth=True)[1])]
        listed = sid in ids
        boot = c2.session(sid)
        n2 = len(c2.messages(sid))
        seq_boot = int(boot.get("seq", 0))
        print(f"  after relaunch: listed={listed} messages={n2} boot_seq={seq_boot}")

        # Phase 4: still promptable; seq continues above the prior max. A prompt
        # may add several messages (user + assistant + any tool-call/result
        # turns), so require only that the count grows, not an exact delta.
        nonce2 = secrets.token_hex(3)
        c2.prompt(sid, f"Use the bash tool to run: echo again-{nonce2}. Then confirm.")
        s3 = c2.wait_idle(sid, min_messages=n2 + 1)
        n3 = len(c2.messages(sid))
        seq3 = int(s3.get("seq", 0))
        print(f"  after second prompt: messages={n3} max_seq={seq3}")

        # Evaluate.
        ok = True
        checks = [
            ("session listed after relaunch", listed),
            (f"message count retained ({n2} == {n1})", n2 == n1),
            (f"seq continued above prior max ({seq3} > {seq1})", seq3 > seq1),
            (f"session promptable ({n3} > {n2})", n3 > n2),
        ]
        for name, passed in checks:
            print(f"  [{'PASS' if passed else 'FAIL'}] {name}")
            ok = ok and passed

        if strict:
            return ok

        # Negative control: report only. Losing messages is the EXPECTED classic
        # behavior; retaining everything is fine too. Never fails the run.
        if n2 == n1:
            print(f"  (control) classic volume retained all {n1} messages (no loss observed)")
        else:
            print(f"  (control) classic volume lost {n1 - n2} of {n1} messages (expected)")
        return True
    finally:
        runner.terminate_all()


def make_image(bin_path: str):
    return modal.Image.from_registry("golang:1.25").add_local_file(
        bin_path, "/harness", copy=True
    )


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--classic-volume",
        action="store_true",
        help="also run the v1 (classic) Volume negative control (informational)",
    )
    args = ap.parse_args()

    if not os.environ.get("ANTHROPIC_API_KEY"):
        print("ANTHROPIC_API_KEY is not set", file=sys.stderr)
        return 2

    bin_path = build_binary()
    image = make_image(bin_path)
    app = modal.App.lookup("harness-e2e", create_if_missing=True)

    suffix = secrets.token_hex(4)
    v2_name = f"harness-e2e-v2-{suffix}"
    v2 = modal.Volume.from_name(v2_name, create_if_missing=True, version=2)

    created_volumes = [v2_name]
    passed = False
    try:
        passed = run_flow(image, app, v2, "v2 durability (version=2)", strict=True)

        if args.classic_volume:
            v1_name = f"harness-e2e-v1-{suffix}"
            v1 = modal.Volume.from_name(v1_name, create_if_missing=True, version=1)
            created_volumes.append(v1_name)
            run_flow(image, app, v1, "classic control (version=1)", strict=False)
    finally:
        for name in created_volumes:
            try:
                # Prefer the newer objects API; fall back to the classmethod.
                deleter = getattr(getattr(modal.Volume, "objects", None), "delete", None)
                if deleter is not None:
                    deleter(name)
                else:
                    modal.Volume.delete(name)
            except Exception as e:  # noqa: BLE001 - best-effort cleanup
                print(f"(cleanup) volume delete {name}: {e}")

    print("\n" + ("PASS: v2 volume retained all messages and seq continuity"
                  if passed else "FAIL: v2 durability assertions did not hold"))
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
