// Custom addition (fork lasse200188/multica, see CUSTOM.md): client side of the
// personal Telegram notifications (server/cmd/server/custom_telegram_routes.go).
// Account-scoped: the link belongs to the user and covers all workspaces.

import { queryOptions } from "@tanstack/react-query";
import { z } from "zod";
import { api } from "../api";
import { parseWithFallback } from "../api/schema";

const CustomTelegramStatusSchema = z.object({
  configured: z.boolean().catch(false),
  bot_username: z.string().catch(""),
  connected: z.boolean().catch(false),
  telegram_username: z.string().catch(""),
  notify_mentions: z.boolean().catch(true),
  notify_assignments: z.boolean().catch(true),
  linked_at: z.string().nullable().catch(null),
});
export type CustomTelegramStatus = z.infer<typeof CustomTelegramStatusSchema>;

const EMPTY_STATUS: CustomTelegramStatus = {
  configured: false,
  bot_username: "",
  connected: false,
  telegram_username: "",
  notify_mentions: true,
  notify_assignments: true,
  linked_at: null,
};

const CustomTelegramLinkSchema = z.object({
  url: z.string(),
  expires_at: z.string(),
});
export type CustomTelegramLink = z.infer<typeof CustomTelegramLinkSchema>;

export type CustomTelegramSettings = Partial<
  Pick<CustomTelegramStatus, "notify_mentions" | "notify_assignments">
>;

// ApiClient has no public generic request method, and adding one would touch
// a large upstream file. Its `fetch<T>` is only TypeScript-private, so reach
// it through a narrow cast; it still carries auth, CSRF and error handling.
type Requester = { fetch<T>(path: string, init?: RequestInit): Promise<T> };
const request = <T>(path: string, init?: RequestInit) =>
  (api as unknown as Requester).fetch<T>(path, init);

const PATH = "/api/me/custom/telegram";

export const customTelegramKeys = {
  status: () => ["custom-telegram", "status"] as const,
};

export async function getCustomTelegramStatus(): Promise<CustomTelegramStatus> {
  const raw = await request<unknown>(PATH);
  return parseWithFallback(raw, CustomTelegramStatusSchema, EMPTY_STATUS, { endpoint: `GET ${PATH}` });
}

export async function updateCustomTelegramSettings(
  settings: CustomTelegramSettings,
): Promise<CustomTelegramStatus> {
  const raw = await request<unknown>(PATH, { method: "PATCH", body: JSON.stringify(settings) });
  return parseWithFallback(raw, CustomTelegramStatusSchema, EMPTY_STATUS, { endpoint: `PATCH ${PATH}` });
}

export async function createCustomTelegramLink(): Promise<CustomTelegramLink> {
  const raw = await request<unknown>(`${PATH}/link`, { method: "POST" });
  const parsed = CustomTelegramLinkSchema.safeParse(raw);
  if (!parsed.success) throw new Error("Unexpected response from the server");
  return parsed.data;
}

export async function disconnectCustomTelegram(): Promise<void> {
  await request<void>(PATH, { method: "DELETE" });
}

export async function sendCustomTelegramTest(): Promise<void> {
  await request<void>(`${PATH}/test`, { method: "POST" });
}

export const customTelegramStatusOptions = (opts?: { poll?: boolean }) =>
  queryOptions({
    queryKey: customTelegramKeys.status(),
    queryFn: getCustomTelegramStatus,
    refetchInterval: opts?.poll ? 2000 : false,
  });
