import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const secy = {
  sci: "0200000a02010001",
  cipher: "gcm-aes-128",
  encoding_sa: 0,
  tx_sas: [{ an: 0, next_pn: 1201, created: "2026-11-09T12:00:00Z", out_pkts_encrypted: 1200 }],
  rx_scs: [
    {
      sci: "0200000b02010001",
      mac: "02:00:00:0b:02:01",
      port: 1,
      sas: [{ an: 0, next_pn: 1190, lowest_pn: 1190, created: "2026-11-09T12:00:00Z" }],
      in_pkts_ok: 1189,
      in_pkts_late: 2,
    },
  ],
  out_pkts_encrypted: 1200,
  out_pkts_protected: 0,
};

const mka = {
  state: "secured",
  mi: "aabbccddeeff00112233",
  mn: 42,
  priority: 10,
  key_server: true,
  key_server_sci: "0200000a02010001",
  cipher_suite: "gcm-aes-128",
  latest_key: { kn: 3, an: 2, key_server_mi: "aabb", tx: true, rx: true, created: "2026-11-09T12:00:00Z" },
  peers: [{ sci: "0200000b02010001", mi: "1122", mn: 40, live: true, priority: 16, key_server: false, receiving_latest: true, sending_latest: true, expires_in_s: 5.2 }],
  tx_mkpdus: 42,
  rx_mkpdus: 40,
  rekeys: 2,
};

const instance = (status: Record<string, unknown>, config: Record<string, unknown> = {}): Protocol =>
  ({
    name: "evpl-macsec",
    type: "macsec",
    device: "d1",
    state: "running",
    config: { scope: "device", cipher: "gcm-aes-128", encrypt: true, confidentiality_offset: 0, include_sci: true, port_id: 1, validate: "strict", replay_protect: true, replay_window: 0, policy: "must-secure", ...config },
    status,
  }) as unknown as Protocol;

test("a MACsec instance shows its frame, the MKA session and what it received", async ({ page }) => {
  const st = { scope: "device", clear_tags: 1, clear_vlans: [100], encrypted_vlans: [], overhead: 32, encrypt: true, confidentiality_offset: 0, keys: "mka", secured: true, policy: "must-secure", mka, ...secy };
  await mockAgent(page, instance(st, { mka: { cak: "ffeeddccbbaa99887766554433221100", ckn: "657670", key_server_priority: 10 } }), () => undefined);
  await page.goto("/#/a/a1/p/evpl-macsec");
  const frame = page.locator(".ms-frame");
  await expect(frame).toContainText("VLAN 100");
  await expect(frame).toContainText("SecTAG 16 B");
  await expect(frame.locator(".ms-enc")).toContainText("EtherType · data");
  await expect(page.getByText("+32 bytes per frame")).toBeVisible();
  await expect(page.getByRole("heading", { name: /MKA/ })).toContainText("secured");
  await expect(page.getByText("Key server, priority 10")).toBeVisible();
  const peer = page.getByRole("row", { name: /0200000b02010001/ }).first();
  await expect(peer).toContainText("live");
  await expect(peer).toContainText("rx+tx");
  await expect(page.getByRole("row", { name: /02:00:00:0b:02:01/ })).toContainText("in pkts late 2");
  await expect(page.locator(".proto-state")).toContainText("secured, 1 peer, key server, SAK 3 AN 2 · VLAN 100");
});

test("static keys at port scope: no MKA, tags encrypted, integrity only", async ({ page }) => {
  const st = { scope: "port", clear_tags: 0, clear_vlans: [], encrypted_vlans: [], overhead: 24, encrypt: false, confidentiality_offset: 0, keys: "static", secured: true, policy: "should-secure", ...secy };
  await mockAgent(page, instance(st, { scope: "port", encrypt: false, include_sci: false, static: { key: "000102030405060708090a0b0c0d0e0f" } }), () => undefined);
  await page.goto("/#/a/a1/p/evpl-macsec");
  await expect(page.locator(".ms-frame .ms-auth")).toContainText("each device's VLAN tags");
  await expect(page.getByText("integrity protected, readable")).toBeVisible();
  await expect(page.getByText("whole port (one secure channel)")).toBeVisible();
  await expect(page.getByRole("heading", { name: /^MKA/ })).toHaveCount(0);
  await expect(page.locator(".proto-state")).toContainText("static key · port");
});
