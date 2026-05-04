# 11-workflows

API server submitting DAG workflows via `providers/workflows`.

## Run

```sh
go run ./examples/11-workflows
```

```sh
# Trigger an instance of the "etl" workflow.
curl -s -X POST http://localhost:8081/workflows/etl/run
# {"Instance":"7e2b…"}

# Inspect status — repeat until state=completed.
curl -s http://localhost:8081/workflows/instances/7e2b… | jq

# Cancel.
curl -s -X DELETE http://localhost:8081/workflows/instances/7e2b…
```

## What this demonstrates

- A static three-node DAG: `extract → transform → load`.
- Per-node handlers receive `parents map[string][]byte`; the
  upstream output flows directly as the downstream input.
- One-line submit + inspect via the existing fizz handler signature.
- Failure policy: this demo uses default `FailHaltWorkflow` — a
  failed extract halts the whole instance.

To make the example more interesting:
- Add `OnFail(workflows.FailContinue)` to a node and watch downstream
  proceed with empty parent input.
- Replace `Outputs:` with a Redis or SQL `Cache` so multiple
  processes can run the same workflow definition.
- Add a TS-authored workflow (see `13-ts-workflows`) that runs on
  top of the same engine.
