"use client";

/* eslint-disable i18next/no-literal-string, no-restricted-syntax -- custom fork code: copy stays out of the locale files (see below) */
// Custom addition (fork lasse200188/multica, see CUSTOM.md): personal Telegram
// notifications for mentions and assignments, shown in Settings → Profile.
// Backend: server/cmd/server/custom_telegram_routes.go. Strings are not in
// the locale files on purpose, so upstream locale changes never conflict.

import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { clientErrorMessage } from "@multica/core/api";
import {
  createCustomTelegramLink,
  customTelegramKeys,
  customTelegramStatusOptions,
  disconnectCustomTelegram,
  sendCustomTelegramTest,
  updateCustomTelegramSettings,
  type CustomTelegramLink,
  type CustomTelegramStatus,
} from "@multica/core/custom-telegram";
import { Button } from "@multica/ui/components/ui/button";
import { Switch } from "@multica/ui/components/ui/switch";
import { openExternal } from "../../platform";
import { TelegramMark } from "./telegram-mark";
import { SettingsCard, SettingsRow, SettingsSection } from "./settings-layout";

function errorText(err: unknown, fallback: string) {
  return clientErrorMessage(err) ?? fallback;
}

export function CustomTelegramSection() {
  const queryClient = useQueryClient();
  const [pendingLink, setPendingLink] = useState<CustomTelegramLink | null>(null);
  const [busy, setBusy] = useState(false);

  // While a link is waiting for "Start" in Telegram, poll until it lands.
  const { data: status, isLoading } = useQuery(customTelegramStatusOptions({ poll: !!pendingLink }));

  useEffect(() => {
    if (!pendingLink) return;
    if (status?.connected) {
      setPendingLink(null);
      toast.success(
        status.telegram_username
          ? `Telegram connected as @${status.telegram_username}`
          : "Telegram connected",
      );
      return;
    }
    const msLeft = new Date(pendingLink.expires_at).getTime() - Date.now();
    const timer = setTimeout(() => setPendingLink(null), Math.max(msLeft, 0));
    return () => clearTimeout(timer);
  }, [pendingLink, status?.connected, status?.telegram_username]);

  const statusKey = customTelegramKeys.status();
  const setStatus = (next: CustomTelegramStatus) => queryClient.setQueryData(statusKey, next);
  const refresh = () => queryClient.invalidateQueries({ queryKey: statusKey });

  const connect = async () => {
    setBusy(true);
    try {
      const link = await createCustomTelegramLink();
      setPendingLink(link);
      openExternal(link.url);
    } catch (err) {
      toast.error(errorText(err, "Could not create a Telegram link"));
    } finally {
      setBusy(false);
    }
  };

  const disconnect = async () => {
    setBusy(true);
    try {
      await disconnectCustomTelegram();
      toast.success("Telegram disconnected");
      await refresh();
    } catch (err) {
      toast.error(errorText(err, "Could not disconnect Telegram"));
    } finally {
      setBusy(false);
    }
  };

  const sendTest = async () => {
    setBusy(true);
    try {
      await sendCustomTelegramTest();
      toast.success("Test message sent");
    } catch (err) {
      toast.error(errorText(err, "Sending the test message failed"));
      await refresh();
    } finally {
      setBusy(false);
    }
  };

  const toggle = async (key: "notify_mentions" | "notify_assignments", value: boolean) => {
    if (status) setStatus({ ...status, [key]: value });
    try {
      setStatus(await updateCustomTelegramSettings({ [key]: value }));
    } catch (err) {
      toast.error(errorText(err, "Could not save the setting"));
      await refresh();
    }
  };

  const title = (
    <span className="inline-flex items-center gap-2">
      <TelegramMark className="size-4 text-[#26A5E4]" />
      Telegram
    </span>
  );
  const description =
    "Get a Telegram message when you are mentioned or an issue is assigned to you. Applies to all workspaces.";

  if (isLoading || !status) {
    return <SettingsSection title={title} description={description}>{null}</SettingsSection>;
  }

  if (!status.configured) {
    return (
      <SettingsSection title={title} description={description}>
        <SettingsCard>
          <SettingsRow
            label="Not configured"
            description="Telegram notifications are not set up on this server."
          >
            {null}
          </SettingsRow>
        </SettingsCard>
      </SettingsSection>
    );
  }

  if (!status.connected) {
    return (
      <SettingsSection title={title} description={description}>
        <SettingsCard>
          <SettingsRow
            label="Not connected"
            description={
              pendingLink
                ? "Press “Start” in Telegram to finish. Waiting for confirmation…"
                : `Opens a chat with @${status.bot_username || "the Multica bot"} in Telegram.`
            }
          >
            <div className="flex items-center gap-2">
              {pendingLink ? (
                // Fallback when the browser blocked the automatic popup.
                <Button variant="outline" size="sm" onClick={() => openExternal(pendingLink.url)}>
                  Open Telegram
                </Button>
              ) : null}
              <Button size="sm" onClick={connect} disabled={busy}>
                {pendingLink ? "New link" : "Connect Telegram"}
              </Button>
            </div>
          </SettingsRow>
        </SettingsCard>
      </SettingsSection>
    );
  }

  return (
    <SettingsSection title={title} description={description}>
      <SettingsCard>
        <SettingsRow
          label={status.telegram_username ? `Connected as @${status.telegram_username}` : "Connected"}
          description={
            status.linked_at
              ? `Since ${new Date(status.linked_at).toLocaleDateString()} · via @${status.bot_username}`
              : undefined
          }
        >
          <div className="flex items-center gap-2">
            <Button variant="outline" size="sm" onClick={sendTest} disabled={busy}>
              Send test message
            </Button>
            <Button variant="outline" size="sm" onClick={disconnect} disabled={busy}>
              Disconnect
            </Button>
          </div>
        </SettingsRow>
        <SettingsRow
          label="When I'm mentioned"
          description="In an issue description or comment."
        >
          <Switch
            checked={status.notify_mentions}
            onCheckedChange={(v) => void toggle("notify_mentions", v)}
          />
        </SettingsRow>
        <SettingsRow
          label="When an issue is assigned to me"
          description="Muting a group under Notifications in a workspace also silences it here."
        >
          <Switch
            checked={status.notify_assignments}
            onCheckedChange={(v) => void toggle("notify_assignments", v)}
          />
        </SettingsRow>
      </SettingsCard>
    </SettingsSection>
  );
}
