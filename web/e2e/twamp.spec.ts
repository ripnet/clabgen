import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const session = (over: Record<string, unknown>) => ({
  sender: "10.0.0.1:49154",
  local: "10.0.0.2:862",
  ssid: 0,
  kind: "twamp-light",
  timestamps: "ntp",
  state: "active",
  first: "2026-11-17T16:02:32Z",
  last: "2026-11-17T16:02:52Z",
  last_ago_s: 0.4,
  rx: 120,
  tx: 116,
  dropped: 4,
  gaps: 0,
  reordered: 0,
  duplicates: 0,
  last_seq: 119,
  ttl: 255,
  dscp: 0,
  dwell_us: 152,
  ...over,
});

const instance = (status: Record<string, unknown>, config: Record<string, unknown> = {}): Protocol =>
  ({ name: "refl", type: "twamp-reflector", device: "gen", state: "running", config: { mode: "stamp", ports: [862], ttl: 255, ...config }, status }) as unknown as Protocol;

const base = { mode: "stamp", ports: [862], muted: false, rx: 300, tx: 290, dropped: 10, refused: 0, errors: 0, pending: 0 };

test("a reflector lists the router's sessions: kind, drops, sender gaps, TLVs", async ({ page }) => {
  const st = {
    ...base,
    active: 2,
    faults: ["delay 5000 µs"],
    sessions: [
      session({ burst_remaining: 3 }),
      session({ sender: "10.0.0.1:49155", ssid: 3, kind: "stamp", ttl: 1, dscp: 48, gaps: 2, tlvs: { "extra-padding": 60 }, tlv_length_with_header: true }),
      session({ sender: "[2001:db8::1]:49153", local: "[2001:db8::2]:862", ssid: 1, kind: "stamp", state: "idle", last_ago_s: 42 }),
    ],
  };
  await mockAgent(page, instance(st, { delay_us: 5000 }), () => undefined);
  await page.goto("/#/a/a1/p/refl");
  await expect(page.locator(".proto-state")).toContainText("2 sessions from 10.0.0.1 · delay 5000 µs");
  await expect(page.getByText("STAMP · 862", { exact: true })).toBeVisible();
  const tl = page.getByRole("row", { name: /10\.0\.0\.1:49154/ });
  await expect(tl).toContainText("TWAMP Light");
  await expect(tl).toContainText("3 more to drop");
  const lm = page.getByRole("row", { name: /10\.0\.0\.1:49155/ });
  await expect(lm).toContainText("STAMP SSID 3");
  await expect(lm).toContainText("1 · 48");
  await expect(lm).toContainText("extra-padding");
  await expect(lm).toContainText("length counts the TLV header");
  const idle = page.getByRole("row", { name: /2001:db8::1/ });
  await expect(idle).toHaveClass(/stale/);
  await expect(idle).toContainText("idle, 42.0 s ago");
});

test("a muted reflector says so; one with no sessions is idle", async ({ page }) => {
  await mockAgent(page, instance({ ...base, muted: true, active: 1, sessions: [session({})] }), () => undefined);
  await page.goto("/#/a/a1/p/refl");
  await expect(page.locator(".proto-state")).toContainText("muted · 1 session");
  await expect(page.locator(".kv").getByText("muted", { exact: true })).toBeVisible();

  await mockAgent(page, instance({ ...base, rx: 0, tx: 0, dropped: 0, active: 0, sessions: [] }), () => undefined);
  await page.goto("/#/a/a1/p/refl");
  await page.reload();
  await expect(page.locator(".proto-state")).toContainText("no sessions · port 862");
  await expect(page.getByText("No test packets received yet.")).toBeVisible();
});
