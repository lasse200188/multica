// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from "vitest";

const fetchMock = vi.fn();
vi.mock("../api", () => ({ api: { fetch: (...args: unknown[]) => fetchMock(...args) } }));

import { createCustomTelegramLink, getCustomTelegramStatus } from "./index";

describe("custom telegram client", () => {
  beforeEach(() => fetchMock.mockReset());

  it("parses a connected status", async () => {
    fetchMock.mockResolvedValue({
      configured: true,
      bot_username: "multica_bot",
      connected: true,
      telegram_username: "jens",
      notify_mentions: false,
      notify_assignments: true,
      linked_at: "2026-09-29T10:00:00Z",
    });
    const status = await getCustomTelegramStatus();
    expect(fetchMock).toHaveBeenCalledWith("/api/me/custom/telegram", undefined);
    expect(status).toMatchObject({ connected: true, telegram_username: "jens", notify_mentions: false });
  });

  it("falls back per field on a malformed status", async () => {
    fetchMock.mockResolvedValue({ configured: "yes", connected: true, notify_mentions: null });
    const status = await getCustomTelegramStatus();
    expect(status).toEqual({
      configured: false,
      bot_username: "",
      connected: true,
      telegram_username: "",
      notify_mentions: true,
      notify_assignments: true,
      linked_at: null,
    });
  });

  it("rejects a link response without a url", async () => {
    fetchMock.mockResolvedValue({ expires_at: "2026-09-29T10:15:00Z" });
    await expect(createCustomTelegramLink()).rejects.toThrow();
  });
});
