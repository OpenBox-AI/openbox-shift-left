#!/usr/bin/env python3
"""Qualify the installed Codex macOS sandbox without a model invocation."""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import os
import platform
import shutil
import signal
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


PROFILE = "project-assurance-qualification"
UNLISTED_URL = "http://not-allowed.invalid/"
BACKEND_DENIAL_PROFILE = (
    '(version 1) (allow default) '
    '(deny process-exec (literal "/usr/bin/sandbox-exec"))'
)
PROFILE_ARGS = (
    "--enable",
    "network_proxy",
    "-c",
    f'permissions.{PROFILE}.extends=":workspace"',
    "-c",
    f"permissions.{PROFILE}.network.enabled=true",
    "-c",
    f'permissions.{PROFILE}.network.mode="full"',
    "-c",
    f"permissions.{PROFILE}.network.allow_local_binding=true",
    "-c",
    (
        f'permissions.{PROFILE}.network.domains='
        '{"127.0.0.1"="allow","::1"="allow","localhost"="allow"}'
    ),
    "-c",
    'shell_environment_policy.inherit="all"',
)


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


class Receiver(BaseHTTPRequestHandler):
    requests_seen = 0

    def do_GET(self) -> None:  # noqa: N802 - stdlib handler API
        type(self).requests_seen += 1
        body = b"qualification-ok"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args: Any) -> None:
        return


def command_text(command: list[str], replacements: dict[str, str]) -> str:
    rendered: list[str] = []
    for argument in command:
        value = argument
        for raw, replacement in replacements.items():
            value = value.replace(raw, replacement)
        rendered.append(value)
    return " ".join(json.dumps(value) for value in rendered)


def normalize_strings(value: Any, replacements: dict[str, str]) -> Any:
    if isinstance(value, dict):
        return {key: normalize_strings(item, replacements) for key, item in value.items()}
    if isinstance(value, list):
        return [normalize_strings(item, replacements) for item in value]
    if isinstance(value, str):
        for raw, replacement in replacements.items():
            value = value.replace(raw, replacement)
    return value


def relevant_denials(stderr: str, protected_root: Path) -> list[str]:
    keep: list[str] = []
    for line in stderr.splitlines():
        if str(protected_root) in line or "network-outbound" in line:
            keep.append(line)
    return sorted(set(keep))


