/**
 * mkfst-stack — clean TS API over stack primitives, scoped to the
 * workflow's bound stack.
 *
 * The workflow author does NOT pass a stack name. Calls are
 * automatically scoped to the stack the workflow was submitted
 * with. Cross-stack reach is denied at the bridge regardless of
 * what the script asks for.
 *
 * Direct imports:
 *   import { runOneShot, exec, address, waitHealthy } from "mkfst-stack";
 *
 * Or grouped:
 *   import { stack } from "mkfst-stack";
 *   await stack.runOneShot({ image: "alpine", cmd: ["true"] });
 */

import { stack as hostStack, log as hostLog } from "@mkfst/host";
import type { OneShotOpts, OneShotResult, ExecOpts, ExecResult, StackHandle } from "@mkfst/host";

export type { OneShotOpts, OneShotResult, ExecOpts, ExecResult, StackHandle };

// The bridge reads the workflow's bound stack name and enforces
// scope; passing the empty string here lets the dispatcher inject
// the bound stack. Workflow authors don't need to know its name.
const handle: StackHandle = hostStack("");

export const runOneShot = (opts: OneShotOpts): OneShotResult => handle.runOneShot(opts);
export const exec = (service: string, replica: number, opts: ExecOpts): ExecResult => handle.exec(service, replica, opts);
export const address = (service: string): string => handle.address(service);
export const waitHealthy = (service: string, timeoutSec: number): boolean => handle.waitHealthy(service, timeoutSec);

export const stack = { runOneShot, exec, address, waitHealthy };

export const log = hostLog;
