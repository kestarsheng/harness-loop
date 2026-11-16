"""Scheduler fixture: decorators, async methods and module-level helpers."""

import asyncio
import time
from typing import Any

from .checkpoint import Checkpoint, CheckpointStore

MAX_ATTEMPTS = 3


class RetryPolicy:
    def __init__(self, attempts: int = MAX_ATTEMPTS, pause: float = 0.2) -> None:
        self.attempts = attempts
        self.pause = pause

    @property
    def label(self) -> str:
        return f"retry:{self.attempts}"

    def delay(self, attempt: int) -> float:
        return self.pause * (2 ** attempt)


class Scheduler:
    """Runs one task's stages with retry and checkpointing."""

    def __init__(self, store: CheckpointStore, policy: RetryPolicy | None = None) -> None:
        self.store = store
        self.policy = policy or RetryPolicy()

    async def run_stage(self, task_id: str, stage: str, work: Any) -> Checkpoint:
        attempt = 0
        while attempt < self.policy.attempts:
            try:
                result = await work()
                checkpoint = Checkpoint(task_id=task_id, seq=attempt + 1, payload={"stage": stage})
                self.store.append(checkpoint)
                return checkpoint
            except RuntimeError:
                attempt += 1
                await asyncio.sleep(self.policy.delay(attempt))
        raise RuntimeError(f"stage {stage} exhausted retries")


def describe(policy: RetryPolicy) -> str:
    return f"{policy.label} pause={policy.pause:.2f}"


def timed(fn):
    def wrapper(*args, **kwargs):
        started = time.monotonic()
        value = fn(*args, **kwargs)
        elapsed = time.monotonic() - started
        return value, elapsed

    return wrapper
