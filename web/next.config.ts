import type { NextConfig } from "next";

import { controlPlaneRewrites } from "./src/lib/config/rewrites";

const nextConfig: NextConfig = {
  async rewrites() {
    return controlPlaneRewrites(process.env.CONTROLPLANE_API_BASE_URL);
  },
};

export default nextConfig;
