# Eigene Anpassungen (Fork lasse200188/multica)

Dieser Fork ist nur für den eigenen Betrieb auf multica.codingwizards.de gedacht. Es gelten die
Lizenzbedingungen in `LICENSE` (Multica License): `LICENSE` und `NOTICE` bleiben erhalten, Logo,
Produktname und Copyright-Hinweise in der Oberfläche werden nicht verändert, und es gibt keinen
Betrieb für Dritte.

- `main`: unveränderte Kopie des Originals (nur fast-forward, nie selbst bearbeiten)
- `custom`: eigene Änderungen, per Rebase auf das jeweils letzte Upstream-Release gesetzt
- Bauen, Deployen, Updates: `/srv/multica/ANLEITUNG-FORK.md`

## Regeln für eigene Änderungen

- Neue Logik in **neue Dateien** mit Präfix `custom_` (z. B. `server/cmd/server/custom_listeners.go`),
  in bestehenden Dateien nur **einzeilige Einhängepunkte**. So entstehen beim Rebase kaum Konflikte.
- Datenbank-Migrationen nur als `server/migrations/900_custom_<name>.{up,down}.sql` (fortlaufend 900, 901, …)
  und nur **additiv**: neue Tabellen oder neue Spalten mit Default. Kein DROP/RENAME/Typwechsel, sonst läuft
  das offizielle Image nicht mehr auf der Datenbank. Wie upstream (`AGENTS.md`): keine Foreign Keys/Cascades,
  Indizes nur `CONCURRENTLY` in eigener Datei.
- Jede Änderung unten eintragen.

## Änderungen gegenüber dem Original

| Funktion | Dateien | Einhängepunkte in Upstream-Dateien | Migrationen |
|---|---|---|---|
| Projektstatus automatisch aus Issues: alle done/cancelled (≥ 1 done) → `completed`; offenes Issue in `completed` → `in_progress`; begonnenes Issue in `planned` → `in_progress`; `paused`/`cancelled` unangetastet. Auslöser: Issue-Events (2 s gebündelt) + Sweep alle 10 min. Aus: `MULTICA_CUSTOM_PROJECT_AUTO_STATUS=off` | `server/cmd/server/custom_project_status.go` (+ `_test.go`) | `server/cmd/server/main.go`: `registerCustomProjectStatus(...)` nach `registerNotificationListeners` | – |
| Telegram-Benachrichtigung bei Erwähnung und Zuweisung: In Einstellungen → Profil Telegram verbinden (Deep-Link `t.me/<bot>?start=<token>`, eigener Bot per Long-Polling, `/stop` trennt). Danach geht jeder Inbox-Eintrag `mentioned`/`issue_assigned` des Nutzers aus allen Workspaces zusätzlich per Telegram raus (Workspace-Name, Issue, Ausschnitt, Link); zwei Schalter pro Nutzer. Stummschaltung in den Notifications eines Workspace gilt mit. Bot-Token: `MULTICA_CUSTOM_TELEGRAM_BOT_TOKEN` (ohne Token inaktiv), aus: `MULTICA_CUSTOM_TELEGRAM_NOTIFY=off` | `server/cmd/server/custom_telegram_notify.go`, `custom_telegram_routes.go` (+ `_test.go`), `packages/core/custom-telegram/` (+ Test), `packages/views/settings/components/custom-telegram-section.tsx` | `server/cmd/server/main.go`: `registerCustomTelegramNotify(...)`; `server/cmd/server/router.go`: `registerCustomTelegramRoutes(r)` im User-Scope; `packages/core/package.json`: Export `./custom-telegram`; `packages/views/settings/components/account-tab.tsx`: Import + `<CustomTelegramSection />` | `900_custom_telegram_notify` (Tabellen `custom_telegram_link`, `custom_telegram_link_token`) |

## Betriebsumgebung (nicht im Code)

- Reverse-Proxy (Traefik): `/api/*`, `/v1/*`, `/uploads/*`, `/ws`, `/health`, `/readyz` gehen direkt ans Backend,
  nur Seiten und `/auth/*` ans Web-Frontend. Grund: Der Next.js-Rewrite-Proxy lieferte bei parallelen Anfragen
  sporadisch 502 („Loading more failed — Retry“ in Boards). Details: `/srv/multica/ANLEITUNG-FORK.md`.
