#!/usr/bin/env python3
"""Compose the standalone and inherited Claude qualification records."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def run(command: list[str], timeout: float) -> tuple[dict[str, Any], bytes]:
    environment = dict(os.environ)
    environment["PYTHONDONTWRITEBYTECODE"] = "1"
    completed = subprocess.run(
        command,
        check=False,
        capture_output=True,
        timeout=timeout,
        env=environment,
    )
    if completed.returncode != 0:
        raise AssertionError(
            f"qualification command exited {completed.returncode}: {command!r}: "
            f"{completed.stderr[:4000]!r}"
        )
    return json.loads(completed.stdout), completed.stdout


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--python", required=True)
    parser.add_argument("--node", required=True)
    parser.add_argument("--srt-cli", required=True)
    parser.add_argument("--srt-tarball", required=True)
    parser.add_argument("--claude", required=True)
    parser.add_argument("--payload", required=True)
    parser.add_argument("--standalone-probe", required=True)
    parser.add_argument("--inherited-probe", required=True)
    args = parser.parse_args()

    python = Path(args.python).resolve()
    node = Path(args.node).resolve()
    srt_cli = Path(args.srt_cli).resolve()
    srt_tarball = Path(args.srt_tarball).resolve()
    claude = Path(args.claude).resolve()
    payload = Path(args.payload).resolve()
    standalone_probe = Path(args.standalone_probe).resolve()
    inherited_probe = Path(args.inherited_probe).resolve()
    for path in (
        python,
        node,
        srt_cli,
        srt_tarball,
        claude,
        payload,
        standalone_probe,
        inherited_probe,
    ):
        if not path.is_file():
            raise RuntimeError(f"missing qualification input: {path}")

    standalone_command = [
        str(python),
        str(standalone_probe),
        "--node",
        str(node),
        "--srt-cli",
        str(srt_cli),
        "--payload",
        str(payload),
        "--package-tarball",
        str(srt_tarball),
    ]
    inherited_command = [
        str(python),
        str(inherited_probe),
        "--claude",
        str(claude),
        "--payload",
        str(payload),
    ]
    standalone, standalone_stdout = run(standalone_command, 30.0)
    inherited, inherited_stdout = run(inherited_command, 45.0)

    artifact = {
        "schema": "openbox.project-assurance.claude-sandbox-qualification/v1",
        "captured_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace(
            "+00:00", "Z"
        ),
        "task": "SE-00-06",
        "platform": {"macos": "26.5.2", "darwin": "25.5.0", "machine": "arm64"},
        "sources": [
            {
                "kind": "official_documentation",
                "url": "https://code.claude.com/docs/en/sandboxing",
                "use": "inherited Bash sandbox and strict fallback settings",
            },
            {
                "kind": "official_documentation",
                "url": "https://code.claude.com/docs/en/sandbox-environments",
                "use": "standalone Sandbox Runtime boundary",
            },
            {
                "kind": "official_documentation",
                "url": "https://code.claude.com/docs/en/cli-usage",
                "use": "bare non-interactive command flags",
            },
            {
                "kind": "official_documentation",
                "url": "https://code.claude.com/docs/en/configuration",
                "use": "settings keys and precedence",
            },
            {
                "kind": "official_documentation",
                "url": "https://code.claude.com/docs/en/env-vars",
                "use": "nonessential traffic and updater controls",
            },
            {
                "kind": "npm_tarball",
                "url": "https://registry.npmjs.org/@anthropic-ai/sandbox-runtime/-/sandbox-runtime-0.0.73.tgz",
                "sha256": digest(srt_tarball.read_bytes()),
            },
        ],
        "inputs": {
            "claude": {
                "version": inherited["candidate"]["claude_version"],
                "path": str(claude),
                "sha256": digest(claude.read_bytes()),
            },
            "sandbox_runtime": {
                "package": "@anthropic-ai/sandbox-runtime",
                "version": standalone["candidate"]["package_version"],
                "tarball_sha256": digest(srt_tarball.read_bytes()),
                "cli_reported_version_limit": standalone["candidate"]["cli_version_limit"],
            },
            "node": {"version": standalone["candidate"]["node_version"], "path": str(node)},
        },
        "commands": {
            "standalone": standalone_command,
            "inherited": inherited_command,
        },
        "captured_output_sha256": {
            "standalone": digest(standalone_stdout),
            "inherited": digest(inherited_stdout),
        },
        "probe_script_sha256": {
            Path(__file__).name: digest(Path(__file__).read_bytes()),
            payload.name: digest(payload.read_bytes()),
            standalone_probe.name: digest(standalone_probe.read_bytes()),
            inherited_probe.name: digest(inherited_probe.read_bytes()),
        },
        "standalone": standalone,
        "inherited": inherited,
        "judgment": {
            "standalone": "qualified feasibility candidate; Phase 03 still owns support-tuple admission",
            "inherited": "qualified feasibility candidate only when the driver parses Bash tool failures and maps backend unavailability to a nonzero probe result",
            "first_driver": "Codex 0.148.0 remains ADR-0020's first candidate",
            "live_proof": "No paid model, Anthropic service, production credential, or production endpoint was used.",
        },
        "retained_limits": [
            "SRT and inherited Bash require allowLocalBinding=true for the loopback baseline on this POSIX/macOS tuple, which grants sandboxed commands direct access to all local ports.",
            "Standalone SRT and Claude CLI expose no native output cap; the future driver must stream-cap stdout and stderr and own process-group timeout and cleanup.",
            "The inherited Claude parent was not kernel-network-confined because an outer macOS Seatbelt profile prevents its nested Bash Seatbelt profile from applying. No unexpected proxy request was observed, but direct raw parent sockets were not syscall-captured.",
            "Claude returns process status 0 after a Bash tool backend failure if the model ends the turn. The qualification wrapper verified the error tool result, absence of the sentinel, and a real mapped exit status 86.",
            "Managed Claude settings remain outside --setting-sources and are part of the effective support tuple.",
            "The synthetic result stream reports cost arithmetic from mock token counts; no provider request or billing occurred.",
        ],
    }
    print(json.dumps(artifact, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
