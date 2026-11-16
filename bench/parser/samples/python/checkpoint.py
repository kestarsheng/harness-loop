"""Parser accuracy fixture: Python runtime sample."""

import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterable


@dataclass
class Checkpoint:
    task_id: str
    seq: int
    payload: dict = field(default_factory=dict)


class CheckpointStore:
    """Append-only checkpoint store backed by one JSONL file per task."""

    def __init__(self, root: Path) -> None:
        self.root = root
        self.root.mkdir(parents=True, exist_ok=True)

    def path_for(self, task_id: str) -> Path:
        return self.root / f"{task_id}.jsonl"

    def append(self, checkpoint: Checkpoint) -> None:
        target = self.path_for(checkpoint.task_id)
        with target.open("a", encoding="utf-8") as handle:
            handle.write(json.dumps(checkpoint.__dict__) + "\n")

    def load(self, task_id: str) -> list[Checkpoint]:
        target = self.path_for(task_id)
        if not target.exists():
            return []
        rows = []
        with target.open("r", encoding="utf-8") as handle:
            for line in handle:
                text = line.strip()
                if text:
                    rows.append(Checkpoint(**json.loads(text)))
        return rows


def latest(store: CheckpointStore, task_id: str) -> Checkpoint | None:
    rows = store.load(task_id)
    if not rows:
        return None
    return max(rows, key=lambda item: item.seq)


def merge(batches: Iterable[list[Checkpoint]]) -> list[Checkpoint]:
    by_seq = {}
    for batch in batches:
        for checkpoint in batch:
            by_seq[checkpoint.seq] = checkpoint
    return [by_seq[seq] for seq in sorted(by_seq)]


def env_root(default: str = "./checkpoints") -> Path:
    return Path(os.environ.get("HARNESS_CHECKPOINT_DIR", default))
