import { EventEmitter } from "node:events";
import type { HarnessEvent } from "./types";

type Handler = (event: HarnessEvent) => void;

export class EventBus extends EventEmitter {
  private readonly seen: Set<string> = new Set();

  subscribe(topic: string, handler: Handler): void {
    const guarded = this.guard(handler);
    this.on(topic, guarded);
  }

  publish(topic: string, event: HarnessEvent): void {
    this.seen.add(event.id);
    this.emit(topic, event);
  }

  drain(): number {
    const count = this.seen.size;
    this.seen.clear();
    return count;
  }

  guard(handler: Handler): Handler {
    const seen = this.seen;
    return (event: HarnessEvent): void => {
      if (seen.has(event.id)) {
        return;
      }
      seen.add(event.id);
      handler(event);
    };
  }
}

export function replay(events: HarnessEvent[], fromSeq: number): HarnessEvent[] {
  const ordered = events.slice().sort((left, right) => left.seq - right.seq);
  return ordered.filter((event) => event.seq > fromSeq);
}

export const summarize = (events: HarnessEvent[]): string => {
  const types = new Map<string, number>();
  for (const event of events) {
    types.set(event.type, (types.get(event.type) ?? 0) + 1);
  }
  return Array.from(types.entries()).join(";");
};
