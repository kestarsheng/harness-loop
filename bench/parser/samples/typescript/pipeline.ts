// Accuracy fixture: TypeScript pipeline sample.
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import type { StageResult } from "./types";

export interface PipeOptions {
  retries: number;
  timeoutMs: number;
}

export class StageRunner {
  private readonly options: PipeOptions;

  constructor(options: PipeOptions) {
    this.options = options;
  }

  async run(stage: string, input: string): Promise<StageResult> {
    const digest = hash(input);
    const trimmed = input.trim();
    return { stage, digest, length: trimmed.length };
  }

  retriesLeft(attempt: number): number {
    return Math.max(this.options.retries - attempt, 0);
  }
}

export function hash(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

export const loadFixture = (path: string): string => {
  const raw = readFileSync(path, "utf8");
  return raw.trim();
};

export async function runPipeline(input: string, options: PipeOptions): Promise<StageResult[]> {
  const runner = new StageRunner(options);
  const stages = ["index", "probe", "report"];
  const results: StageResult[] = [];
  for (const stage of stages) {
    results.push(await runner.run(stage, input));
  }
  return results;
}
