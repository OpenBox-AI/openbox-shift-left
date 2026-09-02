#!/usr/bin/env python3
"""Qualify Claude Code's inherited Bash sandbox against a loopback model stub."""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import os
import platform
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


MODEL = "claude-sonnet-4-5-20250929"
BACKEND_DENIAL_PROFILE = (
    '(version 1) (allow default) '
    '(deny process-exec (literal "/usr/bin/sandbox-exec"))'
)
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
    return " ".join(json.dumps(normalize_strings(item, replacements)) for item in command)


def tool_result_from_request(body: dict[str, Any]) -> dict[str, Any] | None:
    for message in reversed(body.get("messages", [])):
        content = message.get("content", [])
        if not isinstance(content, list):
            continue
        for block in reversed(content):
            if isinstance(block, dict) and block.get("type") == "tool_result":
                return block
    return None


def sse_message(events: list[tuple[str, dict[str, Any]]]) -> bytes:
    chunks = []
    for event, data in events:
        chunks.append(f"event: {event}\ndata: {json.dumps(data, separators=(',', ':'))}\n\n")
    return "".join(chunks).encode("utf-8")


def tool_use_response(tool_input: dict[str, Any], scenario: str) -> bytes:
    tool_id = f"toolu_openbox_{scenario}"
    events = [
        (
            "message_start",
            {
                "type": "message_start",
                "message": {
                    "id": f"msg_openbox_{scenario}_1",
                    "type": "message",
                    "role": "assistant",
                    "model": MODEL,
                    "content": [],
                    "stop_reason": None,
                    "stop_sequence": None,
                    "usage": {"input_tokens": 1, "output_tokens": 1},
                },
            },
        ),
        (
            "content_block_start",
            {
                "type": "content_block_start",
                "index": 0,
                "content_block": {
                    "type": "tool_use",
                    "id": tool_id,
                    "name": "Bash",
                    "input": {},
                },
            },
        ),
        (
            "content_block_delta",
            {
                "type": "content_block_delta",
                "index": 0,
                "delta": {
                    "type": "input_json_delta",
                    "partial_json": json.dumps(tool_input, separators=(",", ":")),
                },
            },
        ),
        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        (
            "message_delta",
            {
                "type": "message_delta",
                "delta": {"stop_reason": "tool_use", "stop_sequence": None},
                "usage": {"output_tokens": 20},
            },
        ),
        ("message_stop", {"type": "message_stop"}),
    ]
    return sse_message(events)


def final_response(scenario: str) -> bytes:
    events = [
        (
            "message_start",
            {
                "type": "message_start",
                "message": {
                    "id": f"msg_openbox_{scenario}_2",
                    "type": "message",
                    "role": "assistant",
                    "model": MODEL,
                    "content": [],
                    "stop_reason": None,
                    "stop_sequence": None,
                    "usage": {"input_tokens": 1, "output_tokens": 1},
                },
            },
        ),
        (
            "content_block_start",
            {
                "type": "content_block_start",
                "index": 0,
                "content_block": {"type": "text", "text": ""},
            },
        ),
        (
            "content_block_delta",
            {
                "type": "content_block_delta",
                "index": 0,
                "delta": {"type": "text_delta", "text": "qualification complete"},
            },
        ),
        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        (
            "message_delta",
            {
                "type": "message_delta",
                "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                "usage": {"output_tokens": 2},
            },
        ),
        ("message_stop", {"type": "message_stop"}),
    ]
    return sse_message(events)


