---
arc42-section: 7
revisit-triggers:
  - install.sh / bootstrap orchestration changes
  - a new systemd unit or instance-template is added
  - the default DB path or config path moves
  - a new operator recovery command lands (doctor / db migrate / refresh-all-mcps class)
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A initial cut — #386) -->

# §7 Deployment View

tmux-tell is **deployable-as-documented today**. This section frames the
deployment topology at the architecture level and links the operational depth;
it does not replace the `install.sh` comments or [docs/operator-manual.md](../operator-manual.md).

## Topology — one host, one UID

```
host (single UID)
├── ~/.local/share/tmux-tell/messages.db        ← the bus (SQLite, WAL)
├── /etc/tmux-tell/config.toml                   ← per-agent config (settle-delay, …)
├── systemd --user
│   ├── tmux-tell-claude-mailman@<NAME>.service  ← one mailman per claude agent
│   └── tmux-tell-codex-mailman@<NAME>.service   ← one mailman per codex agent
└── tmux server
    └── panes ── each a registered agent (claude / codex adapter binary + MCP)
```

- **The bus** is a single SQLite file at the XDG-default path
  `~/.local/share/tmux-tell/messages.db` (canonical since #308). Both the
  systemd-managed mailmen and the chamber MCP servers resolve only this default
  path.
- **The daemons** are `systemctl --user` instance units, one per agent, from the
  templates in [`init/`](../../init/). No root daemon; sudo is needed only at
  install.

## Install path — `install.sh` (with `bootstrap`) is canonical

`sudo ./install.sh` builds + installs the adapter binaries and the systemd
templates, then drops privileges and runs the **`bootstrap`** subcommand, which
wires a fully-working bus in one invocation:

1. `systemctl --user daemon-reload`
2. stale-DB detect (delegates to `db migrate` if a pre-#308 DB is found)
3. `discover` to populate the agents table from current tmux state
4. `enable` + **`restart`** per mailman (restart, not `enable --now` — the latter
   is a no-op on an already-active unit and would leave the deleted-inode binary
   running post-deploy)
5. orphan walk of stale `…-mailman@<NAME>.service` units (`--prune-orphans` to act)
6. `refresh-all-mcps` so chamber MCPs rebind to the freshly-installed binary + DB

`install.sh --no-bootstrap` opts out and prints the manual next-steps.

## Operator recovery surface

| Command | Recovers from | Detail |
|---|---|---|
| `refresh-all-mcps` | stale MCP↔binary/DB inode binding after a deploy | cap-protected, operator-explicit |
| `doctor` | MCP/DB-binding divergence (orphaned inode) | walks live processes, exits non-zero on drift |
| `db migrate <path>` | a DB that needs to move (WAL-safe) | atomic; `--dry-run` available |
| `resend <id>` | a soft-failed delivery | re-queues |
| `uninstall.sh` | everything | one script |

## Deploy automation

CI deploy rides `.forgejo/workflows/deploy.yml` on a dedicated `alcatraz-host`
runner, invoking the deploy wrapper and hard-failing on `doctor` divergence.
The cut-and-ship chain and the deploy lane are documented in the BookStack
**Release & Deploy Procedure** page (see [docs/release-cut-checklist.md](../release-cut-checklist.md)
for the cut-side gates).

> Depth: [docs/operator-manual.md](../operator-manual.md),
> [docs/diagnostic-playbook.md](../diagnostic-playbook.md), and the `install.sh`
> top-comment.
