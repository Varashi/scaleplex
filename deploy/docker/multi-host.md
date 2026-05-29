# scaleplex multi-host on Docker

This walks through running scaleplex across multiple Docker hosts —
one host runs the orchestrator (and usually PMS), other hosts run
just a worker. Single-host all-in-one is covered by `compose.yaml`;
this is the "now I want to add a second GPU box" recipe.

> **Prerequisite — shared storage.** scaleplex's worker writes
> transcoded segments where PMS serves them from. On a single host
> bind-mounts work; across hosts you need the same path mounted on
> every box (NFS / SMB / iSCSI / Ceph / GlusterFS — operator's
> choice). `/transcode` must be writable by uid 1000; `/media` is
> read-only. WAN / public-cloud workers are blocked until the
> media-plane changes in the WAN-worker memo land.

> **Security posture.** LAN-only, HTTP plaintext. scaleplex does
> not ship auth or TLS. Wrap with Caddy/Traefik or restrict the
> subnet if you need either. Cross-site / WAN exposure is not
> supported by this deployment.

> **Host sysctl — GPU-busy load telemetry.** On each worker host,
> drop `kernel.perf_event_paranoid` to `0` so the worker's
> `CAP_PERFMON` can read the i915 PMU. Without it, the orchestrator
> still load-balances (on session count), but the per-engine
> GPU-busy% telemetry is unavailable. Distros default to a higher
> value (Ubuntu 24.04: 4) which blocks PMU even with the capability:
>
> ```bash
> echo "kernel.perf_event_paranoid=0" > /etc/sysctl.d/99-scaleplex.conf
> sysctl --system
> ```
>
> Skip this only if you'd rather not relax perf access on the host.

## Topology

```
┌─────────────────────────────┐         ┌─────────────────────────┐
│ Host A (orchestrator + PMS) │         │ Host B (worker only)    │
│                             │  PUSH   │                         │
│  scaleplex_orchestrator ◄───┼─────────┤ scaleplex_worker        │
│  scaleplex_worker           │         │  /dev/dri/renderD128    │
│  plex (DOCKER_MODS=shim)    │         │  /media, /transcode     │
└──────────┬──────────────────┘         └─────────────────────────┘
           │                            ┌─────────────────────────┐
           │                            │ Host C (worker only)    │
           └─PUSH───────────────────────┤ scaleplex_worker        │
                                        └─────────────────────────┘
```

## Three discovery modes

The orchestrator merges workers from up to three sources, deduped by
URL. Pick whichever fits your fleet — they coexist freely.

| Mode | Activated by | Adding a new worker | Best for |
|---|---|---|---|
| **DNS** | `WORKERS_DNS=<hostname>` on orchestrator | Add A-record / `/etc/hosts` entry pointing to worker, restart-free auto-pickup within `WORKERS_REFRESH_SECONDS` | Compose single-host (Docker DNS) or fleets with LAN DNS |
| **LIST** | `WORKERS_LIST=h1:3501,h2:3501,…` on orchestrator | Append to the env, restart orchestrator | Fixed multi-host without DNS infra |
| **PUSH** | `SCALEPLEX_ORCHESTRATOR_URL=…` on each worker | Just `docker run` the worker — joins automatically | Friction-free Docker multi-host; future autoscaling |

PUSH workers heartbeat every 5s; the orchestrator reaps them after
15s of silence (`WORKERS_PUSH_TIMEOUT_SECONDS`).

## Recipe — Host A (orchestrator + PMS + local worker)

Use `compose.yaml` from this directory. Set `WORKERS_LIST` in `.env`
to include any remote workers that won't be using PUSH, or leave it
empty and rely on PUSH from the remote hosts.

```bash
cd deploy/docker
cp .env.example .env
$EDITOR .env          # adjust paths + WORKERS_LIST
docker compose up -d
```

Verify:

```bash
curl -s http://localhost:3500/workers | jq .
# → array; each entry has "sources":"dns" / "list" / "push" (or combos)
```

## Recipe — Host B (worker only, PUSH discovery)

The friction-free path: one `docker run` and the worker self-registers
with the orchestrator on Host A.

```bash
docker run -d --name scaleplex-worker --restart unless-stopped \
  --device /dev/dri/renderD128:/dev/dri/renderD128 \
  --user 1000:100 --group-add 44 --group-add 568 \
  -v /srv/media:/media:ro \
  -v /srv/transcode:/transcode \
  -p 3501:3501 \
  -e SCALEPLEX_ORCHESTRATOR_URL=http://host-a.lan:3500 \
  -e SCALEPLEX_WORKER_HOST=host-b.lan \
  -e SCALEPLEX_FORCE_HW=1 \
  ghcr.io/varashi/scaleplex_worker:latest
```

