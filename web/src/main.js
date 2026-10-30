import { jsx as _jsx } from "react/jsx-runtime";
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
import "./protocols/l2";
import "./protocols/packet";
import "./protocols/isis";
import "./protocols/lldp";
import "./protocols/bgp";
import "./protocols/ospf";
import "./protocols/dhcp";
createRoot(document.getElementById("root")).render(_jsx(StrictMode, { children: _jsx(ToastProvider, { children: _jsx(AgentsProvider, { children: _jsx(App, {}) }) }) }));
