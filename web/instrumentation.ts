// Startup build-id log (deploy-and-survive.md DS-12): the standalone
// server names its build at boot so the RUNBOOK §10 upgrade procedure can
// verify the web tier alongside `controlplane --version`.
export async function register() {
  if (process.env.NEXT_RUNTIME !== "nodejs") return;
  const { readFileSync } = await import("node:fs");
  const { join } = await import("node:path");
  let buildID = "dev";
  try {
    buildID = readFileSync(join(process.cwd(), ".next", "BUILD_ID"), "utf8").trim();
  } catch {
    // `next dev` has no BUILD_ID; "dev" is accurate there.
  }
  console.log(`alphamintx-web starting build=${buildID}`);
}
