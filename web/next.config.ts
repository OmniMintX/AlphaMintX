import type { NextConfig } from "next";

// Baseline security headers on every route. Content-Security-Policy is
// deliberately ABSENT: the App Router's inline flight scripts would need
// nonces, which forces dynamic rendering — rejected after review. HSTS is
// production-only (build-time check) so local http dev is never pinned.
const securityHeaders = [
  { key: "X-Content-Type-Options", value: "nosniff" },
  { key: "X-Frame-Options", value: "DENY" },
  { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
  { key: "Permissions-Policy", value: "camera=(), microphone=(), geolocation=()" },
];
if (process.env.NODE_ENV === "production") {
  securityHeaders.push({
    key: "Strict-Transport-Security",
    value: "max-age=31536000; includeSubDomains",
  });
}

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
  async headers() {
    return [{ source: "/:path*", headers: securityHeaders }];
  },
};

export default nextConfig;