Notes:
- `SCALEPLEX_WORKER_HOST=host-b.lan` is what the orchestrator will
  call back to — must resolve from Host A and be reachable on port
  3501. Without this env, the worker advertises its container
  hostname, which is rarely resolvable across hosts.
- `-p 3501:3501` exposes the worker port so the orchestrator can
  call it. Skip if Host A and Host B share a Docker swarm network.
- Same `/transcode` + `/media` paths as PMS uses on Host A.

Check the worker showed up:

```bash
curl -s http://host-a.lan:3500/workers | jq '.[] | select(.sources | contains("push"))'
```

## Recipe — Host C (worker only, LIST discovery)

If you'd rather keep workers passive and have the orchestrator
discover them, add Host C to `WORKERS_LIST` on Host A (no env
needed on Host C itself):

```bash
# On Host A:
$EDITOR .env  # add: WORKERS_LIST=host-b.lan:3501,host-c.lan:3501
docker compose up -d   # picks up the change on restart
```

```bash
# On Host C:
docker run -d --name scaleplex-worker --restart unless-stopped \
  --device /dev/dri/renderD128:/dev/dri/renderD128 \
  --user 1000:100 --group-add 44 --group-add 568 \
  -v /srv/media:/media:ro -v /srv/transcode:/transcode \
  -p 3501:3501 \
  ghcr.io/varashi/scaleplex_worker:latest
```

## Troubleshooting

**Worker registers (in PUSH mode) but orchestrator can't dial it.**
The host the worker advertised (`SCALEPLEX_WORKER_HOST` or the
container hostname) doesn't resolve from the orchestrator, OR port
3501 isn't reachable. `curl http://<worker-host>:3501/healthz` from
the orchestrator container to confirm.

**Worker reaped repeatedly.** Heartbeat or `/capability` poll is
failing. Check `docker logs scaleplex-orchestrator` for the
`push: reaped worker …` line — the lag value in parens tells you
how long the gap was. If the worker is up but unreachable, see
above.

**`/workers` returns `sources: "list+push"` for one entry.** Expected
— the worker is vouched for by two sources at once. Dedup happens on
URL; the merged entry still corresponds to one physical worker.

**`/transcode` permission errors.** Worker runs as uid 1000 / gid 100
with supplementary groups 44 + 568. The mount needs to be writable
by uid 1000. On NFS with `root_squash`, the export must map uid 1000
through (or the dir must already be owned 1000:1000 with `0775`).

## Logs

scaleplex containers (orchestrator, worker) are stateless and log to
stdout/stderr — there is no in-container log file or PVC. Durable logging
is the operator's responsibility, same as the k8s deployment (which relies
on a cluster log pipeline):

- **Recent / live:** `docker compose logs -f` or `docker logs <container>`.
  The `compose.yaml` ships a `json-file` driver capped at 3×10 MB per
  container (`x-logging` anchor) so a long-running worker can't fill the
  host disk — but that's a small rolling window, not an archive.
- **Durable / central (recommended for fleets):** point Docker's logging
  driver at your log stack instead of `json-file`. Per-service override:

  ```yaml
  services:
    worker:
      logging:
        driver: fluentd          # or loki / syslog / journald / gelf
        options:
          fluentd-address: "log-host.lan:24224"
          tag: "scaleplex.worker"
  ```

  Or set it host-wide in `/etc/docker/daemon.json` (`"log-driver"` +
  `"log-opts"`) so every container ships automatically. The per-session
  rewriter lines (`rewriter applied: …`, incl. `force-hw:*` re-accel tags)
  flow through whichever driver you choose — there is no scaleplex-side
  log persistence to configure.

## What this deployment does NOT cover

- **WAN / public-cloud workers.** Workers and PMS need to share a
  filesystem; that's a LAN-only assumption today. The WAN-worker
  future memo covers media + segment HTTP planes that would unblock
  this.
- **Authentication.** scaleplex's `/register`, `/task`, etc. are
  HTTP without auth. Anyone on the subnet who can reach the
  orchestrator can dispatch transcodes or register a fake worker.
  Wrap with a reverse proxy if your LAN has untrusted devices.
- **TLS.** Same reasoning. Caddy/Traefik in front terminates TLS
  with auto-renewal.
- **mTLS / shared-secret auth.** Future work alongside WAN support.
