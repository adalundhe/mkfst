/**
 * @mkfst/host — PRIVATE bridge SDK.
 *
 * Importing this from a non-blessed module is a build-time error
 * enforced by the bundler. Blessed modules use this surface to
 * implement clean TS APIs over mkfst's host primitives. Each call
 * is dispatched through __mkfst_dispatch to the Go bridge, which
 * applies capability enforcement before invoking the underlying
 * primitive.
 *
 * The dispatch protocol is JSON for v1 (msgpack later). Args and
 * results round-trip as plain objects; binary payloads use base64.
 */

declare const globalThis: {
  __mkfst_dispatch?: (op: string, argsJSON: string) => string;
  __mkfst_module_name?: string;
};

class HostError extends Error {
  readonly code: string;
  constructor(opts: { code: string; message: string }) {
    super(opts.message);
    this.name = "HostError";
    this.code = opts.code;
  }
}

function dispatch<R>(op: string, args: unknown): R {
  if (typeof globalThis.__mkfst_dispatch !== "function") {
    throw new HostError({ code: "NO_BRIDGE", message: "host bridge not installed" });
  }
  const raw = globalThis.__mkfst_dispatch(op, JSON.stringify(args ?? {}));
  if (raw === "" || raw === undefined || raw === null) {
    return undefined as unknown as R;
  }
  const parsed = JSON.parse(raw);
  if (parsed && typeof parsed === "object" && "__error" in parsed) {
    const e = parsed as { __error: { code: string; message: string } };
    throw new HostError(e.__error);
  }
  return parsed as R;
}

// === Stack handle ===

export interface OneShotOpts {
  image: string;
  cmd?: string[];
  entrypoint?: string[];
  env?: Record<string, string>;
  workDir?: string;
  user?: string;
  stdin?: string;       // optional stdin payload
  timeoutSec?: number;
  aliases?: string[];
  pullIfMissing?: boolean;
}

export interface OneShotResult {
  containerId: string;
  exitCode: number;
  stdout: string;
  stderr: string;
  durationMs: number;
}

export interface ExecOpts {
  cmd: string[];
  env?: Record<string, string>;
  user?: string;
  workDir?: string;
  stdin?: string;
  timeoutSec?: number;
}

export interface ExecResult {
  exitCode: number;
  stdout: string;
  stderr: string;
  durationMs: number;
}

export interface StackHandle {
  runOneShot(opts: OneShotOpts): OneShotResult;
  exec(service: string, replica: number, opts: ExecOpts): ExecResult;
  address(service: string): string;
  waitHealthy(service: string, timeoutSec: number): boolean;
}

export function stack(name: string): StackHandle {
  return {
    runOneShot: (opts) => dispatch<OneShotResult>("stack.runOneShot", { stack: name, ...opts }),
    exec: (service, replica, opts) => dispatch<ExecResult>("stack.exec", { stack: name, service, replica, ...opts }),
    address: (service) => dispatch<string>("stack.address", { stack: name, service }),
    waitHealthy: (service, timeoutSec) => dispatch<boolean>("stack.waitHealthy", { stack: name, service, timeoutSec }),
  };
}

// === Logging ===

export const log = {
  debug: (msg: string, fields?: Record<string, unknown>): void => {
    dispatch<void>("log", { level: "debug", msg, fields });
  },
  info: (msg: string, fields?: Record<string, unknown>): void => {
    dispatch<void>("log", { level: "info", msg, fields });
  },
  warn: (msg: string, fields?: Record<string, unknown>): void => {
    dispatch<void>("log", { level: "warn", msg, fields });
  },
  error: (msg: string, fields?: Record<string, unknown>): void => {
    dispatch<void>("log", { level: "error", msg, fields });
  },
};

// === host (root) ===

export const host = {
  stack,
  log,
};

export { HostError };
