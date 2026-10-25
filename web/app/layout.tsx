import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "acme-card simulator",
  description: "Drive the secured-card flows against the local gateway.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
