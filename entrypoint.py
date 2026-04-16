"""Single entrypoint that runs all enabled services in one container."""

import os
import sys
import signal
import subprocess
import logging

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(message)s")
logger = logging.getLogger("entrypoint")


def main():
    procs: list[subprocess.Popen] = []

    # memory_server.py is always on
    logger.info("Starting memory_server.py on port %s", os.getenv("MCP_PORT", "8000"))
    procs.append(subprocess.Popen([sys.executable, "memory_server.py"]))

    if os.getenv("ENABLE_TODOIST", "").lower() in ("1", "true", "yes"):
        if not os.getenv("TODOIST_TOKEN"):
            logger.warning("ENABLE_TODOIST=true but TODOIST_TOKEN not set, skipping")
        else:
            port = os.getenv("TODOIST_MCP_PORT", "8001")
            logger.info("Starting todoist_server.py on port %s", port)
            procs.append(subprocess.Popen(
                [sys.executable, "todoist_server.py"],
                env={**os.environ, "MCP_PORT": port},
            ))

    if os.getenv("ENABLE_VIZ", "").lower() in ("1", "true", "yes"):
        port = os.getenv("VIZ_PORT", "8080")
        logger.info("Starting viz_server.py on port %s", port)
        procs.append(subprocess.Popen([sys.executable, "viz_server.py"]))

    def shutdown(sig, frame):
        logger.info("Shutting down...")
        for p in procs:
            p.terminate()
        for p in procs:
            p.wait(timeout=10)
        sys.exit(0)

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)

    # Wait for any process to exit — if one dies, bring everything down
    while True:
        for p in procs:
            ret = p.poll()
            if ret is not None:
                logger.error("Process %s exited with code %d, shutting down", p.args, ret)
                shutdown(None, None)
        try:
            procs[0].wait(timeout=1)
        except subprocess.TimeoutExpired:
            pass


if __name__ == "__main__":
    main()
