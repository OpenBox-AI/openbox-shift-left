#!/usr/bin/env python3
"""Loopback-only OpenBox LangGraph SDK wire and pre-effect qualification probe."""

from __future__ import annotations

import argparse
import asyncio
import base64
import hashlib
import ipaddress
import json
import os
import socket
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from importlib.metadata import version
from typing import Annotated, Any, TypedDict

from langchain_core.language_models.fake_chat_models import FakeMessagesListChatModel
from langchain_core.messages import AIMessage, BaseMessage, HumanMessage
from langchain_core.tools import tool
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages
from langgraph.prebuilt import ToolNode
from openbox_langgraph import create_openbox_graph_handler


TEST_API_KEY = "obx_test_project_assurance_qualification"
TEST_DID = "did:aip:12345678-1234-5678-1234-567812345678"
TEST_PRIVATE_KEY = base64.b64encode(bytes(range(32))).decode("ascii")


class AgentState(TypedDict):
    messages: Annotated[list[BaseMessage], add_messages]


class Recorder:
    def __init__(self, verdict_mode: str) -> None:
        self.verdict_mode = verdict_mode
        self.effects: list[str] = []
        self.requests: list[dict[str, Any]] = []
        self.non_loopback_attempts: list[str] = []
        self.lock = threading.Lock()

    def append_request(
        self,
        *,
        method: str,
        path: str,
        headers: Any,
        request_body: bytes,
        response_body: bytes,
        status: int,
        payload: dict[str, Any] | None,
    ) -> None:
        signature = headers.get("X-OpenBox-Agent-Signature", "")
        with self.lock:
            self.requests.append(
                {
                    "method": method,
                    "path": path,
                    "status": status,
                    "request_body_base64": base64.b64encode(request_body).decode("ascii"),
                    "request_body_sha256": hashlib.sha256(request_body).hexdigest(),
                    "response_body_base64": base64.b64encode(response_body).decode("ascii"),
                    "response_body_sha256": hashlib.sha256(response_body).hexdigest(),
                    "headers": {
                        "authorization": "Bearer obx_test_[REDACTED]"
                        if headers.get("Authorization", "").startswith("Bearer obx_test_")
                        else "unexpected",
                        "content_type": headers.get("Content-Type"),
                        "sdk_version": headers.get("X-OpenBox-SDK-Version"),
                        "agent_did": headers.get("X-OpenBox-Agent-DID"),
                        "body_sha256": headers.get("X-OpenBox-Body-SHA256"),
                        "signature_present": bool(signature),
                        "signature_sha256": hashlib.sha256(signature.encode("utf-8")).hexdigest()
                        if signature
                        else None,
                    },
                    "event_type": payload.get("event_type") if payload else None,
                    "activity_type": payload.get("activity_type") if payload else None,
                    "effect_count_at_receive": len(self.effects),
                }
            )


def receiver_handler(recorder: Recorder) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:  # noqa: N802 - stdlib handler API
            request_body = b""
            if self.path == "/api/v1/auth/validate":
                status = 200
                response_body = b'{"valid":true}'
            else:
                status = 404
                response_body = b'{"error":"unsupported"}'
            recorder.append_request(
                method="GET",
                path=self.path,
                headers=self.headers,
                request_body=request_body,
                response_body=response_body,
                status=status,
                payload=None,
            )
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(response_body)))
            self.end_headers()
            self.wfile.write(response_body)

        def do_POST(self) -> None:  # noqa: N802 - stdlib handler API
            length = int(self.headers.get("Content-Length", "0"))
            request_body = self.rfile.read(length)
            try:
                payload = json.loads(request_body)
            except json.JSONDecodeError:
                payload = None

            is_tool_start = bool(
                isinstance(payload, dict)
                and payload.get("event_type") == "ActivityStarted"
                and payload.get("activity_type") == "recording_tool"
            )
            if self.path != "/api/v1/governance/evaluate" or payload is None:
                status = 400
                response_body = b'{"error":"unsupported"}'
            elif recorder.verdict_mode == "mock-block" and is_tool_start:
                status = 200
                response_body = b'{"verdict":"block","reason":"qualification mock only"}'
            else:
                status = 200
                response_body = b'{"verdict":"allow"}'

            recorder.append_request(
                method="POST",
                path=self.path,
                headers=self.headers,
                request_body=request_body,
                response_body=response_body,
                status=status,
                payload=payload if isinstance(payload, dict) else None,
            )
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(response_body)))
            self.end_headers()
            self.wfile.write(response_body)

        def log_message(self, format: str, *args: Any) -> None:
            return

    return Handler


def install_loopback_guard(recorder: Recorder) -> Any:
    original_connect = socket.socket.connect

    def guarded_connect(sock: socket.socket, address: Any) -> Any:
        if isinstance(address, tuple):
            host = str(address[0])
            try:
                allowed = ipaddress.ip_address(host).is_loopback
            except ValueError:
                allowed = host == "localhost"
            if not allowed:
                recorder.non_loopback_attempts.append(repr(address))
                raise OSError(f"qualification probe denied non-loopback connection: {address!r}")
        return original_connect(sock, address)

    socket.socket.connect = guarded_connect
    return original_connect


