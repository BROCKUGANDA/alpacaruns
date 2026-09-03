/** @type {import('next').NextConfig} */
const nextConfig = {
  // Static export so the build output is a plain `out/` directory
  // that can be embedded into the Go binary at api/ui/. Combined
  // with the Go `//go:embed all:ui` directive in api/ui.go, this
  // gives a single-binary deploy with one port and no Node process
  // to run in production.
  output: "export",
  reactStrictMode: true,
  // Allow images from anywhere by default; the showcase only embeds
  // local SVGs (the alpaca logo) so this is harmless.
  images: {
    unoptimized: true,
  },
  // The Go embedded static server resolves all routes through
  // http.FileServer's directory → index.html rewrite. Trailing
  // slashes are required for that to work, so don't strip them.
  trailingSlash: true,
};

export default nextConfig;