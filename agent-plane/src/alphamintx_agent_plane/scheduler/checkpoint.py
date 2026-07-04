"""LangGraph checkpoint DB isolation (persistence-and-api.md §checkpoint/resume).

ALL sqlite/SqliteSaver construction lives in this ONE module: the CI boundary
gate bans ``sqlite3`` everywhere else in agent-plane to protect the
CONTROL-PLANE store — this local checkpoint DB is a separate SQLite file
explicitly sanctioned by the spec, never the control-plane DB.

A corrupt or unopenable checkpoint DB at startup is a FAIL-FAST error (operator
alert; never silently recreated — ticks are recomputable, silent data loss is
not diagnosable). A missing file is a fresh deployment, not corruption.
"""

from __future__ import annotations

import sqlite3
from pathlib import Path

from langgraph.checkpoint.sqlite import SqliteSaver

ENV_CHECKPOINT_DB = "ALPHAMINTX_CHECKPOINT_DB"


class CheckpointCorruptionError(RuntimeError):
    """The checkpoint DB exists but cannot be opened or fails integrity_check."""


def open_checkpointer(path: str) -> SqliteSaver:
    """Open (or freshly create) the checkpoint DB and wrap it in a SqliteSaver.

    ``check_same_thread=False`` because the graph runs in a worker thread
    (``asyncio.to_thread``) while the saver is constructed on the main thread.
    """
    db_path = Path(path)
    existed = db_path.exists()
    if not existed:
        db_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        connection = sqlite3.connect(path, check_same_thread=False)
    except sqlite3.Error as exc:
        raise CheckpointCorruptionError(
            f"checkpoint DB {path} could not be opened: {exc}"
        ) from exc
    if existed:
        try:
            row = connection.execute("PRAGMA integrity_check").fetchone()
        except sqlite3.DatabaseError as exc:
            connection.close()
            raise CheckpointCorruptionError(
                f"checkpoint DB {path} is corrupt (not a database): {exc}"
            ) from exc
        if row is None or row[0] != "ok":
            connection.close()
            raise CheckpointCorruptionError(
                f"checkpoint DB {path} failed PRAGMA integrity_check: {row!r}"
            )
    return SqliteSaver(connection)
