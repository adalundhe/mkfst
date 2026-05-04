// example.workflow.ts — submit this to the 13-ts-workflows server.
//
//   curl -X POST --data-binary @example.workflow.ts \
//     -H 'Content-Type: application/typescript' \
//     'http://localhost:8081/workflows?name=demo'
//
// The submitted workflow is bound to the "demo" stack server-side.
// Calls to exec() / runOneShot() target containers in that stack;
// cross-stack reach would be denied automatically.

import { defineTask, defineDAG } from "@mkfst/sdk";
import { exec } from "mkfst-stack";

const probe = defineTask({
  name: "probe",
  run: () => {
    const r = exec("svc", 0, {
      cmd: ["sh", "-c", "echo probed-from-ts"],
      timeoutSec: 10,
    });
    return r.stdout.trim();
  },
});

const summarise = defineTask({
  name: "summarise",
  parents: { probe },
  run: ({ parents }) => {
    return `seen: ${parents.probe}`;
  },
});

export default defineDAG("demo", (b) => {
  const p = b.add(probe);
  b.add(summarise, { dependsOn: { probe: p } });
});
