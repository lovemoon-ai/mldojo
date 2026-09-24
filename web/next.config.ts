import type { NextConfig } from "next";

// Static export: mldojo-api serves out/ and falls back from /path to /path.html,
// so every page is a plain route (no dynamic segments) that reads query params.
const nextConfig: NextConfig = {
  output: "export",
  images: { unoptimized: true },
};

export default nextConfig;
