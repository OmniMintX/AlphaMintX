// Startup build-id log (deploy-and-survive.md DS-12): the standalone
// server names its build at boot so the RUNBOOK §10 upgrade procedure can
// verify the web tier alongside `controlplane --version`.
export async function register() {
  if (process.env.NEXT_RUNTIME !== "nodejs") return;
  // webpackIgnore: middleware.ts adds an edge compile of this file, and
  // webpack must not try to bundle node builtins there — the runtime guard
  // above means these imports only ever execute under node.
  const { readFileSync } = await import(/* webpackIgnore: true */ "node:fs");
  const { join } = await import(/* webpackIgnore: true */ "node:path");
  let buildID = "dev";
  try {
    buildID = readFileSync(join(process.cwd(), ".next", "BUILD_ID"), "utf8").trim();
  } catch {
    // `next dev` has no BUILD_ID; "dev" is accurate there.
  }
  console.log(`alphamintx-web starting build=${buildID}`);
}
