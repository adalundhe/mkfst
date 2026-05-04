# 10-tasks

API server with background-job processing and a recurring schedule via
`providers/tasks`.

## Run

```sh
go run ./examples/10-tasks
```

Exercise:

```sh
# Enqueue an email.
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"to":"alice@example.com","subject":"hi"}' \
  http://localhost:8081/jobs/email
# {"job_id":"01HG..."}

# Look up status.
curl -s http://localhost:8081/jobs/01HG...

# Live stats: emails_sent + heart_beats + worker stats.
curl -s http://localhost:8081/stats | jq
```

## What this demonstrates

- An in-memory `tasks.Store` + `tasks.Worker` + `tasks.Scheduler`
  inside the API process.
- Two registered task types: `email` (caller-triggered) and
  `heartbeat` (scheduled every 5s).
- Handler enqueues + returns 202 immediately; the worker handles the
  actual send asynchronously.
- `UniqueKey` dedup means re-submitting the same `{to,subject}`
  within the dedup window collapses to one job.
- Status endpoint reflects retries, attempts, and last error.

Swap `tasks.NewMemoryStore` for `tasks.NewRedisStore` or
`tasks.NewSQLStore` to make jobs survive restarts and span multiple
processes — the rest of the code is unchanged.