class LoopbackMessages(BaseHTTPRequestHandler):
    scenario = ""
    tool_input: dict[str, Any] = {}
    requests: list[dict[str, Any]] = []
    unexpected_requests: list[dict[str, str]] = []
    payload_requests = 0

    def do_POST(self) -> None:  # noqa: N802 - stdlib handler API
        if self.path != "/v1/messages?beta=true":
            type(self).unexpected_requests.append({"method": "POST", "path": self.path})
            self.send_error(403, "loopback qualification endpoint only")
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        tool_result = tool_result_from_request(body)
        type(self).requests.append(
            {
                "path": self.path,
                "model": body.get("model"),
                "stream": body.get("stream"),
                "tool_names": [tool.get("name") for tool in body.get("tools", [])],
                "tool_result": tool_result,
                "anthropic_version": self.headers.get("anthropic-version"),
                "user_agent": self.headers.get("user-agent"),
                "api_key_present": bool(self.headers.get("x-api-key")),
            }
        )
        if len(type(self).requests) == 1:
            response = tool_use_response(type(self).tool_input, type(self).scenario)
        elif len(type(self).requests) == 2:
            response = final_response(type(self).scenario)
        else:
            self.send_error(500, "unexpected extra request")
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)

    def do_CONNECT(self) -> None:  # noqa: N802 - stdlib handler API
        type(self).unexpected_requests.append({"method": "CONNECT", "path": self.path})
        self.send_error(403, "external proxy target denied")

    def do_GET(self) -> None:  # noqa: N802 - stdlib handler API
        if self.path == "/qualification":
            type(self).payload_requests += 1
            body = b"qualification-ok"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        type(self).unexpected_requests.append({"method": "GET", "path": self.path})
        self.send_error(403, "unexpected request denied")

    def log_message(self, format: str, *args: Any) -> None:
        return


def parse_stream(stdout: bytes) -> list[dict[str, Any]]:
    events = []
    for line in stdout.decode("utf-8").splitlines():
        if line.strip():
            events.append(json.loads(line))
    return events


def result_text(tool_result: dict[str, Any]) -> str:
    content = tool_result.get("content", "")
    if isinstance(content, str):
        return content
    return json.dumps(content, sort_keys=True)


