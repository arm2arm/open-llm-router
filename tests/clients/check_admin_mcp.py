#!/usr/bin/env python3
"""MCP interoperability smoke check. Requires the Python mcp SDK used by Hermes.

OLR_ADMIN_BASE_URL=http://127.0.0.1:8080 OLR_ADMIN_API_KEY=... \
  uv run --with 'mcp==2.0.0' python tests/clients/check_admin_mcp.py
"""
import asyncio
import json
import os
import uuid

from mcp import ClientSession
from mcp.client import streamable_http as transport

# Hermes supports both SDK generations. Each transport needs its own HTTP
# client implementation (the 2.x SDK uses httpx2).
httpx = getattr(transport, "httpx2", None) or transport.httpx


def tool_error(result):
    return getattr(result, "is_error", getattr(result, "isError", False))


async def check():
    url = os.environ["OLR_ADMIN_BASE_URL"].rstrip("/") + "/api/admin/v1/mcp"
    headers = {"Authorization": "Bearer " + os.environ["OLR_ADMIN_API_KEY"]}
    async with httpx.AsyncClient(headers=headers) as client, transport.streamable_http_client(url, http_client=client) as streams:
        read, write = streams[:2]
        async with ClientSession(read, write) as session:
            await session.initialize()
            tools = await session.list_tools()
            names = {tool.name for tool in tools.tools}
            assert {"get_identity", "get_change_status"} <= names
            identity = await session.call_tool("get_identity", {})
            assert not tool_error(identity)
            data = json.loads(identity.content[0].text)
            assert data["key_id"] and data["source_ip"]
            if os.environ.get("OLR_TEST_MCP_WRITE") == "1":
                arguments = {"parameters": {"id": os.environ["OLR_TEST_MCP_USER_ID"], "field": "tokens_per_day"}, "body": {"value": 321, "reason": "MCP interoperability test"}}
                preview = await session.call_tool("preview_change", {"operation": "set_user_override", "arguments": arguments})
                assert not tool_error(preview)
                change = json.loads(preview.content[0].text)
                applied = await session.call_tool("apply_change", {"id": change["id"], "arguments": arguments, "idempotency_key": str(uuid.uuid4())})
                assert not tool_error(applied)
                status = await session.call_tool("get_change_status", {"id": change["id"]})
                assert not tool_error(status)
                assert json.loads(status.content[0].text)["status"] == "succeeded"

            # The protocol client sees no inference tools or general shell tools.
            assert "chat_completions" not in names and "shell" not in names
    async with httpx.AsyncClient() as client:
        denied = await client.post(url, json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
        assert denied.status_code == 401
    print("Administration MCP initialize, discovery, identity and unauthorized access passed")


if __name__ == "__main__":
    asyncio.run(check())
