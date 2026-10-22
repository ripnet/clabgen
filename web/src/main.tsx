import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { AgentsProvider } from "./agents";
import { ToastProvider } from "./ui";
import "./styles.css";
// Protocol views register themselves.
import "./protocols/cfm";
import "./protocols/icmp";
import "./protocols/udp";
import "./protocols/isis";
import "./protocols/lldp";
import "./protocols/bgp";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ToastProvider>
      <AgentsProvider>
        <App />
      </AgentsProvider>
    </ToastProvider>
  </StrictMode>,
);
