/**
 * @mkfst/sdk — public surface for end-user TypeScript workflows.
 *
 * What's here:
 *   - defineTask(opts) → TaskDefinition
 *   - defineDAG(name, build) → DAGDefinition
 *   - TaskError (structured errors that surface to the workflow engine)
 *   - log() helper (forwards to host log if available)
 *   - types: TaskCtx, TaskHandle, DAGBuilder, FailPolicy
 *
 * Pure types + a minimal runtime stub. End-user workflows import
 * from here and never see the host bridge.
 *
 * The bundle pipeline collects defineTask/defineDAG calls into a
 * single global `__mkfst_workflow` populated at module-eval time.
 * The Go side reads that global to discover registered tasks and
 * the DAG shape, then registers Go-side handlers that proxy back
 * into the JS task functions when nodes fire.
 */

export type FailPolicy = "haltWorkflow" | "skipDownstream" | "continue";

export interface TaskCtx<P = Record<string, unknown>> {
  parents: P;
  ctx: {
    signal: AbortSignal;
    deadline: number;       // unix ms
    instanceId: string;
    nodeName: string;
  };
  log: (level: "debug" | "info" | "warn" | "error", msg: string, fields?: Record<string, unknown>) => void;
}

export interface TaskRunOpts<P> {
  name: string;
  retries?: number;
  timeoutSec?: number;
  parents?: Record<string, TaskHandle>;
  run: (ctx: TaskCtx<P>) => Promise<unknown> | unknown;
}

export interface TaskDefinition<P = Record<string, unknown>> {
  readonly __id: number;
  readonly name: string;
  readonly retries?: number;
  readonly timeoutSec?: number;
  readonly parentNames: string[];
  readonly run: (ctx: TaskCtx<P>) => Promise<unknown> | unknown;
}

export interface TaskHandle {
  readonly __nodeId: number;
  readonly task: TaskDefinition;
}

export interface DAGBuilder {
  add<P>(task: TaskDefinition<P>, opts?: { dependsOn?: TaskHandle[] | Record<string, TaskHandle>; onFail?: FailPolicy; }): TaskHandle;
}

export interface DAGNode {
  __nodeId: number;
  taskName: string;
  taskId: number;
  parents: number[];        // node IDs
  parentNames: Record<string, number>; // alias → parent node ID for typed access
  onFail: FailPolicy;
}

export interface DAGDefinition {
  name: string;
  tasks: TaskDefinition[];
  nodes: DAGNode[];
}

/** Structured task error. */
export class TaskError extends Error {
  readonly code: string;
  readonly retryable: boolean;
  readonly details: unknown;
  constructor(opts: { code: string; message?: string; retryable?: boolean; details?: unknown }) {
    super(opts.message ?? opts.code);
    this.name = "TaskError";
    this.code = opts.code;
    this.retryable = opts.retryable ?? true;
    this.details = opts.details;
  }
}

// === internal registry ===

interface WorkflowGlobal {
  tasks: TaskDefinition[];
  dag: DAGDefinition | null;
}

declare const globalThis: {
  __mkfst_workflow?: WorkflowGlobal;
  __mkfst_log?: (level: string, msg: string, fields?: unknown) => void;
};

function reg(): WorkflowGlobal {
  if (!globalThis.__mkfst_workflow) {
    globalThis.__mkfst_workflow = { tasks: [], dag: null };
  }
  return globalThis.__mkfst_workflow!;
}

let nextTaskId = 0;
let nextNodeId = 0;

export function defineTask<P = Record<string, unknown>>(opts: TaskRunOpts<P>): TaskDefinition<P> {
  if (!opts.name) throw new TaskError({ code: "INVALID_TASK", message: "task name is required" });
  if (typeof opts.run !== "function") {
    throw new TaskError({ code: "INVALID_TASK", message: `task ${opts.name}: run must be a function` });
  }
  const parentNames = opts.parents ? Object.keys(opts.parents) : [];
  const td: TaskDefinition<P> = {
    __id: nextTaskId++,
    name: opts.name,
    retries: opts.retries,
    timeoutSec: opts.timeoutSec,
    parentNames,
    run: opts.run,
  };
  reg().tasks.push(td as unknown as TaskDefinition);
  return td;
}

export function defineDAG(
  name: string,
  build: (b: DAGBuilder) => void,
): DAGDefinition {
  if (!name) throw new TaskError({ code: "INVALID_DAG", message: "dag name is required" });
  const nodes: DAGNode[] = [];
  const builder: DAGBuilder = {
    add(task, opts) {
      const node: DAGNode = {
        __nodeId: nextNodeId++,
        taskName: task.name,
        taskId: task.__id,
        parents: [],
        parentNames: {},
        onFail: opts?.onFail ?? "haltWorkflow",
      };
      const dep = opts?.dependsOn;
      if (Array.isArray(dep)) {
        for (const h of dep) {
          node.parents.push(h.__nodeId);
        }
      } else if (dep && typeof dep === "object") {
        for (const [alias, h] of Object.entries(dep)) {
          node.parents.push(h.__nodeId);
          node.parentNames[alias] = h.__nodeId;
        }
      }
      nodes.push(node);
      return { __nodeId: node.__nodeId, task } as TaskHandle;
    },
  };
  build(builder);
  const dag: DAGDefinition = {
    name,
    tasks: reg().tasks,
    nodes,
  };
  reg().dag = dag;
  return dag;
}

export function log(level: "debug" | "info" | "warn" | "error", msg: string, fields?: Record<string, unknown>): void {
  if (typeof globalThis.__mkfst_log === "function") {
    globalThis.__mkfst_log(level, msg, fields);
  }
}
