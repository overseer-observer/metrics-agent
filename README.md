# Metrics agent

Lightweight Linux host telemetry agent. It runs as a systemd service, samples the host once
a minute and ships the samples to the backend it is configured for, where they turn into
charts and threshold alerts. Single static binary, no dependencies, no agent-side database.

The agent is backend-agnostic: the endpoint it reports to is supplied at install time, so the
same build serves any service that speaks the agent protocol.

## What it collects

Every tick the agent reads `/proc` and calls `statfs(2)`:

- **CPU** — core count and user / system / iowait / steal / idle shares over the interval
- **Load average** — 1, 5 and 15 minutes
- **Memory** — total, free, available, buffers, cached, shmem (bytes)
- **Swap** — total, used, and in/out paging rates
- **Filesystems** — per mount point: space and inodes, used and total
- **Host facts** — hostname, OS, boot id, uptime, agent version

Pseudo filesystems (`proc`, `sysfs`, `tmpfs`, `overlay`, …) are skipped, and network mounts
(`nfs`, `cifs`, `ceph`, …) are never touched — `statfs` on an unreachable server can hang.
At most 32 mount points are reported, largest first.

No process list, no command lines, no file contents, no user data — only the counters above.

## How it behaves

- **Report interval** — 60 s by default. The server may change it in its response
  (clamped to 10 s … 24 h). The exact second within the interval is derived from the token,
  so many hosts do not hit the backend at the same moment.
- **Disk buffer** — samples that fail to reach the backend are appended to a JSONL buffer in
  `/var/lib/metrics-agent/buffer` and re-sent later. The buffer is capped at 32 MiB and
  7 days; oldest samples are evicted first, so the agent cannot fill the disk.
- **Backpressure** — batches of up to 60 samples, gzip above 2 KiB, 128 KiB body cap,
  10 s request timeout, 3 attempts with exponential backoff on 5xx and network errors.
  `Retry-After` is honoured. After `401` the agent retries every 15 minutes instead of
  giving up or hammering the endpoint.
- **Privileges** — runs as the unprivileged `metrics-agent` user under a hardened systemd
  unit (`ProtectSystem=strict`, empty `CapabilityBoundingSet`, a `SystemCallFilter` deny list).
  Writes only to its own state directory.

## Requirements

- Linux with systemd, `amd64` or `arm64`
- root for install/uninstall
- outbound HTTPS to the backend endpoint
- an agent token (UUID) issued by that backend

## Installed layout

Take the install command from the service you are connecting this host to: it carries the
correct endpoint and your token. `install.sh` accepts `--endpoint <url>` and the token as a
positional argument, and it is the release artifact next to the binaries on the
[releases page](../../releases/latest). A later token change is done with `set-token.sh`
from the same page, see [Rotating the token](#rotating-the-token).

The script creates the `metrics-agent` system user, installs the binary to
`/usr/local/bin/metrics-agent`, writes `/etc/metrics-agent/config.yaml` (mode `0640`), then
installs and starts `metrics-agent.service`.

Check it:

```sh
systemctl status metrics-agent
journalctl -u metrics-agent -f
```

Re-running the same command upgrades the binary and the unit; an existing config is left
untouched, so `--endpoint` and the token may be omitted on an upgrade.

## Configuration

`/etc/metrics-agent/config.yaml`:

```yaml
endpoint: https://example.com/api/agent/v1/metrics
token: 00000000-0000-4000-8000-000000000000
log_level: info   # debug | info | warn | error | none
buffer_path: /var/lib/metrics-agent/buffer
```

All four fields are required. The token must be a UUID and never appears in the logs.
`METRICS_AGENT_TOKEN` overrides the token from the file. The report interval is not
configurable — it is set by the server.

`install.sh` creates the config as `root:metrics-agent 0640`. If the file is world-readable
the agent only logs a warning at startup and keeps running — it does not refuse to start,
because a stalled agent is worse than a token an already-privileged local user can read.
Tightening the mode is up to you: `chmod 0640 /etc/metrics-agent/config.yaml`, or keep the
token out of the file entirely via `METRICS_AGENT_TOKEN` in a systemd drop-in.

Environment variables understood by `install.sh`: `METRICS_AGENT_TOKEN`,
`METRICS_AGENT_ENDPOINT`, `METRICS_AGENT_BASE_URL`, `METRICS_AGENT_REPO`.

Running the binary directly:

```sh
metrics-agent --config /etc/metrics-agent/config.yaml
metrics-agent --version
```

Apply config changes with `systemctl restart metrics-agent`.

## Rotating the token

When the backend issues a new token for the host, `set-token.sh` replaces it in the existing
config and restarts the service. It ships on the
[releases page](../../releases/latest) next to `install.sh`:

```sh
curl -fsSL https://github.com/<owner>/<repo>/releases/latest/download/set-token.sh \
  | sudo sh -s -- <new-token>
```

A token passed as an argument is visible in `ps` to any user of the machine. On a shared
machine download the script first and let it ask, or pass the token through the environment:

```sh
curl -fsSL https://github.com/<owner>/<repo>/releases/latest/download/set-token.sh -o set-token.sh
sudo sh set-token.sh                                   # prompts for the token
METRICS_AGENT_TOKEN=<new-token> sudo -E sh set-token.sh
```

The script touches nothing but the `token:` line — the endpoint, the log level and the
buffered samples stay as they are, and the buffer is delivered under the new token.
Re-running `install.sh` does not help here: it leaves an existing config untouched.

## Release verification

Binaries are published on the [releases page](../../releases/latest) as
`metrics-agent-linux-amd64` and `metrics-agent-linux-arm64`, together with `SHA256SUMS`.
`install.sh` checks the downloaded binary against `SHA256SUMS`. The binaries also carry
GitHub build provenance, which can be checked by hand with `<owner>/<repo>` being this
repository:

```sh
gh attestation verify metrics-agent-linux-amd64 --repo <owner>/<repo>
```

## Uninstallation

`uninstall.sh` ships alongside the binaries; with the repository checked out, `sudo ./uninstall.sh`.

This stops and disables the service and removes the unit, the binary,
`/etc/metrics-agent` (including the config), `/var/lib/metrics-agent` (including buffered
samples) and the `metrics-agent` user. Nothing is left behind.

## Building from source

Go 1.27+:

```sh
make build          # dist/metrics-agent-linux-{amd64,arm64}, static, CGO disabled
make test vet fmt
```

Tagging a commit `vX.Y.Z` and pushing the tag builds both architectures and publishes a
GitHub release with the binaries, the install scripts and `SHA256SUMS`.
