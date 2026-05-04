# 13-ts-workflows

API server accepting **TypeScript workflows** via `providers/ts`.

Workflows are bundled by esbuild, validated against an allowlist
(`@mkfst/sdk` + `mkfst-stack`), executed in a sandboxed
QuickJS-NG-via-wazero runtime, and run as DAGs against the same
`workflows.Engine` mkfst exposes natively.

Each submitted workflow is **bound to a docker stack server-side**;
the bridge enforces that workflow tasks can only `exec` / `runOneShot`
into containers within that stack.

## Prerequisites

A reachable docker daemon. Common setup:

```sh
export DOCKER_HOST=unix:///run/user/$(id -u)/docker.sock
```

## Run

```sh
go run ./examples/13-ts-workflows
```

The server brings up a small `demo` stack with one alpine service,
then exposes `/workflows` HTTP endpoints.

## Submit + run a TS workflow

The included `example.workflow.ts` is a 2-node DAG that exec's into
the stack's `svc` container.

```sh
# Submit. Server bundles, allowlist-validates, registers.
curl -s -X POST --data-binary @examples/13-ts-workflows/example.workflow.ts \
  -H 'Content-Type: application/typescript' \
  'http://localhost:8081/workflows?name=demo' | jq

# Trigger an instance.
curl -s -X POST http://localhost:8081/workflows/demo/run | jq
# {"Instance":"7e2b…"}

# Poll for completion.
curl -s http://localhost:8081/workflows/instances/7e2b… | jq
```

## What this demonstrates

- Server-side TS bundling via `providers/ts/bundle` + esbuild +
  allowlist enforcement (try `import lodash from "lodash"` — rejected).
- AST validation: `eval`, `Function`, dynamic `import`, `__proto__`,
  `with`, etc. — all rejected at submit.
- Workflow→stack scoping: every workflow is bound to `demo`. A TS
  workflow that tried to forge a `stack` arg pointing at a different
  stack would be rejected by the bridge dispatcher (see
  `tests/e2e/ts_workflow_test.go::TestTSWorkflow_CrossStackDenied`).
- Full workflow integration: TS-defined tasks register against the
  same `workflows.Engine` Go-defined tasks would use.
- Per-module capability scoping: `mkfst-stack` declares
  `stack.runOneShot` + `stack.exec`; the bridge checks every call
  against those declarations.

## Try a malicious workflow

The bundler rejects each of these at submit time:

```sh
# eval — rejected.
echo 'export default eval("1+1")' | curl -s -X POST --data-binary @- \
  -H 'Content-Type: application/typescript' \
  'http://localhost:8081/workflows?name=evil1' | jq

# Unapproved module — rejected.
cat <<EOF | curl -s -X POST --data-binary @- \
  -H 'Content-Type: application/typescript' \
  'http://localhost:8081/workflows?name=evil2' | jq
import _ from "lodash";
export default _;
EOF

# Direct @mkfst/host import (private SDK) — rejected.
cat <<EOF | curl -s -X POST --data-binary @- \
  -H 'Content-Type: application/typescript' \
  'http://localhost:8081/workflows?name=evil3' | jq
import { stack } from "@mkfst/host";
export default stack;
EOF
```

Each returns a 422 with a structured error message.

## See also

- [`docs/TYPESCRIPT_TASKS.md`](../../docs/TYPESCRIPT_TASKS.md) — the
  full architecture reference.
- `tests/e2e/ts_workflow_test.go` — comprehensive e2e coverage.
