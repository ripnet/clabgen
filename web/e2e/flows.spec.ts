import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const flow = (name: string, verdict: Record<string, unknown>, state = "running"): Protocol =>
  ({
    name,
    type: "icmp",
    device: "d1",
    state: "running",
    config: { destination: "10.0.0.2" },
    status: {
      state,
      destination: "10.0.0.2",
      source: "10.0.0.1",
      kind: "echo",
      rate: 100,
      tracks_replies: true,
      tx: 1000,
      rx: 990,
      lost: 10,
      loss_pct: 1,
      outage_max_ms: 120,
      outage_current_ms: 0,
      rtt: { min_us: 900, avg_us: 1500, max_us: 4000, jitter_us: 100, last_us: 1400 },
      recent: [],
      timeline: [],
      verdict,
    },
  }) as unknown as Protocol;

test("lab-wide flows with checks", async ({ page }) => {
  const pass = flow("ping-ok", { result: "pass", checks: [{ check: "loss_pct", limit: 2, value: 1, pass: true }] });
  const fail = flow("ping-bad", {
    result: "fail",
    failed: "outage_ms 120 > 50",
    checks: [
      { check: "loss_pct", limit: 2, value: 1, pass: true },
      { check: "outage_ms", limit: 50, value: 120, pass: false },
    ],
  }, "stopped");
  await mockAgent(page, fail, () => undefined, { more: [pass] });
  const started: string[] = [];
  page.on("request", (r) => {
    if (r.url().endsWith("/RunProtocolAction")) started.push((r.postDataJSON() as { name: string; action: string }).name);
  });

  await page.goto("./#/");
  await page.locator(".sidebar").getByRole("link", { name: "Flows" }).click();
  await expect(page.getByRole("heading", { name: "Flows" })).toBeVisible();
  const stat = (l: string) => page.locator(".stat", { hasText: l }).locator(".stat-n");
  await expect(stat("Flows")).toHaveText("2");
  await expect(stat("Pass")).toHaveText("1");
  await expect(stat("Fail")).toHaveText("1");
  const rows = page.locator("tbody tr");
  await expect(rows).toHaveCount(2);
  await expect(rows.filter({ hasText: "ping-bad" }).locator(".verdict")).toHaveText("fail");
  await expect(rows.filter({ hasText: "ping-bad" }).locator(".verdict")).toHaveAttribute("title", "failed: outage_ms 120 > 50");

  // Failing only, then Start all starts just what's listed (the stopped one).
  await page.getByLabel("failing only").check();
  await expect(rows).toHaveCount(1);
  await page.getByRole("button", { name: /Start all/ }).click();
  await expect.poll(() => started).toEqual(["ping-bad"]);

  // The flow page lists the checks.
  await rows.first().getByRole("link", { name: "ping-bad" }).click();
  const panel = page.locator(".verdict-panel");
  await expect(panel).toContainText("longest outage");
  await expect(panel.locator("tbody tr").nth(1)).toContainText("≤ 50 ms");
  await expect(panel.locator("tbody tr").nth(1)).toContainText("120 ms");
  await expect(panel.locator("tbody tr").nth(1).locator(".verdict")).toHaveText("fail");
});
