#!/usr/bin/env python3
"""Payload shared by standalone SRT and inherited Claude Bash sandbox probes."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import socket
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


CREDENTIAL_NAMES = (
    "ANTHROPIC_API_KEY",
    "AWS_ACCESS_KEY_ID",
    "AWS_SECRET_ACCESS_KEY",
    "CLAUDE_CODE_OAUTH_TOKEN",
    "CODEX_API_KEY",
    "GOOGLE_APPLICATION_CREDENTIALS",
    "OPENAI_API_KEY",
    "OPENBOX_AGENT_PRIVATE_KEY",
    "OPENBOX_API_KEY",
    "OPENBOX_PROBE_SECRET",
)


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def environment_observation() -> dict[str, Any]:
    proxies: dict[str, Any] = {}
    for name in ("ALL_PROXY", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"):
        value = os.environ.get(name)
        if value is None:
            proxies[name] = None
            continue
        parsed = urllib.parse.urlparse(value)
        proxies[name] = {
            "present": True,
            "scheme": parsed.scheme,
            "loopback_host": parsed.hostname in {"127.0.0.1", "::1", "localhost"},
            "empty": value == "",
        }
    return {
        "qualification_marker": os.environ.get("OPENBOX_QUALIFICATION_MARKER"),
        "credential_names_present": [name for name in CREDENTIAL_NAMES if name in os.environ],
        "proxies": proxies,
    }


def write_observation(path: Path, value: str) -> dict[str, Any]:
    try:
        path.write_text(value, encoding="utf-8")
    except OSError as exc:
        return {"written": False, "error_type": type(exc).__name__, "errno": exc.errno}
    return {"written": True, "sha256": digest(path.read_bytes())}


def read_observation(path: Path) -> dict[str, Any]:
    try:
        data = path.read_bytes()
    except OSError as exc:
        return {"read": False, "error_type": type(exc).__name__, "errno": exc.errno}
    return {"read": True, "sha256": digest(data)}


def http_observation(url: str) -> dict[str, Any]:
    try:
        with urllib.request.urlopen(url, timeout=3.0) as response:
            data = response.read(4096)
            return {
                "reached": True,
                "status": response.status,
                "body_sha256": digest(data),
            }
    except urllib.error.HTTPError as exc:
        exc.read(4096)
        return {
            "reached": False,
            "error_type": type(exc).__name__,
            "status": exc.code,
        }
    except (urllib.error.URLError, OSError) as exc:
        reason = getattr(exc, "reason", exc)
        return {
            "reached": False,
            "error_type": type(exc).__name__,
            "reason_type": type(reason).__name__,
            "errno": getattr(reason, "errno", None),
        }


def direct_socket_observation(loopback_url: str) -> dict[str, Any]:
    parsed = urllib.parse.urlparse(loopback_url)
    try:
        with socket.create_connection((parsed.hostname or "127.0.0.1", parsed.port or 80), 1.0):
            return {"connected": True}
    except OSError as exc:
        return {"connected": False, "error_type": type(exc).__name__, "errno": exc.errno}


def one_process_observation(args: argparse.Namespace, mode: str) -> dict[str, Any]:
    workspace = Path(args.workspace)
    return {
        "mode": mode,
        "cwd": os.getcwd(),
        "environment": environment_observation(),
        "workspace_write": write_observation(workspace / f"{mode}-allowed.txt", mode),
        "outside_write": write_observation(Path(args.protected_target), mode),
        "outside_read": read_observation(Path(args.read_marker)),
        "loopback_http": http_observation(args.loopback_url),
        "unlisted_http": http_observation(args.unlisted_url),
        "direct_loopback_socket": direct_socket_observation(args.loopback_url),
    }


def assert_process(
    observation: dict[str, Any], workspace: str, expect_direct_loopback: str
) -> None:
    assert Path(observation["cwd"]).resolve() == Path(workspace).resolve()
    assert observation["environment"]["qualification_marker"] == "SE-00-06"
    assert observation["environment"]["credential_names_present"] == []
    for name in ("ALL_PROXY", "HTTPS_PROXY", "HTTP_PROXY"):
        assert observation["environment"]["proxies"][name]["loopback_host"] is True
    assert observation["workspace_write"]["written"] is True
    assert observation["outside_write"]["written"] is False
    assert observation["outside_write"]["errno"] == 1
    assert observation["outside_read"]["read"] is False
    assert observation["outside_read"]["errno"] == 1
    expected_loopback = {
        "reached": True,
        "status": 200,
        "body_sha256": digest(b"qualification-ok"),
    }
    assert observation["loopback_http"] == expected_loopback, observation["loopback_http"]
    assert observation["unlisted_http"]["reached"] is False
    assert observation["unlisted_http"]["status"] == 403
    if expect_direct_loopback == "connected":
        assert observation["direct_loopback_socket"] == {"connected": True}
    else:
        assert observation["direct_loopback_socket"]["connected"] is False
        assert observation["direct_loopback_socket"]["errno"] == 1


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("parent", "child"), required=True)
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--protected-target", required=True)
    parser.add_argument("--read-marker", required=True)
    parser.add_argument("--loopback-url", required=True)
    parser.add_argument("--unlisted-url", required=True)
    parser.add_argument(
        "--expect-direct-loopback", choices=("connected", "denied"), required=True
    )
    args = parser.parse_args()

    current = one_process_observation(args, args.mode)
    if args.mode == "child":
        assert_process(current, args.workspace, args.expect_direct_loopback)
        print(json.dumps(current, sort_keys=True))
        return

    child_command = [
        sys.executable,
        str(Path(__file__).resolve()),
        "--mode",
        "child",
        "--workspace",
        args.workspace,
        "--protected-target",
        f"{args.protected_target}.child",
        "--read-marker",
        args.read_marker,
        "--loopback-url",
        args.loopback_url,
        "--unlisted-url",
        args.unlisted_url,
        "--expect-direct-loopback",
        args.expect_direct_loopback,
    ]
    child = subprocess.run(child_command, check=False, capture_output=True, text=True, timeout=8.0)
    if child.returncode != 0:
        raise AssertionError(
            f"sandbox child failed: exit={child.returncode} stderr={child.stderr[:1000]!r}"
        )
    child_observation = json.loads(child.stdout)
    assert_process(current, args.workspace, args.expect_direct_loopback)
    assert_process(child_observation, args.workspace, args.expect_direct_loopback)
    print(json.dumps({"parent": current, "child": child_observation}, sort_keys=True))


if __name__ == "__main__":
    main()
