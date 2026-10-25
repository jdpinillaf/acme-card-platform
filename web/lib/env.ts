// Server-side configuration. Secrets never reach the browser.
export const env = {
  gatewayUrl: process.env.GATEWAY_URL ?? "http://localhost:8088",
  jwtSecret: process.env.JWT_HS256_SECRET ?? "",
  bankSecret: process.env.BANK_WEBHOOK_SECRET ?? "",
};
