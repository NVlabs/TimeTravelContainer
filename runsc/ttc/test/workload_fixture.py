#!/usr/bin/env python3
"""Long-lived memory, CPU, and SQLite workload for TTC stress qualification."""

import hashlib
import os
import sqlite3
import sys
import time


STATE = os.environ.get("TTC_STRESS_STATE", "/state")
MEMORY_MIB = int(os.environ.get("TTC_STRESS_MEMORY_MIB", "192"))
PAGE = 4096


def page_byte(index: int) -> int:
    return (index * 131 + 17) & 0xFF


def main() -> int:
    os.makedirs(STATE, exist_ok=True)
    memory = bytearray(MEMORY_MIB * 1024 * 1024)
    pages = len(memory) // PAGE
    for index in range(pages):
        memory[index * PAGE] = page_byte(index)

    database = os.path.join(STATE, "workload.db")
    connection = sqlite3.connect(database, timeout=30, isolation_level=None)
    connection.execute("PRAGMA journal_mode=WAL")
    connection.execute("PRAGMA synchronous=FULL")
    connection.execute(
        "CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY, digest TEXT NOT NULL)"
    )

    sequence = connection.execute("SELECT COALESCE(MAX(seq), 0) FROM events").fetchone()[0]
    digest = hashlib.sha256()
    while True:
        index = sequence % pages
        if memory[index * PAGE] != page_byte(index):
            raise RuntimeError(f"memory corruption on page {index}")
        digest.update(memory[index * PAGE : index * PAGE + 64])
        sequence += 1
        connection.execute(
            "INSERT INTO events(seq, digest) VALUES (?, ?)",
            (sequence, digest.hexdigest()),
        )
        if sequence % 25 == 0:
            with open(os.path.join(STATE, "heartbeat"), "w", encoding="ascii") as output:
                output.write(f"{sequence}\n")
        if sequence % 100 == 0:
            # Bound CPU consumption while retaining continuous dirty-memory and
            # durable filesystem activity.
            time.sleep(0.01)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except BaseException as error:
        print(f"workload failed: {error}", file=sys.stderr, flush=True)
        raise