def build_graph(recorder: Recorder) -> Any:
    @tool
    def recording_tool(text: str) -> str:
        """Record one bounded synthetic effect."""
        recorder.effects.append(text)
        return f"echo: {text}"

    model = FakeMessagesListChatModel(
        responses=[
            AIMessage(
                content="",
                tool_calls=[
                    {
                        "name": "recording_tool",
                        "args": {"text": "synthetic-effect"},
                        "id": "qualification-call-1",
                    }
                ],
            ),
            AIMessage(content="done"),
        ]
    )

    async def call_model(state: AgentState) -> dict[str, Any]:
        result = await model.ainvoke(state["messages"])
        return {"messages": [result]}

    def should_continue(state: AgentState) -> str:
        return "tools" if getattr(state["messages"][-1], "tool_calls", None) else END

    graph = StateGraph(AgentState)
    graph.add_node("agent", call_model)
    graph.add_node("tools", ToolNode([recording_tool], handle_tool_errors=False))
    graph.add_edge(START, "agent")
    graph.add_conditional_edges("agent", should_continue, {"tools": "tools", END: END})
    graph.add_edge("tools", "agent")
    return graph.compile()


async def run_probe(verdict_mode: str) -> dict[str, Any]:
    for name in (
        "OPENBOX_URL",
        "OPENBOX_API_URL",
        "OPENBOX_API_KEY",
        "OPENBOX_AGENT_DID",
        "OPENBOX_AGENT_PRIVATE_KEY",
    ):
        if os.environ.get(name):
            raise RuntimeError(f"refusing inherited OpenBox environment variable: {name}")

    recorder = Recorder(verdict_mode)
    server = ThreadingHTTPServer(("127.0.0.1", 0), receiver_handler(recorder))
    server_thread = threading.Thread(target=server.serve_forever, daemon=True)
    server_thread.start()
    original_connect = install_loopback_guard(recorder)
    handler = None
    observed_exception: str | None = None
    try:
        api_url = f"http://127.0.0.1:{server.server_port}"
        handler = create_openbox_graph_handler(
            build_graph(recorder),
            api_url=api_url,
            api_key=TEST_API_KEY,
            agent_did=TEST_DID,
            agent_private_key=TEST_PRIVATE_KEY,
            governance_timeout=5.0,
            validate=True,
            on_api_error="fail_closed",
        )
        try:
            await handler.ainvoke(
                {"messages": [HumanMessage(content="run the bounded synthetic tool")]},
                config={"configurable": {"thread_id": "sdk-wire-qualification"}},
            )
        except Exception as exc:  # the mock-block mode expects the SDK-native error
            observed_exception = type(exc).__name__
    finally:
        if handler is not None:
            await handler._client.close()
            if handler._core_runtime is not None:
                await handler._core_runtime.aclose()
        socket.socket.connect = original_connect
        server.shutdown()
        server.server_close()
        server_thread.join(timeout=2.0)

    tool_starts = [
        item
        for item in recorder.requests
        if item["event_type"] == "ActivityStarted"
        and item["activity_type"] == "recording_tool"
    ]
    if len(tool_starts) != 1:
        raise AssertionError(f"expected one tool ActivityStarted, got {len(tool_starts)}")
    if tool_starts[0]["effect_count_at_receive"] != 0:
        raise AssertionError("tool ActivityStarted arrived after the synthetic effect")
    if recorder.non_loopback_attempts:
        raise AssertionError(f"non-loopback attempts observed: {recorder.non_loopback_attempts}")
    if verdict_mode == "allow":
        if recorder.effects != ["synthetic-effect"] or observed_exception is not None:
            raise AssertionError(
                f"ALLOW did not run exactly one effect: effects={recorder.effects!r} "
                f"exception={observed_exception!r}"
            )
        classification = "baseline_allow_wire_observed"
    else:
        if recorder.effects or observed_exception != "GovernanceBlockedError":
            raise AssertionError(
                f"mock BLOCK did not stop the effect: effects={recorder.effects!r} "
                f"exception={observed_exception!r}"
            )
        classification = "sdk_mock_interception_only_not_openbox_block_proof"

    return {
        "schema": "openbox.project-assurance.sdk-wire-probe/v1",
        "verdict_mode": verdict_mode,
        "classification": classification,
        "packages": {
            name: version(name)
            for name in (
                "openbox-langgraph-sdk-python",
                "openbox-langchain-sdk-python",
                "openbox-sdk-python",
                "langgraph",
                "langchain-core",
                "httpx",
            )
        },
        "test_identity": {
            "api_key_prefix": "obx_test_",
            "signed": True,
            "did": TEST_DID,
            "private_key_persisted": False,
        },
        "requests": recorder.requests,
        "tool_effects": recorder.effects,
        "observed_exception": observed_exception,
        "non_loopback_attempts": recorder.non_loopback_attempts,
        "limits": [
            "The receiver is a loopback fixture, not OpenBox Core.",
            "A mock BLOCK proves SDK pre-effect application only; it is not a blocked outcome.",
            "The fake chat model proves deterministic graph/tool ordering without a model-provider call.",
        ],
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--verdict", choices=("allow", "mock-block"), required=True)
    args = parser.parse_args()
    print(json.dumps(asyncio.run(run_probe(args.verdict)), indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
