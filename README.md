# Account Reset Manager for CLIProxyAPI

Fork of [JefferyZhang2019/cpa-plugin-codex-auto-reset](https://github.com/JefferyZhang2019/cpa-plugin-codex-auto-reset),
adding Claude grants and durable reset scheduling to the Codex implementation.
The plugin ID/library name is **`reset-manager`**. It makes no model requests,
runs neither CLI, and reads current credentials from CLIProxyAPI before each step.

## Behavior

- **Observation only by default:** automatic and manual spending remain disabled.
- Codex redemption names the exact credit. Claude respects eligibility, paused or
  blocked grants, start/end dates, covered usage, cooldown, and exhaustion rules.
- Claude multi-use grant units have distinct identities, so an uncertain retry
  cannot silently become a new logical attempt against the next unit.
- A full intent (account identity, target, request UUID, pre-reset snapshot) is
  atomically written and fsynced before POST. Disk failure prevents the request.
- Retries and restarts retain request IDs. A changed account identity pauses the
  automation. Duplicate logins for one account are ignored within the worker.
- Ambiguous operations keep their target; they never select a replacement credit.
  Claude ambiguity lasting ten minutes moves to `HOLD` for read-only reconciliation.
- Verification checks credit deduction and covered-quota recovery, including
  Claude model-specific grants. Only verified results clear gateway cooldowns.
- State, logs, and history persist without OAuth credential values. State recovery
  from corruption/backup starts in observation mode.

Non-expiring credits are not spent automatically. Manual redemption cannot replace
an unresolved attempt in `HOLD`. Provider rules can prevent redemption before expiry;
the manager reports the reason instead of manufacturing usage.

## Build and configure

Requires Go 1.26+, CGO, and a C compiler:

```sh
go test -race ./...
go vet ./...
(cd sim && go test ./...)
make build VERSION=0.2.0
```

Copy `dist/reset-manager.so` to the platform plugin directory, for example
`/var/lib/cliproxyapi/plugins/linux/amd64/`. The host must include dynamic plugin
support. This integration targets the CLIProxyAPI v8 ABI, including v8.0.47.

```yaml
plugins:
  enabled: true
  dir: /var/lib/cliproxyapi/plugins
  configs:
    reset-manager:
      enabled: true
      priority: 1
      observe_only: true
      refresh_interval: 10m
      trigger_lead_time: 10m
      retry_interval: 1m
      enabled_accounts: [CODEX_AUTH_INDEX, CLAUDE_AUTH_INDEX]
      state_dir: /var/lib/cliproxyapi/reset-manager
```

These values bootstrap fresh state. Later settings persist in `state.json` and are
edited through the authenticated UI/API. Select accounts explicitly for personal
and work control. The inherited `"*"` option includes future supported logins too.
Inherited interval defaults remain twelve-hour discovery and a six-hour lead;
set the ten-minute values above to match the expiring-bank automation.

Open `/v0/resource/plugins/reset-manager/status` and enter the management key.
The page shows providers, balances, expiry, blocked reasons, next action, mode,
logs, and history. It embeds no account data; reads and actions require the key.
Management routes under `/v0/management/plugins/reset-manager/` include `status`,
`accounts`, `logs`, `settings`, `check`, `check/all`, `reset`, and `export`.
`PUT settings` accepts `observe_only`, `refresh_interval`, `trigger_lead_time`, and
`enabled_accounts`; the operator's state directory and retry interval are retained.

## Migration

1. Compare real provider account identities with the old scheduler; do not copy
   CLI refresh tokens into another credential store.
2. Run the plugin in observation mode alongside the old timers. Compare balances
   and `expiry - trigger_lead_time` with their schedules.
3. Stop discovery and due timers, wait for active legacy services to finish, and
   check for pending attempts. Preserve old state/history. Unresolved operations
   must retain their original request IDs before transferring ownership.
4. Enable spending for the matched accounts. Restart and verify durable state,
   selected accounts, mode, and provider snapshots.

Run **one redemption scheduler per real provider account**. Other gateways,
original plugins, scripts, and native manual reset buttons do not share these
locks. Use this manager's manual action while its automation is active.
Back up the plugin state, gateway auth files, and configuration together.
`HOLD` requires inspection of logs and provider balances before clearing it.
To roll back, first enable observation mode and confirm no pending operation,
then re-enable the old timers.

Provider write tests use local fixtures only, including blocked grants, multi-use
identity, model-specific quota recovery, durable restart replay, disk failures,
observation mode, and account changes. Live rollout checks read balances only;
no real credit is redeemed as a test.

MIT license; original attribution is preserved in LICENSE and repository history.