def process_exists(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def wait_gone(pid: int, timeout: float) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not process_exists(pid):
            return True
        time.sleep(0.05)
    return not process_exists(pid)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--codex", default="/opt/homebrew/bin/codex")
    parser.add_argument(
        "--payload",
        default=str(Path(__file__).with_name("codex_sandbox_payload.py")),
    )
    parser.add_argument(
        "--protected-root",
        default=str(Path(__file__).resolve().parents[1] / ".codex-sandbox-protected-SE-00-05"),
    )
    args = parser.parse_args()

    codex = Path(args.codex).resolve()
    payload = Path(args.payload).resolve()
    protected_root = Path(args.protected_root).resolve()
    if protected_root.exists():
        raise RuntimeError(f"refusing pre-existing protected probe path: {protected_root}")

    run_root = Path(tempfile.mkdtemp(prefix="openbox-se00-05-codex."))
    workspace = run_root / "workspace"
    codex_home = run_root / "codex-home"
    sandbox_tmp = workspace / "tmp"
    workspace.mkdir()
    codex_home.mkdir()
    sandbox_tmp.mkdir()
    protected_root.mkdir()
    read_marker = protected_root / "read-marker.txt"
    read_marker.write_bytes(b"outside-read-marker")
    protected_target = protected_root / "parent-denied.txt"
    fallback_sentinel = workspace / "invalid-state-fallback-sentinel"
    backend_sentinel = workspace / "backend-unavailable-fallback-sentinel"
    timeout_pid_file = workspace / "timeout.pid"

    safe_env = {
        "CODEX_HOME": str(codex_home),
        "LANG": "C.UTF-8",
        "OPENBOX_QUALIFICATION_MARKER": "SE-00-05",
        "PATH": "/usr/bin:/bin:/opt/homebrew/bin",
        "PYTHONDONTWRITEBYTECODE": "1",
        "TMPDIR": f"{sandbox_tmp}/",
    }
    replacements = {
        str(run_root.resolve()): "$RUN_ROOT",
        str(run_root): "$RUN_ROOT",
        str(protected_root): "$PROTECTED_ROOT",
        str(payload): "$PAYLOAD",
    }

    Receiver.requests_seen = 0
    server = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
    server_thread = threading.Thread(target=server.serve_forever, daemon=True)
    server_thread.start()
    loopback_url = f"http://127.0.0.1:{server.server_port}/"
    replacements[loopback_url] = "$LOOPBACK_URL"

    base = [
        str(codex),
        "sandbox",
        "--permission-profile",
        PROFILE,
        "--cd",
        str(workspace),
        *PROFILE_ARGS,
    ]
    main_command = [
        *base,
        "--log-denials",
        "--",
        "/usr/bin/python3",
        str(payload),
        "--mode",
        "parent",
        "--workspace",
        str(workspace),
        "--protected-target",
        str(protected_target),
        "--read-marker",
        str(read_marker),
        "--loopback-url",
        loopback_url,
        "--unlisted-url",
        UNLISTED_URL,
    ]

    result: dict[str, Any] | None = None
    cleanup: dict[str, Any] = {}
    try:
        version_run = subprocess.run(
            [str(codex), "--version"],
            check=True,
            capture_output=True,
            text=True,
            timeout=5.0,
            env=safe_env,
        )
        main_run = subprocess.run(
            main_command,
            check=False,
            capture_output=True,
            timeout=15.0,
            env=safe_env,
        )
        if main_run.returncode != 0:
            raise AssertionError(
                f"main sandbox probe failed: exit={main_run.returncode} "
                f"stderr={main_run.stderr[:4000]!r}"
            )
        observation = normalize_strings(json.loads(main_run.stdout), replacements)
        if Receiver.requests_seen != 2:
            raise AssertionError(f"expected two loopback requests, got {Receiver.requests_seen}")
        if protected_target.exists() or Path(f"{protected_target}.child").exists():
            raise AssertionError("sandbox wrote a protected outside target")
        denials = relevant_denials(main_run.stderr.decode("utf-8", "replace"), protected_root)
        if not any("file-write" in line for line in denials):
            raise AssertionError(f"protected write denial was not logged: {denials!r}")
        if not any("network-outbound" in line for line in denials):
            raise AssertionError(f"direct network denial was not logged: {denials!r}")

        invalid_state_command = [
            *base,
            "--sandbox-state-json",
            "{",
            "--",
            "/usr/bin/touch",
            str(fallback_sentinel),
        ]
        invalid_state_run = subprocess.run(
            invalid_state_command,
            check=False,
            capture_output=True,
            timeout=5.0,
            env=safe_env,
        )
        if invalid_state_run.returncode == 0 or fallback_sentinel.exists():
            raise AssertionError("invalid sandbox state did not fail before payload execution")

        backend_unavailable_command = [
            "/usr/bin/sandbox-exec",
            "-p",
            BACKEND_DENIAL_PROFILE,
            *base,
            "--",
            "/usr/bin/touch",
            str(backend_sentinel),
        ]
        backend_unavailable_run = subprocess.run(
            backend_unavailable_command,
            check=False,
            capture_output=True,
            timeout=5.0,
            env=safe_env,
        )
        if backend_unavailable_run.returncode == 0 or backend_sentinel.exists():
            raise AssertionError("unavailable sandbox backend retried the payload unsandboxed")
        if b"Operation not permitted" not in backend_unavailable_run.stderr:
            raise AssertionError(
                "backend-unavailable probe did not expose the expected exec denial: "
                f"{backend_unavailable_run.stderr[:1000]!r}"
            )

        timeout_command = [
            *base,
            "--",
            "/bin/sh",
            "-c",
            f"printf '%s' $$ > {timeout_pid_file}; exec /bin/sleep 60",
        ]
        timeout_process = subprocess.Popen(
            timeout_command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=safe_env,
            start_new_session=True,
        )
        timeout_triggered = False
        try:
            timeout_stdout, timeout_stderr = timeout_process.communicate(timeout=1.0)
        except subprocess.TimeoutExpired:
            timeout_triggered = True
            os.killpg(timeout_process.pid, signal.SIGTERM)
            try:
                timeout_stdout, timeout_stderr = timeout_process.communicate(timeout=2.0)
            except subprocess.TimeoutExpired:
                os.killpg(timeout_process.pid, signal.SIGKILL)
                timeout_stdout, timeout_stderr = timeout_process.communicate(timeout=2.0)
        if not timeout_triggered or not timeout_pid_file.exists():
            raise AssertionError("bounded parent timeout did not exercise the sandboxed payload")
        sandboxed_pid = int(timeout_pid_file.read_text(encoding="utf-8"))
        if not wait_gone(sandboxed_pid, 2.0):
            raise AssertionError(f"sandboxed timeout process remained alive: {sandboxed_pid}")

        output_command = [
            *base,
            "--",
            "/usr/bin/python3",
            "-c",
            "import sys; sys.stdout.write('x' * 70000)",
        ]
        output_run = subprocess.run(
            output_command,
            check=False,
            capture_output=True,
            timeout=8.0,
            env=safe_env,
        )
        if output_run.returncode != 0 or len(output_run.stdout) != 70000:
            raise AssertionError("standalone sandbox output-forwarding probe was not exact")

        result = {
            "schema": "openbox.project-assurance.codex-sandbox-probe/v1",
            "candidate": {
                "version": version_run.stdout.strip(),
                "binary": str(codex),
                "binary_sha256": digest(codex.read_bytes()),
                "source_tag": "rust-v0.149.0",
                "source_tag_object": "a4e15bf371341b067c8278d3b70b1a8c7b3d793e",
                "source_commit": "758ef40f50c1a458425c7cfbf1eb12cbc07af0b0",
                "platform": {
                    "macos": platform.mac_ver()[0],
                    "machine": platform.machine(),
                    "darwin": platform.release(),
                },
            },
            "configuration": {
                "command": command_text(main_command, replacements),
                "permission_profile": PROFILE,
                "extends": ":workspace",
                "network_proxy_feature": True,
                "network_mode": "full",
                "allow_local_binding": True,
                "allowed_domains": ["127.0.0.1", "::1", "localhost"],
                "environment_keys": sorted(safe_env),
                "cwd": "$RUN_ROOT/workspace",
            },
            "parent_child": observation,
            "receiver": {
                "bind": "127.0.0.1",
                "requests_seen": Receiver.requests_seen,
                "response_body_sha256": digest(b"qualification-ok"),
            },
            "denial_reporting": {
                "flag": "--log-denials",
                "relevant_denials": [
                    line.replace(str(protected_root), "$PROTECTED_ROOT").replace(
                        f":{server.server_port}", ":$LOOPBACK_PORT"
                    )
                    for line in denials
                ],
            },
            "invalid_state_fail_closed": {
                "command": command_text(invalid_state_command, replacements),
                "exit_status": invalid_state_run.returncode,
                "stderr_sha256": digest(invalid_state_run.stderr),
                "payload_executed": fallback_sentinel.exists(),
            },
            "backend_unavailable_fail_closed": {
                "command": command_text(backend_unavailable_command, replacements),
                "fault_injection": "outer Seatbelt denies process-exec of /usr/bin/sandbox-exec",
                "exit_status": backend_unavailable_run.returncode,
                "stderr_sha256": digest(backend_unavailable_run.stderr),
                "payload_executed": backend_sentinel.exists(),
                "unsandboxed_retry_observed": backend_sentinel.exists(),
            },
            "timeout": {
                "command": command_text(timeout_command, replacements),
                "owner": "caller process group; codex sandbox exposes no timeout flag",
                "limit_seconds": 1.0,
                "triggered": timeout_triggered,
                "codex_exit_status": timeout_process.returncode,
                "sandboxed_process_gone": not process_exists(sandboxed_pid),
                "stdout_bytes": len(timeout_stdout),
                "stderr_bytes": len(timeout_stderr),
            },
            "output": {
                "command": command_text(output_command, replacements),
                "requested_bytes": 70000,
                "forwarded_stdout_bytes": len(output_run.stdout),
                "stdout_sha256": digest(output_run.stdout),
                "native_cap_available": False,
                "limit": "codex sandbox exposes no output-cap flag; the production driver must cap both streams while reading",
            },
            "limits": [
                "This invokes the standalone sandbox only; it makes no model or OpenAI API request.",
                "HTTP loopback succeeds through the managed proxy; the amended profile also permits the project's direct loopback listener and socket while direct external sockets remain denied.",
                "The built-in workspace profile permits broad reads and temporary-directory writes; this probe proves outside-workspace write denial only for the declared protected target.",
                "Seatbelt backend unavailability is a controlled fault injection: an outer Seatbelt profile denies execution of the inner /usr/bin/sandbox-exec. It proves CLI failure without an unsandboxed payload retry, not a naturally degraded host.",
                "The standalone command has no native timeout or output cap. The driver must own process-group timeout, streaming byte caps, and cleanup before this candidate can become a support tuple.",
            ],
            "judgment": "qualified_candidate_for_driver_implementation",
        }
    finally:
        server.shutdown()
        server.server_close()
        server_thread.join(timeout=2.0)
        with contextlib.suppress(FileNotFoundError):
            shutil.rmtree(run_root)
        with contextlib.suppress(FileNotFoundError):
            shutil.rmtree(protected_root)
        cleanup = {
            "server_thread_alive": server_thread.is_alive(),
            "run_root_exists": run_root.exists(),
            "protected_root_exists": protected_root.exists(),
        }

    if result is None:
        raise AssertionError("probe produced no result")
    if cleanup != {
        "server_thread_alive": False,
        "run_root_exists": False,
        "protected_root_exists": False,
    }:
        raise AssertionError(f"probe cleanup failed: {cleanup!r}")
    result["cleanup"] = cleanup
    print(json.dumps(result, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