def run_scenario(
    *,
    scenario: str,
    run_root: Path,
    claude: Path,
    payload_source: Path,
) -> dict[str, Any]:
    scenario_root = run_root / scenario
    workspace = scenario_root / "workspace"
    config_dir = scenario_root / "config"
    sandbox_tmp = workspace / "tmp"
    protected_root = (
        Path(__file__).resolve().parents[1]
        / f".claude-inherited-protected-SE-00-06-{scenario}"
    )
    if protected_root.exists():
        raise RuntimeError(f"refusing pre-existing protected path: {protected_root}")
    workspace.mkdir(parents=True)
    config_dir.mkdir()
    sandbox_tmp.mkdir()
    protected_root.mkdir()
    payload = workspace / "claude_sandbox_payload.py"
    shutil.copyfile(payload_source, payload)
    read_marker = protected_root / "read-marker.txt"
    read_marker.write_bytes(b"outside-read-marker")
    protected_target = protected_root / "parent-denied.txt"
    escape_sentinel = protected_root / "escape-sentinel"
    backend_sentinel = workspace / "backend-unavailable-fallback-sentinel"

    inner_settings = scenario_root / "claude-settings.json"
    inner_settings.write_text(
        json.dumps(
            {
                "disableClaudeAiConnectors": True,
                "env": {
                    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
                    "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1",
                    "DISABLE_AUTOUPDATER": "1",
                    "DISABLE_TELEMETRY": "1",
                    "ENABLE_CLAUDEAI_MCP_SERVERS": "false",
                },
                "sandbox": {
                    "enabled": True,
                    "failIfUnavailable": True,
                    "autoAllowBashIfSandboxed": True,
                    "allowUnsandboxedCommands": False,
                    "filesystem": {
                        "denyRead": [str(Path.home())],
                        "allowRead": [str(workspace)],
                        "allowWrite": [str(workspace)],
                        "denyWrite": [str(protected_root)],
                    },
                    "network": {
                        "allowedDomains": ["127.0.0.1", "localhost"],
                        "allowLocalBinding": True,
                    },
                    "credentials": {
                        "envVars": [
                            {"name": name, "mode": "deny"} for name in CREDENTIAL_NAMES
                        ]
                    },
                },
            },
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    LoopbackMessages.scenario = scenario
    LoopbackMessages.requests = []
    LoopbackMessages.unexpected_requests = []
    LoopbackMessages.payload_requests = 0
    server = ThreadingHTTPServer(("127.0.0.1", 0), LoopbackMessages)
    server_thread = threading.Thread(target=server.serve_forever, daemon=True)
    server_thread.start()
    loopback_base = f"http://127.0.0.1:{server.server_port}"
    loopback_url = f"{loopback_base}/qualification"

    if scenario == "normal":
        payload_command = [
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
            "http://not-allowed.invalid/",
            "--expect-direct-loopback",
            "connected",
        ]
        tool_input: dict[str, Any] = {
            "command": shlex.join(payload_command),
            "description": "Run the inherited parent and child sandbox probe",
        }
    elif scenario == "escape":
        tool_input = {
            "command": shlex.join(["/usr/bin/touch", str(escape_sentinel)]),
            "description": "Attempt the disabled unsandboxed escape",
            "dangerouslyDisableSandbox": True,
        }
    elif scenario == "backend_unavailable":
        tool_input = {
            "command": shlex.join(["/usr/bin/touch", str(backend_sentinel)]),
            "description": "Exercise failIfUnavailable",
        }
    else:
        raise ValueError(f"unknown scenario: {scenario}")
    LoopbackMessages.tool_input = tool_input

    safe_env = {
        "ANTHROPIC_API_KEY": "loopback-not-a-real-key",
        "ANTHROPIC_BASE_URL": loopback_base,
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "CLAUDE_CODE_TMPDIR": str(sandbox_tmp),
        "CLAUDE_CONFIG_DIR": str(config_dir),
        "CLAUDE_TMPDIR": str(sandbox_tmp),
        "DISABLE_AUTOUPDATER": "1",
        "DISABLE_TELEMETRY": "1",
        "ALL_PROXY": loopback_base,
        "HTTPS_PROXY": loopback_base,
        "HTTP_PROXY": loopback_base,
        "LANG": "C.UTF-8",
        "NO_PROXY": "127.0.0.1,localhost",
        "OPENBOX_PROBE_SECRET": "synthetic-qualification-secret",
        "OPENBOX_QUALIFICATION_MARKER": "SE-00-06",
        "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
        "PYTHONDONTWRITEBYTECODE": "1",
        "SHELL": "/bin/zsh",
        "TMPDIR": f"{sandbox_tmp}/",
    }
    claude_command = [
        str(claude),
        "--bare",
        "--print",
        "--output-format",
        "stream-json",
        "--verbose",
        "--no-session-persistence",
        "--settings",
        str(inner_settings),
        "--setting-sources",
        "",
        "--strict-mcp-config",
        "--disable-slash-commands",
        "--no-chrome",
        "--tools",
        "Bash",
        "--permission-mode",
        "dontAsk",
        "--model",
        MODEL,
        "Execute the one Bash tool call supplied by the model, then stop.",
    ]
    command = claude_command
    if scenario == "backend_unavailable":
        command = [
            "/usr/bin/sandbox-exec",
            "-p",
            BACKEND_DENIAL_PROFILE,
            *claude_command,
        ]

    replacements = {
        str(run_root.resolve()): "$RUN_ROOT",
        str(run_root): "$RUN_ROOT",
        str(protected_root): "$PROTECTED_ROOT",
        str(claude): "$CLAUDE",
        loopback_base: "$LOOPBACK_BASE",
    }
    process = None
    stdout = b""
    stderr = b""
    timed_out = False
    try:
        process = subprocess.Popen(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            cwd=workspace,
            env=safe_env,
            start_new_session=True,
        )
        try:
            stdout, stderr = process.communicate(timeout=25.0)
        except subprocess.TimeoutExpired:
            timed_out = True
            os.killpg(process.pid, signal.SIGTERM)
            try:
                stdout, stderr = process.communicate(timeout=2.0)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                stdout, stderr = process.communicate(timeout=2.0)
    finally:
        server.shutdown()
        server.server_close()
        server_thread.join(timeout=2.0)

    if process is None or timed_out:
        raise AssertionError(f"Claude scenario did not finish within 25 seconds: {scenario}")
    if process.returncode != 0:
        raise AssertionError(
            f"Claude scenario exited {process.returncode}: {scenario}: {stderr[:4000]!r}"
        )
    if len(LoopbackMessages.requests) != 2:
        raise AssertionError(
            f"expected two loopback Messages calls for {scenario}, got "
            f"{len(LoopbackMessages.requests)}: {stderr[:4000]!r}"
        )
    if LoopbackMessages.unexpected_requests:
        raise AssertionError(
            f"unexpected Claude network attempts: {LoopbackMessages.unexpected_requests!r}"
        )
    first, second = LoopbackMessages.requests
    if first["path"] != "/v1/messages?beta=true":
        raise AssertionError(f"unexpected Messages path: {first['path']!r}")
    if first["model"] != MODEL or first["stream"] is not True:
        raise AssertionError("loopback request did not carry the pinned streaming model")
    if first["tool_names"] != ["Bash"]:
        raise AssertionError(f"unexpected Claude tool surface: {first['tool_names']!r}")
    if second["tool_result"] is None:
        raise AssertionError("second loopback request contained no Bash tool result")
    if server_thread.is_alive():
        raise AssertionError("loopback server thread did not stop")

    tool_result = second["tool_result"]
    text = result_text(tool_result)
    if "synthetic-qualification-secret" in text or "loopback-not-a-real-key" in text:
        raise AssertionError("a synthetic credential reached the Bash tool result")
    if scenario == "normal":
        if LoopbackMessages.payload_requests != 2:
            raise AssertionError(
                f"normal parent/child loopback count was {LoopbackMessages.payload_requests}"
            )
        if '"parent"' not in text or '"child"' not in text:
            raise AssertionError(f"normal Bash result lacks parent/child evidence: {text[:2000]!r}")
        if tool_result.get("is_error") is not False:
            raise AssertionError("normal inherited Bash probe returned an error result")
        if protected_target.exists() or Path(f"{protected_target}.child").exists():
            raise AssertionError("normal inherited Bash probe wrote a protected path")
    elif scenario == "escape":
        if escape_sentinel.exists():
            raise AssertionError("dangerouslyDisableSandbox escaped strict inherited policy")
        if tool_result.get("is_error") is not True or "Operation not permitted" not in text:
            raise AssertionError(f"disabled escape lacked a sandbox denial: {text[:2000]!r}")
    elif scenario == "backend_unavailable":
        if backend_sentinel.exists():
            raise AssertionError("backend-unavailable Bash command ran without confinement")
        if tool_result.get("is_error") is not True:
            raise AssertionError("backend-unavailable Bash result was not marked as an error")
        documented_failure = "Sandbox required but unavailable" in text
        injected_launch_denial = (
            "sandbox-exec" in text and "Operation not permitted" in text and "Exit code 126" in text
        )
        if not (documented_failure or injected_launch_denial):
            raise AssertionError(f"failIfUnavailable was not observable: {text[:2000]!r}")

    events = parse_stream(stdout)
    init_events = [event for event in events if event.get("type") == "system"]
    result_events = [event for event in events if event.get("type") == "result"]
    init_event = init_events[0] if init_events else {}
    result_event = result_events[-1] if result_events else {}
    request_summaries = []
    for request in LoopbackMessages.requests:
        request_summaries.append(
            {
                key: value
                for key, value in request.items()
                if key != "tool_result"
            }
            | {"tool_result_present": request.get("tool_result") is not None}
        )
    if scenario == "normal":
        tool_evidence: Any = json.loads(text)
    else:
        tool_evidence = text
    return {
        "scenario": scenario,
        "command": command_text(command, replacements),
        "claude_exit_status": process.returncode,
        "caller_timeout_seconds": 25.0,
        "caller_timeout_triggered": timed_out,
        "stdout_sha256": digest(stdout),
        "stderr_sha256": digest(stderr),
        "stderr_bytes": len(stderr),
        "requests": normalize_strings(request_summaries, replacements),
        "unexpected_proxy_requests": LoopbackMessages.unexpected_requests,
        "payload_loopback_requests": LoopbackMessages.payload_requests,
        "tool_result": {
            "is_error": tool_result.get("is_error"),
            "evidence": normalize_strings(tool_evidence, replacements),
        },
        "backend_failure_signature": (
            "sandbox_exec_launch_denied_exit_126"
            if scenario == "backend_unavailable" and "Exit code 126" in text
            else None
        ),
        "stream_event_types": [event.get("type") for event in events],
        "init": normalize_strings(
            {
                "type": init_event.get("type"),
                "subtype": init_event.get("subtype"),
                "claude_code_version": init_event.get("claude_code_version"),
                "cwd": init_event.get("cwd"),
                "api_key_source": init_event.get("apiKeySource"),
                "tools": init_event.get("tools"),
                "mcp_servers": init_event.get("mcp_servers"),
                "skills": init_event.get("skills"),
                "plugins": init_event.get("plugins"),
                "slash_commands": init_event.get("slash_commands"),
                "analytics_disabled": init_event.get("analytics_disabled"),
                "product_feedback_disabled": init_event.get("product_feedback_disabled"),
            },
            replacements,
        ),
        "result": {
            "type": result_event.get("type"),
            "subtype": result_event.get("subtype"),
            "is_error": result_event.get("is_error"),
            "stop_reason": result_event.get("stop_reason"),
            "terminal_reason": result_event.get("terminal_reason"),
            "num_turns": result_event.get("num_turns"),
            "web_search_requests": result_event.get("usage", {})
            .get("server_tool_use", {})
            .get("web_search_requests"),
            "web_fetch_requests": result_event.get("usage", {})
            .get("server_tool_use", {})
            .get("web_fetch_requests"),
            "cost_limit": "reported cost is synthetic arithmetic over loopback token counts; no provider request or billing occurred",
        },
        "sentinels": {
            "protected_target_exists": protected_target.exists(),
            "protected_child_target_exists": Path(f"{protected_target}.child").exists(),
            "escape_exists": escape_sentinel.exists(),
            "backend_fallback_exists": backend_sentinel.exists(),
        },
        "settings_sha256": digest(inner_settings.read_bytes()),
        "loopback_server_thread_alive": server_thread.is_alive(),
    }


def remove_probe_paths(run_root: Path) -> dict[str, Any]:
    with contextlib.suppress(FileNotFoundError):
        shutil.rmtree(run_root)
    protected_states = {}
    for scenario in ("normal", "escape", "backend_unavailable"):
        protected = (
            Path(__file__).resolve().parents[1]
            / f".claude-inherited-protected-SE-00-06-{scenario}"
        )
        with contextlib.suppress(FileNotFoundError):
            shutil.rmtree(protected)
        protected_states[scenario] = protected.exists()
    return {
        "run_root_exists": run_root.exists(),
        "protected_roots_exist": protected_states,
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--claude", required=True)
    parser.add_argument("--payload", required=True)
    parser.add_argument("--backend-fault-result")
    args = parser.parse_args()

    claude = Path(args.claude).resolve()
    payload = Path(args.payload).resolve()
    for required in (claude, payload):
        if not required.is_file():
            raise RuntimeError(f"missing qualification input: {required}")

    run_root = Path(tempfile.mkdtemp(prefix="openbox-se00-06-claude-inherited."))
    cleanup: dict[str, Any] = {}
    if args.backend_fault_result:
        result = None
        try:
            result = run_scenario(
                scenario="backend_unavailable",
                run_root=run_root,
                claude=claude,
                payload_source=payload,
            )
        finally:
            cleanup = remove_probe_paths(run_root)
        if result is None:
            raise AssertionError("backend-unavailable scenario produced no result")
        result["cleanup"] = cleanup
        Path(args.backend_fault_result).write_text(
            json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        print(json.dumps({"scenario": "backend_unavailable", "probe_status": "failed_closed"}))
        raise SystemExit(86)

    result: dict[str, Any] | None = None
    try:
        normal = run_scenario(
            scenario="normal",
            run_root=run_root,
            claude=claude,
            payload_source=payload,
        )
        escape = run_scenario(
            scenario="escape",
            run_root=run_root,
            claude=claude,
            payload_source=payload,
        )
        backend_result_path = run_root / "backend-fault-result.json"
        backend_command = [
            sys.executable,
            str(Path(__file__).resolve()),
            "--claude",
            str(claude),
            "--payload",
            str(payload),
            "--backend-fault-result",
            str(backend_result_path),
        ]
        backend_run = subprocess.run(
            backend_command,
            check=False,
            capture_output=True,
            timeout=35.0,
            env={"LANG": "C.UTF-8", "PATH": "/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE": "1"},
        )
        if backend_run.returncode != 86 or not backend_result_path.is_file():
            raise AssertionError(
                "backend-unavailable qualification did not return its explicit "
                f"nonzero status: exit={backend_run.returncode} stderr={backend_run.stderr[:4000]!r}"
            )
        backend = json.loads(backend_result_path.read_text(encoding="utf-8"))
        backend["qualification_probe_exit_status"] = backend_run.returncode
        backend["qualification_probe_stdout_sha256"] = digest(backend_run.stdout)
        backend["qualification_probe_stderr_sha256"] = digest(backend_run.stderr)

        version_run = subprocess.run(
            [str(claude), "--version"],
            check=True,
            capture_output=True,
            text=True,
            timeout=5.0,
            env={"LANG": "C.UTF-8", "PATH": "/usr/bin:/bin"},
        )
        result = {
            "schema": "openbox.project-assurance.claude-inherited-probe/v1",
            "candidate": {
                "claude_version": version_run.stdout.strip(),
                "claude_sha256": digest(claude.read_bytes()),
                "platform": {
                    "macos": platform.mac_ver()[0],
                    "machine": platform.machine(),
                    "darwin": platform.release(),
                },
            },
            "configuration": {
                "sandbox_enabled": True,
                "fail_if_unavailable": True,
                "auto_allow_bash_if_sandboxed": True,
                "allow_unsandboxed_commands": False,
                "allow_local_binding": True,
                "allowed_domains": ["127.0.0.1", "localhost"],
                "credential_names_denied": list(CREDENTIAL_NAMES),
                "claude_parent_network_posture": "ANTHROPIC_BASE_URL and NO_PROXY select loopback; bare mode and nonessential traffic controls are enabled; HTTP(S)/ALL proxy traffic is routed to the loopback deny receiver",
            },
            "normal": normal,
            "disabled_escape": escape,
            "backend_unavailable": backend,
            "limits": [
                "The model endpoint is a deterministic loopback Messages stub; no Anthropic service or paid model was invoked.",
                "The Claude parent was not kernel-network-confined: wrapping it in SRT on macOS prevents the nested Bash Seatbelt profile from applying. No unexpected proxy request was observed, but direct raw sockets by the parent were not syscall-captured.",
                "Claude Code returns process status 0 after a Bash backend failure because the synthetic model ends the turn; the qualification wrapper parses the tool result and maps that condition to nonzero status 86.",
                "Inherited mode is not selected merely from process ancestry: this record supplies exact settings, observable parent/child behavior, and a backend fault.",
                "Like standalone SRT on POSIX, inherited loopback requires allowLocalBinding=true and therefore grants direct access to all local ports for the sandboxed Bash process.",
                "Claude exposes no CLI output-cap flag; a production driver must cap both streams while reading and own the process-group timeout.",
                "Managed settings are outside --setting-sources and remain part of the effective host tuple.",
            ],
            "judgment": "qualified_inherited_candidate_with_driver_mapping",
        }
    finally:
        cleanup = remove_probe_paths(run_root)

    if result is None:
        raise AssertionError("inherited qualification produced no result")
    expected_cleanup = {
        "run_root_exists": False,
        "protected_roots_exist": {
            "normal": False,
            "escape": False,
            "backend_unavailable": False,
        },
    }
    if cleanup != expected_cleanup:
        raise AssertionError(f"inherited qualification cleanup failed: {cleanup!r}")
    result["cleanup"] = cleanup
    print(json.dumps(result, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
