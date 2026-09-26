#!/usr/bin/env python3
"""Exercise a live SQLite connection across TTC local and remote forks."""

import argparse
import os
import sqlite3
import sys
import time
import traceback


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--state", default="/state")
    parser.add_argument("--journal", choices=("wal", "delete"), default="wal")
    parser.add_argument("--mmap-size", type=int, default=0)
    parser.add_argument("--sleep-ms", type=float, default=1.0)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    os.makedirs(args.state, exist_ok=True)
    database = os.path.join(args.state, "stress.db")
    connection = sqlite3.connect(database, timeout=30, isolation_level=None)
    actual_journal = connection.execute(
        f"PRAGMA journal_mode={args.journal}"
    ).fetchone()[0]
    connection.execute("PRAGMA synchronous=FULL")
    connection.execute(f"PRAGMA mmap_size={args.mmap_size}")
    actual_mmap = connection.execute("PRAGMA mmap_size").fetchone()[0]
    connection.execute(
        "CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY, value BLOB NOT NULL)"
    )
    sequence = connection.execute("SELECT COALESCE(MAX(seq), 0) FROM events").fetchone()[0]
    print(
        f"ready pid={os.getpid()} journal={actual_journal} mmap_size={actual_mmap} "
        f"sequence={sequence}",
        flush=True,
    )
    payload = os.urandom(4096)
    while True:
        sequence += 1
        connection.execute(
            "INSERT INTO events(seq, value) VALUES (?, ?)", (sequence, payload)
        )
        if sequence % 100 == 0:
            integrity = connection.execute("PRAGMA quick_check").fetchone()[0]
            with open(os.path.join(args.state, "heartbeat"), "w", encoding="ascii") as output:
                output.write(f"{sequence} {integrity}\n")
        if args.sleep_ms:
            time.sleep(args.sleep_ms / 1000)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except sqlite3.Error as error:
        print(
            "sqlite failure: "
            f"{error!r} code={getattr(error, 'sqlite_errorcode', None)} "
            f"name={getattr(error, 'sqlite_errorname', None)}",
            file=sys.stderr,
            flush=True,
        )
        traceback.print_exc()
        raise
