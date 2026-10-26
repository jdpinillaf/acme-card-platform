import type { Metadata } from "next";
import "./portal.css";

export const metadata: Metadata = {
  title: "Mi tarjeta · acme-card",
  description: "Portal del tarjetahabiente para probar compras, devoluciones, pagos, congelamiento y cierre contra el gateway local.",
};

export default function PortalLayout({ children }: { children: React.ReactNode }) {
  return children;
}
