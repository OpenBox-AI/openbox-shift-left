#!/usr/bin/env python3
"""Qualify Anthropic Sandbox Runtime 0.0.73 without invoking a model."""

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


UNLISTED_URL = "http://not-allowed.invalid/"
BACKEND_DENIAL_PROFILE = (
    '(version 1) (allow default) '
    '(deny process-exec (literal "/usr/bin/sandbox-exec"))'
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


def normalize_strings(value: Any, replacements: dict[str, str]) -> Any:
    if isinstance(value, dict):
        return {key: normalize_strings(item, replacements) for key, item in value.items()}
    if isinstance(value, list):
        return [normalize_strings(item, replacements) for item in value]
    if isinstance(value, str):
        for raw, replacement in replacements.items():
            value = value.replace(raw, replacement)
    return value


def command_text(command: list[str], replacements: dict[str, str]) -> str:
    return " ".join(json.dumps(normalize_strings(argument, replacements)) for argument in command)


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
    parser.add_argument("--node", required=True)
    parser.add_argument("--srt-cli", required=True)
    parser.add_argument("--payload", required=True)
    parser.add_argument("--package-tarball", required=True)
    parser.add_argument(
        "--protected-root",
        default=str(Path(__file__).resolve().parents[1] / ".claude-srt-protected-SE-00-06"),
    )
    args = parser.parse_args()

    node = Path(args.node).resolve()
    srt_cli = Path(args.srt_cli).resolve()
    payload_source = Path(args.payload).resolve()
    package_tarball = Path(args.package_tarball).resolve()
    protected_root = Path(args.protected_root).resolve()
    if protected_root.exists():
        raise RuntimeError(f"refusing pre-existing protected probe path: {protected_root}")

    package_root = srt_cli.parents[1]
    package_json = json.loads((package_root / "package.json").read_text(encoding="utf-8"))
    if package_json.get("name") != "@anthropic-ai/sandbox-runtime":
        raise RuntimeError("unexpected SRT package")
    if package_json.get("version") != "0.0.73":
        raise RuntimeError(f"unexpected SRT version: {package_json.get('version')!r}")

    run_root = Path(tempfile.mkdtemp(prefix="openbox-se00-06-srt."))
    workspace = run_root / "workspace"
    sandbox_tmp = workspace / "tmp"
    workspace.mkdir()
    sandbox_tmp.mkdir()
    payload = workspace / "claude_sandbox_payload.py"
    shutil.copyfile(payload_source, payload)
    output_payload = workspace / "output_payload.py"
    output_payload.write_text(
        "import sys\nsys.stdout.write('x' * 70000)\n", encoding="utf-8"
    )
    protected_root.mkdir()
    read_marker = protected_root / "read-marker.txt"
    read_marker.write_bytes(b"outside-read-marker")
    protected_target = protected_root / "parent-denied.txt"
    backend_sentinel = workspace / "backend-unavailable-fallback-sentinel"
    invalid_settings_sentinel = workspace / "invalid-settings-fallback-sentinel"
    timeout_pid_file = workspace / "timeout.pid"
    settings = run_root / "srt-settings.json"
    settings.write_text(
        json.dumps(
            {
                "filesystem": {
                    "denyRead": [str(Path.home())],
                    "allowRead": [str(workspace)],
                    "allowWrite": [str(workspace)],
                    "denyWrite": [str(protected_root)],
                },
                "network": {
                    "allowedDomains": ["127.0.0.1", "localhost"],
                    "deniedDomains": [],
                    "allowLocalBinding": True,
                },
                "credentials": {
                    "envVars": [
                        {"name": name, "mode": "deny"}
                        for name in (
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
                    ]
                },
            },
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )

    safe_env = {
        "LANG": "C.UTF-8",
        "OPENBOX_PROBE_SECRET": "synthetic-qualification-secret",
        "OPENBOX_QUALIFICATION_MARKER": "SE-00-06",
        "PATH": "/usr/bin:/bin",
        "PYTHONDONTWRITEBYTECODE": "1",
        "SRT_DEBUG": "true",
        "TMPDIR": f"{sandbox_tmp}/",
    }
    replacements = {
        str(run_root.resolve()): "$RUN_ROOT",
        str(run_root): "$RUN_ROOT",
        str(protected_root): "$PROTECTED_ROOT",
        str(payload_source): "$PAYLOAD_SOURCE",
        str(payload): "$PAYLOAD",
        str(node): "$NODE",
        str(srt_cli): "$SRT_CLI",
        str(package_tarball): "$SRT_TARBALL",
    }

    Receiver.requests_seen = 0
    server = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
    server_thread = threading.Thread(target=server.serve_forever, daemon=True)
    server_thread.start()
    loopback_url = f"http://127.0.0.1:{server.server_port}/"
    replacements[loopback_url] = "$LOOPBACK_URL"

    base = [str(node), str(srt_cli), "--settings", str(settings)]
    main_command = [
        str(node),
        str(srt_cli),
        "--debug",
        "--settings",
        str(settings),
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
        "--expect-direct-loopback",
        "connected",
    ]

    result: dict[str, Any] | None = None
    cleanup: dict[str, Any] = {}
    try:
        version_run = subprocess.run(
            [str(node), str(srt_cli), "--version"],
            check=False,
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
            cwd=workspace,
            env=safe_env,
        )
        if main_run.returncode != 0:
            raise AssertionError(
                f"standalone SRT probe failed: exit={main_run.returncode} "
                f"stderr={main_run.stderr[:4000]!r}"
            )
        observation = normalize_strings(json.loads(main_run.stdout), replacements)
        if Receiver.requests_seen != 2:
            raise AssertionError(f"expected two loopback requests, got {Receiver.requests_seen}")
        if protected_target.exists() or Path(f"{protected_target}.child").exists():
            raise AssertionError("SRT wrote a protected outside target")

        invalid_settings_command = [
            str(node),
            str(srt_cli),
            "--settings",
            str(run_root / "missing-settings.json"),
            "/usr/bin/touch",
            str(invalid_settings_sentinel),
        ]
        invalid_settings_run = subprocess.run(
            invalid_settings_command,
            check=False,
            capture_output=True,
            timeout=5.0,
            cwd=workspace,
            env=safe_env,
        )
        if invalid_settings_run.returncode == 0 or invalid_settings_sentinel.exists():
            raise AssertionError("explicit missing settings fell back to a default payload run")

        backend_unavailable_command = [
            "/usr/bin/sandbox-exec",
            "-p",
            BACKEND_DENIAL_PROFILE,
            *base,
            "/usr/bin/touch",
            str(backend_sentinel),
        ]
        backend_unavailable_run = subprocess.run(
            backend_unavailable_command,
            check=False,
            capture_output=True,
            timeout=5.0,
            cwd=workspace,
            env=safe_env,
        )
        if backend_unavailable_run.returncode == 0 or backend_sentinel.exists():
            raise AssertionError("unavailable SRT backend retried the payload unsandboxed")

        timeout_command = [
            *base,
            "/bin/sh",
            "-c",
            f"printf '%s' $$ > {timeout_pid_file}; exec /bin/sleep 60",
        ]
        timeout_process = subprocess.Popen(
            timeout_command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            cwd=workspace,
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
            raise AssertionError("bounded timeout did not exercise the SRT payload")
        sandboxed_pid = int(timeout_pid_file.read_text(encoding="utf-8"))
        if not wait_gone(sandboxed_pid, 2.0):
            raise AssertionError(f"sandboxed timeout process remained alive: {sandboxed_pid}")

        output_command = [
            *base,
            "/usr/bin/python3",
            str(output_payload),
        ]
        output_run = subprocess.run(
            output_command,
            check=False,
            capture_output=True,
            timeout=8.0,
            cwd=workspace,
            env=safe_env,
        )
        if output_run.returncode != 0 or len(output_run.stdout) != 70000:
            raise AssertionError(
                "SRT output-forwarding probe was not exact: "
                f"exit={output_run.returncode} stdout={len(output_run.stdout)} "
                f"stderr={output_run.stderr[:2000]!r}"
            )

        result = {
            "schema": "openbox.project-assurance.claude-srt-probe/v1",
            "candidate": {
                "package": "@anthropic-ai/sandbox-runtime",
                "package_version": package_json["version"],
                "package_tarball_sha256": digest(package_tarball.read_bytes()),
                "cli_reported_version": version_run.stdout.strip(),
                "cli_version_limit": "Direct node execution has no npm_package_version and the CLI falls back to the hard-coded string 1.0.0; package.json is authoritative.",
                "node_version": subprocess.run(
                    [str(node), "--version"],
                    check=True,
                    capture_output=True,
                    text=True,
                    timeout=5.0,
                    env=safe_env,
                ).stdout.strip(),
                "platform": {
                    "macos": platform.mac_ver()[0],
                    "machine": platform.machine(),
                    "darwin": platform.release(),
                },
            },
            "configuration": {
                "command": command_text(main_command, replacements),
                "settings_sha256": digest(settings.read_bytes()),
                "cwd": "$RUN_ROOT/workspace",
                "allowed_domains": ["127.0.0.1", "localhost"],
                "allow_local_binding": True,
                "deny_read": [str(Path.home())],
                "allow_read": ["$RUN_ROOT/workspace"],
                "allow_write": ["$RUN_ROOT/workspace"],
                "protected_credential_names": 10,
                "environment_keys": sorted(safe_env),
            },
            "parent_child": observation,
            "receiver": {
                "bind": "127.0.0.1",
                "requests_seen": Receiver.requests_seen,
                "response_body_sha256": digest(b"qualification-ok"),
            },
            "denial_reporting": {
                "outside_read_errno": observation["parent"]["outside_read"]["errno"],
                "outside_write_errno": observation["parent"]["outside_write"]["errno"],
                "direct_loopback_connected": observation["parent"]["direct_loopback_socket"]["connected"],
                "debug_stderr_sha256": digest(main_run.stderr),
                "debug_stderr_bytes": len(main_run.stderr),
            },
            "explicit_settings_fail_closed": {
                "command": command_text(invalid_settings_command, replacements),
                "exit_status": invalid_settings_run.returncode,
                "stderr_sha256": digest(invalid_settings_run.stderr),
                "payload_executed": invalid_settings_sentinel.exists(),
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
                "owner": "caller process group; srt exposes no timeout flag",
                "limit_seconds": 1.0,
                "triggered": timeout_triggered,
                "srt_exit_status": timeout_process.returncode,
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
                "limit": "srt exposes no output-cap flag; the production driver must cap both streams while reading",
            },
            "limits": [
                "This standalone SRT probe invokes no Claude model or Anthropic API.",
                "SRT always puts loopback in NO_PROXY on POSIX, so required loopback access needs allowLocalBinding=true and permits direct access to all local ports; Phase 03 must treat that as an exact support-tuple limitation.",
                "Package.json 0.0.73 is authoritative because direct CLI --version reports its hard-coded 1.0.0 fallback.",
                "The standalone CLI has no native timeout or output cap; a caller must own process-group timeout, streaming caps, and cleanup.",
            ],
            "judgment": "qualified_standalone_candidate",
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
