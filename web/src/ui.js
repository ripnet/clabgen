import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { createContext, useCallback, useContext, useEffect, useState } from "react";
import { errorText } from "./api";
const ToastCtx = createContext(() => { });
export function ToastProvider({ children }) {
    const [toasts, setToasts] = useState([]);
    const push = useCallback((kind, text) => {
        const id = Date.now() + Math.random();
        setToasts((t) => [...t.slice(-4), { id, kind, text }]);
        setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), kind === "error" ? 8000 : 3000);
    }, []);
    return (_jsxs(ToastCtx.Provider, { value: push, children: [children, _jsx("div", { className: "toasts", role: "status", "aria-live": "polite", children: toasts.map((t) => (_jsx("div", { className: `toast toast-${t.kind}`, onClick: () => setToasts((x) => x.filter((y) => y.id !== t.id)), children: t.text }, t.id))) })] }));
}
export const useToast = () => useContext(ToastCtx);
// ---- small components ----
export function Dot({ health, title }) {
    return _jsx("span", { className: `dot ${health}`, title: title });
}
export function HealthText({ health, children }) {
    return (_jsxs("span", { className: "health", children: [_jsx(Dot, { health: health }), _jsx("span", { children: children })] }));
}
export function Modal({ title, onClose, children, wide }) {
    useEffect(() => {
        const f = (e) => e.key === "Escape" && onClose();
        window.addEventListener("keydown", f);
        return () => window.removeEventListener("keydown", f);
    }, [onClose]);
    return (_jsx("div", { className: "modal-back", onMouseDown: (e) => e.target === e.currentTarget && onClose(), children: _jsxs("div", { className: `modal ${wide === "x" ? "wide xwide" : wide ? "wide" : ""}`, role: "dialog", "aria-modal": "true", "aria-label": title, children: [_jsxs("div", { className: "modal-head", children: [_jsx("h3", { children: title }), _jsx("button", { className: "icon-btn", onClick: onClose, "aria-label": "Close", children: "\u00D7" })] }), _jsx("div", { className: "modal-body", children: children })] }) }));
}
export function Empty({ title, children }) {
    return (_jsxs("div", { className: "empty", children: [_jsx("div", { className: "empty-title", children: title }), children && _jsx("div", { className: "empty-body", children: children })] }));
}
export function ErrorBox({ error }) {
    if (!error)
        return null;
    return _jsx("div", { className: "error-box", children: error });
}
// useBusy wraps an async action with a busy flag and toast on error.
export function useBusy() {
    const toast = useToast();
    const [busy, setBusy] = useState(false);
    const run = useCallback(async (fn, ok) => {
        setBusy(true);
        try {
            const r = await fn();
            if (ok)
                toast("ok", ok);
            return r;
        }
        catch (e) {
            toast("error", errorText(e));
            return undefined;
        }
        finally {
            setBusy(false);
        }
    }, [toast]);
    return { busy, run };
}
export function useTheme() {
    const [theme, setTheme] = useState(() => {
        try {
            return localStorage.getItem("clabgen.theme") || "auto";
        }
        catch {
            return "auto";
        }
    });
    useEffect(() => {
        const root = document.documentElement;
        if (theme === "auto")
            root.removeAttribute("data-theme");
        else
            root.setAttribute("data-theme", theme);
        try {
            localStorage.setItem("clabgen.theme", theme);
        }
        catch {
            /* storage unavailable */
        }
    }, [theme]);
    return [theme, setTheme];
}
// copyText puts text on the clipboard. The Clipboard API exists only on
// secure pages (HTTPS or localhost); a UI opened as http://host:8090 falls
// back to a hidden textarea and execCommand, which still works there.
export async function copyText(text) {
    if (window.isSecureContext && navigator.clipboard) {
        try {
            await navigator.clipboard.writeText(text);
            return true;
        }
        catch {
            // fall through
        }
    }
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try {
        ok = document.execCommand("copy");
    }
    catch {
        ok = false;
    }
    ta.remove();
    return ok;
}
