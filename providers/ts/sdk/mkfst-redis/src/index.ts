/**
 * mkfst-redis — minimal Redis client backed by stack.exec(redis-cli),
 * scoped to the workflow's bound stack.
 *
 * Each call shells out to redis-cli inside a named service's
 * container in the workflow's bound stack. Cross-stack reach is
 * denied at the bridge regardless of arguments.
 *
 * Usage:
 *   import { redis } from "mkfst-redis";
 *   await redis("cache").set("counter", "0");
 */

import { stack as hostStack } from "@mkfst/host";

export interface RedisHandle {
  set(key: string, value: string): string;
  get(key: string): string | null;
  del(key: string): number;
  ping(): "PONG";
}

const handle = hostStack(""); // bound by the bridge to the workflow's stack

/**
 * Bind to the redis service named `service` (default "redis") in
 * the workflow's stack. Optional replica index.
 */
export function redis(service: string = "redis", opts: { replica?: number } = {}): RedisHandle {
  const replica = opts.replica ?? 0;
  const exec = (cmd: string[]): string => {
    const r = handle.exec(service, replica, { cmd });
    if (r.exitCode !== 0) {
      throw new Error(`redis-cli ${cmd.join(" ")}: exit ${r.exitCode} stderr=${r.stderr}`);
    }
    return r.stdout.trim();
  };
  return {
    set(key, value) { return exec(["redis-cli", "SET", key, value]); },
    get(key) {
      const out = exec(["redis-cli", "GET", key]);
      return out === "" || out === "(nil)" ? null : out;
    },
    del(key) { return parseInt(exec(["redis-cli", "DEL", key]), 10); },
    ping() {
      const out = exec(["redis-cli", "PING"]);
      return out as "PONG";
    },
  };
}
