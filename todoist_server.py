import os
import httpx
from dotenv import load_dotenv
from mcp.server.fastmcp import FastMCP

load_dotenv()

TODOIST_TOKEN = os.getenv("TODOIST_TOKEN", "")

mcp = FastMCP("todoist")

todoist: httpx.Client | None = None


def _get_client() -> httpx.Client:
    global todoist
    if todoist is None:
        if not TODOIST_TOKEN:
            raise RuntimeError("TODOIST_TOKEN environment variable is required")
        todoist = httpx.Client(
            base_url="https://api.todoist.com/api/v1",
            headers={"Authorization": f"Bearer {TODOIST_TOKEN}"},
            timeout=10.0,
        )
    return todoist


# ---------------------------------------------------------------------------
# Tools
# ---------------------------------------------------------------------------

@mcp.tool()
def get_projects() -> str:
    """List all Todoist projects."""
    try:
        r = _get_client().get("/projects")
        r.raise_for_status()
        result = r.json()
        projects = result.get("results", result) if isinstance(result, dict) else result
        if not projects:
            return "No projects found."
        lines = [f"[{p['id']}] {p['name']}" for p in projects]
        return "\n".join(lines)
    except Exception as e:
        return f"Error: {e}"


@mcp.tool()
def get_labels() -> str:
    """List all personal Todoist labels."""
    try:
        r = _get_client().get("/labels")
        r.raise_for_status()
        result = r.json()
        labels = result.get("results", result) if isinstance(result, dict) else result
        if not labels:
            return "No labels found."
        return "\n".join(f"[{l['id']}] {l['name']}" for l in labels)
    except Exception as e:
        return f"Error: {e}"


@mcp.tool()
def get_tasks(project_id: str | None = None, filter: str | None = None, limit: int = 20) -> str:
    """List active tasks. Optionally filter by project_id or Todoist filter string (e.g. 'today', 'p1').

    Args:
        project_id: Filter by project ID.
        filter: Todoist filter query (e.g. 'today', 'overdue', '#Work', '@label').
        limit: Max tasks to return.
    """
    try:
        params: dict = {}
        if filter:
            params["query"] = filter
            r = _get_client().get("/tasks/filter", params=params)
        else:
            if project_id:
                params["project_id"] = project_id
            r = _get_client().get("/tasks", params=params)
        r.raise_for_status()
        result = r.json()
        tasks = (result.get("results", result) if isinstance(result, dict) else result)[:limit]

        if not tasks:
            return "No tasks found."

        lines = []
        for t in tasks:
            due = t.get("due", {})
            due_str = f" [due: {due['string']}]" if due else ""
            priority = t.get("priority", 1)
            prio_str = f" [p{priority}]" if priority > 1 else ""
            label_str = f" #{' #'.join(t['labels'])}" if t.get("labels") else ""
            lines.append(f"[{t['id']}]{prio_str}{due_str}{label_str} {t['content']}")
        return "\n".join(lines)
    except Exception as e:
        return f"Error: {e}"


@mcp.tool()
def create_task(
    content: str,
    project_id: str | None = None,
    due_string: str | None = None,
    priority: int = 1,
    labels: list[str] | None = None,
) -> str:
    """Create a new Todoist task.

    Args:
        content: Task title/description.
        project_id: Project to add the task to (uses Inbox if omitted).
        due_string: Natural language due date, e.g. 'tomorrow', 'next Monday', 'in 3 days'.
        priority: 1 (normal) to 4 (urgent).
        labels: List of label names to assign, e.g. ['work', 'urgent'].
    """
    try:
        body: dict = {"content": content, "priority": priority}
        if project_id:
            body["project_id"] = project_id
        if due_string:
            body["due_string"] = due_string
        if labels:
            body["labels"] = labels

        r = _get_client().post("/tasks", json=body)
        r.raise_for_status()
        t = r.json()
        due = t.get("due", {})
        due_str = f", due: {due['string']}" if due else ""
        label_str = f", labels: {', '.join(t['labels'])}" if t.get("labels") else ""
        return f"Created task [{t['id']}]: {t['content']}{due_str}{label_str}"
    except Exception as e:
        return f"Error: {e}"


@mcp.tool()
def complete_task(task_id: str) -> str:
    """Mark a task as complete by its ID.

    Args:
        task_id: The task ID (from get_tasks output).
    """
    try:
        r = _get_client().post(f"/tasks/{task_id}/close")
        r.raise_for_status()
        return f"Task {task_id} completed."
    except Exception as e:
        return f"Error: {e}"


@mcp.tool()
def update_task(
    task_id: str,
    content: str | None = None,
    due_string: str | None = None,
    priority: int | None = None,
    labels: list[str] | None = None,
) -> str:
    """Update an existing task.

    Args:
        task_id: The task ID (from get_tasks output).
        content: New task title.
        due_string: New due date in natural language.
        priority: New priority (1-4).
        labels: Replace label list with these names, e.g. ['work']. Pass [] to clear all labels.
    """
    try:
        body: dict = {}
        if content is not None:
            body["content"] = content
        if due_string is not None:
            body["due_string"] = due_string
        if priority is not None:
            body["priority"] = priority
        if labels is not None:
            body["labels"] = labels

        if not body:
            return "Nothing to update."

        r = _get_client().post(f"/tasks/{task_id}", json=body)
        r.raise_for_status()
        t = r.json()
        label_str = f", labels: {', '.join(t['labels'])}" if t.get("labels") else ""
        return f"Updated task [{t['id']}]: {t['content']}{label_str}"
    except Exception as e:
        return f"Error: {e}"


@mcp.tool()
def delete_task(task_id: str) -> str:
    """Delete a task permanently.

    Args:
        task_id: The task ID (from get_tasks output).
    """
    try:
        r = _get_client().delete(f"/tasks/{task_id}")
        r.raise_for_status()
        return f"Task {task_id} deleted."
    except Exception as e:
        return f"Error: {e}"
