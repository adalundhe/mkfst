# Vendored from github.com/adhocore/gronx

This package is a verbatim vendor of the cron expression parser from
[adhocore/gronx](https://github.com/adhocore/gronx) v1.19.6, with the
package name renamed from `gronx` to `cron` so it's namespaced under
`mkfst/providers/tasks/cron`.

The original MIT license is preserved in `LICENSE.gronx` per the
license's redistribution requirement. Any local modifications below
this notice. Original author: Jitendra Adhikari (©2021–2099).

## Why vendor it

We need the parser surface (5/6/7-field cron, `@hourly`/`@5minutes`
descriptors, Quartz `L`/`W`/`#` modifiers) without committing to an
external dependency that may go unmaintained. Vendoring lets us:

- Patch parser bugs in-tree without waiting for upstream releases
- Keep the dependency surface of `mkfst/providers/tasks` minimal
- Audit the code as part of mkfst's own correctness story (no
  panics, all errors surfaced, no leaked goroutines)

## Local modifications

None yet. Subsequent edits will be tracked in commit history with
`tasks/cron:` prefixes.
