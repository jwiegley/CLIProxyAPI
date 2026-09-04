"""One-shot bridge from CLIProxyAPI to the public Factory Droid Python SDK."""

from __future__ import annotations

import asyncio
import base64
import json
import sys
import threading
from collections.abc import Mapping, Sequence
from typing import Any


def emit(payload: dict[str, Any]) -> None:
    print(json.dumps(payload, separators=(",", ":")), flush=True)


def thaw(value: Any) -> Any:
    if isinstance(value, Mapping):
        return {str(key): thaw(item) for key, item in value.items()}
    if isinstance(value, Sequence) and not isinstance(value, (str, bytes, bytearray)):
        return [thaw(item) for item in value]
    return value


async def list_factory_models(request: dict[str, Any]) -> None:
    from droid_sdk import Runtime, list_models

    runtime = Runtime(executable=request.get("droid_command") or None)
    models = await list_models(
        cwd=request.get("cwd") or None,
        runtime=runtime,
    )
    emit(
        {
            "type": "models",
            "models": [
                {
                    "id": model.id,
                    "display_name": model.display_name,
                    "provider": model.model_provider.value,
                    "reasoning_efforts": [
                        effort.value for effort in model.supported_reasoning_efforts
                    ],
                    "default_reasoning_effort": model.default_reasoning_effort.value,
                    "no_image_support": model.no_image_support,
                }
                for model in models
            ],
        }
    )


def watch_for_cancel(loop: asyncio.AbstractEventLoop, task: asyncio.Task[None]) -> None:
    try:
        line = sys.stdin.readline()
    except (OSError, ValueError):
        line = ""
    if not line:
        loop.call_soon_threadsafe(task.cancel)
        return
    try:
        request = json.loads(line)
    except json.JSONDecodeError:
        request = {}
    if request.get("operation") == "cancel":
        loop.call_soon_threadsafe(task.cancel)


def usage_payload(usage: Any) -> dict[str, Any] | None:
    if usage is None:
        return None
    return {
        "input_tokens": usage.input_tokens,
        "output_tokens": usage.output_tokens,
        "cache_creation_tokens": usage.cache_creation_tokens,
        "cache_read_tokens": usage.cache_read_tokens,
        "thinking_tokens": usage.thinking_tokens,
    }


async def run_factory(request: dict[str, Any]) -> None:
    from droid_sdk import (
        Autonomy,
        Document,
        Image,
        JsonSchema,
        ReasoningEffort,
        Runtime,
        Session,
        SessionConfig,
        TextDelta,
    )

    current = asyncio.current_task()
    assert current is not None
    threading.Thread(
        target=watch_for_cancel,
        args=(asyncio.get_running_loop(), current),
        daemon=True,
    ).start()

    images = [
        Image.from_bytes(
            base64.b64decode(image["data"], validate=True),
            media_type=image["media_type"],
        )
        for image in request.get("images", [])
    ]
    files = []
    for document in request.get("files", []):
        if document["kind"] == "pdf":
            files.append(
                Document.from_bytes(
                    base64.b64decode(document["data"], validate=True),
                    name=document.get("name"),
                )
            )
        else:
            files.append(
                Document.from_text(
                    document["data"],
                    name=document.get("name"),
                    mime=document.get("mime_type"),
                )
            )

    system_prompt = None
    if request.get("system_prompt"):
        system_prompt = {
            "type": "preset",
            "preset": "droid",
            "append": request["system_prompt"],
        }
    config = SessionConfig(
        system_prompt=system_prompt,
        autonomy=Autonomy(request.get("autonomy") or "off"),
        restrict_tools=tuple(request.get("tools") or ()),
        auto_reject_permission_requests=True,
        disable_builtin_skills=not bool(request.get("enable_builtin_skills")),
    )
    runtime = Runtime(executable=request.get("droid_command") or None)
    effort = request.get("reasoning_effort")
    output = (
        JsonSchema(request["output_schema"]) if request.get("output_schema") else None
    )

    async with Session(
        cwd=request.get("cwd") or None,
        model=request["model"],
        reasoning_effort=ReasoningEffort(effort) if effort else None,
        config=config,
        runtime=runtime,
    ) as session:
        async with session.stream(
            request.get("prompt", ""),
            images=images,
            files=files,
            output=output,
            include_partial_messages=True,
        ) as stream:
            async for event in stream:
                if isinstance(event, TextDelta):
                    emit({"type": "text_delta", "text": event.text})
        result = stream.result

    payload = {
        "type": "result",
        "success": result.success,
        "subtype": result.subtype,
        "text": result.text,
        "usage": usage_payload(result.usage),
        "structured_output": thaw(result.structured_output),
        "session_id": result.session_id,
    }
    if result.error is not None:
        payload["message"] = result.error.message
    if result.structured_output_error is not None:
        payload["message"] = result.structured_output_error.message
    emit(payload)


async def main() -> None:
    request = json.loads(sys.stdin.readline())
    operation = request.get("operation")
    if operation == "models":
        await list_factory_models(request)
        return
    if operation == "run":
        await run_factory(request)
        return
    raise ValueError(f"unsupported bridge operation: {operation!r}")


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except asyncio.CancelledError:
        raise SystemExit(130) from None
    except ModuleNotFoundError as exc:
        emit(
            {
                "type": "error",
                "code": "sdk_unavailable",
                "message": f"Python module unavailable: {exc.name}",
            }
        )
        raise SystemExit(2) from None
    except Exception as exc:  # noqa: BLE001 - process boundary must return structured failures
        code = (
            "invalid_attachment"
            if type(exc).__name__ == "InvalidAttachmentError"
            else "sdk_error"
        )
        emit(
            {
                "type": "error",
                "code": code,
                "message": f"{type(exc).__name__}: {exc}",
            }
        )
        raise SystemExit(1) from None
