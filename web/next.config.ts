import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Self-contained production server for the systemd deployment
  // (deploy-and-survive.md DS-11c). The tracing root is pinned so a stray
  // lockfile above the checkout can never nest server.js and silently
  // break the unit's ExecStart path.
  output: "standalone",
  outputFileTracingRoot: __dirname,
  // The web tier holds no state: image optimization is the standalone
  // server's only runtime writer (.next/cache), and the unit runs under
  // ProtectSystem=strict with no writable paths.
  images: { unoptimized: true },
};

export default nextConfig;
