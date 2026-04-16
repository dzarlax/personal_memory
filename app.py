"""Unified server: mounts all services on a single port."""

import os
import logging
import threading
from contextlib import asynccontextmanager

import uvicorn
from starlette.applications import Starlette
from starlette.routing import Mount
from dotenv import load_dotenv

load_dotenv()

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("app")

# --- Always load memory ---
from memory_server import mcp as memory_mcp, _init_collection, _backup_loop

# --- Conditionally load todoist ---
todoist_mcp = None
if os.getenv("ENABLE_TODOIST", "").lower() in ("1", "true", "yes"):
    if not os.getenv("TODOIST_TOKEN"):
        logger.warning("ENABLE_TODOIST=true but TODOIST_TOKEN not set, skipping todoist")
    else:
        from todoist_server import mcp as _todoist_mcp
        todoist_mcp = _todoist_mcp
        logger.info("Todoist MCP enabled")

# --- Conditionally load viz ---
viz_app = None
if os.getenv("ENABLE_VIZ", "").lower() in ("1", "true", "yes"):
    from viz_server import app as _viz_app
    viz_app = _viz_app
    logger.info("Visualization dashboard enabled at /viz")


def build_app() -> Starlette:
    routes: list[Mount] = []
    session_managers = []

    # Memory MCP — always on
    memory_mcp.settings.stateless_http = True
    memory_mcp.settings.transport_security = None
    memory_http = memory_mcp.streamable_http_app()
    routes.append(Mount("/memory", app=memory_http))
    session_managers.append(memory_mcp.session_manager)

    # Todoist MCP — optional
    if todoist_mcp is not None:
        todoist_mcp.settings.stateless_http = True
        todoist_mcp.settings.transport_security = None
        todoist_http = todoist_mcp.streamable_http_app()
        routes.append(Mount("/todoist", app=todoist_http))
        session_managers.append(todoist_mcp.session_manager)

    # Viz — optional
    if viz_app is not None:
        routes.append(Mount("/viz", app=viz_app))

    @asynccontextmanager
    async def lifespan(app):
        async with contextmanager_stack(session_managers):
            yield

    return Starlette(routes=routes, lifespan=lifespan)


@asynccontextmanager
async def contextmanager_stack(managers):
    """Nest multiple async context managers."""
    if not managers:
        yield
        return
    async with managers[0].run():
        async with contextmanager_stack(managers[1:]):
            yield


if __name__ == "__main__":
    _init_collection()

    # Start backup thread
    threading.Thread(target=_backup_loop, daemon=True).start()
    logger.info("Backup scheduler started")

    app = build_app()
    port = int(os.getenv("MCP_PORT", "8000"))
    logger.info("Listening on 0.0.0.0:%d", port)
    uvicorn.run(app, host="0.0.0.0", port=port)
