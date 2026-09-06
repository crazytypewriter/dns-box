# dns-box

[![Go Version](https://img.shields.io/badge/go-1.27-blue)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

[README на русском](README.ru.md)

A high-performance DNS forwarding server written in Go, designed for Linux routers and embedded devices. It resolves DNS through encrypted upstreams, routes selected domains through a VPN by populating kernel ipsets, blocks ads/trackers, and backs its configuration up to GitHub.

## Table of Contents

- [Features](#features)
- [Architecture](#architecture)
- [Installation](#installation)
- [Configuration](#configuration)
- [Running](#running)
- [HTTP API](#http-api)
- [ipset Integration](#ipset-integration)
- [Ad and Tracker Blocking](#ad-and-tracker-blocking)
- [GitHub Backup](#github-backup)
- [Caching](#caching)
- [Build and Deploy](#build-and-deploy)
- [Testing](#testing)
- [Project Layout](#project-layout)
- [Logging](#logging)
- [License](#license)

---

## Features

- **DNS forwarding** with multiple upstream servers and failover
- **Protocols**: UDP, TCP, DNS-over-HTTPS (DoH), DNS-over-TLS (DoT)
- **VPN routing** — resolved IPs of matched domains are automatically added to Linux ipsets so iptables/nftables can route them through a tunnel
- **Ad/tracker blocking** via external hosts-format blocklists
- **DNS caching** with configurable TTL normalization and negative caching
- **Demand prefetching** — records close to TTL expiry are refreshed in the background while the client gets an instant cached answer
- **HTTP API** for managing domains, suffixes, lists, blocklists and static CIDRs
- **GitHub backup** of dynamic configuration with merge-on-restore
- **Upstream prioritization** — encrypted (DoH/DoT) upstreams race first; plaintext ones join only if they fail, so domain names never leak in cleartext
- **Conditional forwarding** — per-zone upstreams (`forward_zones`)
- **Local overrides** — `/etc/hosts`-format file with hot reload
- **Rate limiting** — token bucket per client IP
- **DNS rebinding protection** — private addresses stripped from answers
- **ipset persistence** — state file survives reboots; sets are repopulated before the first client query

---

## Architecture

```
                    ┌─────────────────────────────────────────┐
                    │           dns-box (port 953)            │
                    │                                         │
  Client ──UDP──►   │  ┌───────────┐    ┌──────────────────┐  │
  DNS query         │  │  Handler   │───►│   DNS Resolver   │──┼──► Upstream DNS
                    │  │            │    │  (DoH/DoT/UDP)   │  │
                    │  └─────┬─────┘    └──────────────────┘  │
                    │        │                                 │
                    │        ▼                                 │
                    │  ┌───────────┐    ┌──────────────────┐  │
                    │  │  ipset    │    │   BlockList      │  │
                    │  │  (VPN)    │    │  (blocking)      │  │
                    │  └───────────┘    └──────────────────┘  │
                    │        ▲                                 │
                    │        │                                 │
                    │  ┌─────┴─────┐    ┌──────────────────┐  │
                    │  │  Cache    │    │   DomainCache    │  │
                    │  │  (DNS)    │    │  (rules)         │  │
                    │  └───────────┘    └──────────────────┘  │
                    └─────────────────────────────────────────┘
                                         │
                    ┌────────────────────┴────────────────────┐
                    │         HTTP API (port 8090)            │
                    │  /domains  /suffixes  /ipset/...        │
                    └─────────────────────────────────────────┘
                                         │
                    ┌────────────────────┴────────────────────┐
                    │         GitHub Backup (optional)        │
                    │  hosts_config.json in your repository   │
                    └─────────────────────────────────────────┘
```

---

## Installation

### Requirements

- Go 1.27+ (see `go` in `go.mod`)
- Linux (for ipset support)
- root or CAP_NET_ADMIN for ipset operations

### Cloning

```bash
git clone https://github.com/crazytypewriter/dns-box.git
cd dns-box
```

### Building

```bash
# Local build
make local-build

# For ARM (routers, Raspberry Pi)
make arm-build

# Explicit version (CI injects the release tag)
make arm-build VERSION=v1.2.3
```

Version, commit and build date are baked in via `-ldflags`; a plain
`go build ./cmd/dns-box` produces version `dev`.

```bash
./dns-box -version
# dns-box v1.2.3
# commit: a1b2c3d
# built:  2026-01-01T00:00:00Z
# go:     go1.27.0 linux/arm
```

The binary version must match the release tag: `deploy/dns-box.init.d` compares
the first line of `dns-box -version` against the `tag_name` from the GitHub API
and re-downloads the binary on mismatch. That is why CI computes the tag
**before** building and passes it to `make arm-build VERSION=...`, and the first
output line must stay in the `dns-box <version>` format (version is the last
field).

### Installing on OpenWrt

`deploy/dns-box.init.d` is a procd script for OpenWrt: it goes to
`/etc/init.d/dns-box`, the binary lives in `/tmp/dns-box/dns-box`, the config —
in `/data/dns-box/config.json` (`/tmp` is wiped on reboot, `/data` survives).

```bash
scp deploy/dns-box.init.d root@router:/etc/init.d/dns-box
ssh root@router chmod +x /etc/init.d/dns-box
ssh root@router /etc/init.d/dns-box enable
ssh root@router /etc/init.d/dns-box start
```

With `AUTO_UPDATE=1` the script compares the first line of `dns-box -version`
with the `tag_name` of the latest release before starting and downloads a fresh
binary on mismatch. With `AUTO_UPDATE=0` (default) it downloads nothing — and
since the binary lives in `/tmp`, it will be gone after a reboot and the service
won't start. Either deliver it yourself (`make copyToRouter`), set
`AUTO_UPDATE=1`, or move `PROG` to `/data`.

If `github_backup` is enabled you must set the token inside the init script
itself: `procd_set_param env DNS_BOX_GITHUB_TOKEN=...` picks the variable from
the script's environment, and procd runs it with an empty one. Without a token
`Validate()` fails the startup.

There is also a one-line installer that fetches the latest release and the
init script (run on the router):

```bash
curl -fsSL https://raw.githubusercontent.com/crazytypewriter/dns-box/main/install.sh | sh
```

---

## Configuration

`config.json`:

```json
{
  "server": {
    "address": ["127.0.0.1:953", "[::]:953"],
    "log": "debug"
  },
  "dns": {
    "upstream_servers": [
      "https://dns.google/dns-query",
      "tls://1.1.1.1",
      "8.8.8.8:53"
    ],
    "timeout": 5,
    "rate_limit": 0,
    "block_private": false,
    "hosts_file": "",
    "forward_zones": []
  },
  "ipset": {
    "lists": [
      {
        "name": "vpn_domains",
        "enable_ipv6": true,
        "timeout": 7200,
        "rules": {
          "domain": ["rutor.is", "rutracker.org"],
          "domain_suffix": [".googlevideo.com", ".youtube.com"]
        }
      }
    ]
  },
  "blocklist": {
    "enabled": true,
    "urls": ["https://blocklistproject.github.io/Lists/tracking.txt"],
    "refresh_hours": 24
  },
  "github_backup": {
    "enabled": true,
    "token": "ghp_your_personal_access_token",
    "owner": "your-username",
    "repo": "dns-box-config",
    "path": "hosts_config.json",
    "branch": "main"
  }
}
```

### Section Reference

#### `server`

| Parameter | Type | Description |
|-----------|------|-------------|
| `address` | `[]string` | Listen addresses (IPv4 and IPv6); each gets both UDP and TCP |
| `log` | `string` | Log level: `trace`, `debug`, `info`, `warn`, `error` |

#### `dns`

| Parameter | Type | Description |
|-----------|------|-------------|
| `upstream_servers` | `[]string` | Upstream DNS servers (config order doesn't matter; priority is scheme-based, see below) |
| `timeout` | `int` | Per-upstream timeout in seconds, default `5`. Also the negative-cache TTL when NXDOMAIN arrives without SOA, and the delay before plaintext upstreams join the race |
| `forward_zones` | `[]object` | Conditional forwarding: `{"domain_suffix": ".corp.local", "servers": ["192.168.1.1"]}`. The most specific zone wins; its servers fully replace the global list |
| `hosts_file` | `string` | Path to an `/etc/hosts`-format file for local A/AAAA overrides. Empty = disabled. Reloaded by mtime once a minute; takes precedence over blocklists and cache |
| `rate_limit` | `int` | Queries per second per client IP, `0` = unlimited. Excess → `REFUSED`. Burst never goes below 10 so `rate_limit: 1` doesn't kill normal bursts |
| `block_private` | `bool` | DNS rebinding protection: strip private addresses (RFC 1918/4193, loopback, link-local) from answers |

**Supported upstream protocols:**

| Prefix | Protocol | Default port |
|--------|----------|--------------|
| `https://` or `doh://` | DNS-over-HTTPS | 443 |
| `tls://` or `dot://` | DNS-over-TLS | 853 |
| `tcp://` | TCP DNS | 53 |
| `udp://` or no prefix | UDP DNS | 53 |

**Priority:** DoH → DoT → TCP → UDP

Encrypted (`https`/`doh`/`tls`/`dot`) and plaintext (`tcp`/`udp`) upstreams
race in two waves. First, only the encrypted ones compete: queries start
staggered by 300 ms, the first valid answer wins. Plaintext upstreams join only
when every encrypted one has errored or timed out — otherwise the queried
domain name would leak in cleartext on every request even when DoH ultimately
answers. If there are no encrypted upstreams, plaintext ones start immediately.

#### `ipset` — VPN routing

The `ipset` section defines which domains' resolved IPs go into which Linux
ipset for subsequent VPN routing.

**Core idea:** dns-box listens for DNS queries; when a domain matches a rule,
the answer's IPs are added to the corresponding ipset. iptables then routes
traffic to those IPs through the tunnel.

Each element of `lists` is a separate ipset with its own domain rules.

| List parameter | Type | Description |
|----------------|------|-------------|
| `name` | `string` | Base ipset name. Used as-is for IPv4; IPv6 gets a `6` suffix (`vpn_domains` → `vpn_domains6`) |
| `enable_ipv6` | `bool` | Also create the IPv6 set (`name` + `6`) |
| `timeout` | `uint32` | Entry timeout in seconds. `0` = default (7200 = 2 hours) |
| `persistent` | `bool` | `true` — entries are added without a lifetime (`timeout 0`). Default `false` |
| `prefetch` | `bool` | `true` — enable demand prefetch: when a cached record is near TTL expiry (less than 10% of the original TTL), the client gets the cached answer instantly while a refresh runs in the background. Default `false` |
| `maxelem` | `uint32` | Set size limit, `0` = kernel default (65536). **Only applied at set creation**: if the set already exists (survived a process restart), the limit is not changed — to raise it, `ipset destroy <name>` and restart |
| `rules` | `RulesConfig` | Domain rules for this list (see below) |

**`rules` parameters (per list):**

| Parameter | Type | Description |
|----------|------|-------------|
| `domain` | `[]string` | Exact domains. Matches only the given name |
| `domain_suffix` | `[]string` | Domain suffixes (start with `.`). Matches the domain and all its subdomains |

**Rule matching examples:**

| Rule | Matches | Does not match |
|------|---------|----------------|
| `"youtube.com"` | `youtube.com` | `www.youtube.com`, `myyoutube.com` |
| `".youtube.com"` | `www.youtube.com`, `m.youtube.com` | `youtube.com`, `myyoutube.com` |

**Multiple lists example:**

```json
"ipset": {
  "lists": [
    {
      "name": "vpn_domains",
      "enable_ipv6": true,
      "timeout": 7200,
      "rules": {
        "domain": ["rutracker.org", "rutor.is"],
        "domain_suffix": [".youtube.com"]
      }
    },
    {
      "name": "proxy_domains",
      "enable_ipv6": false,
      "timeout": 3600,
      "rules": {
        "domain": ["example.com"],
        "domain_suffix": [".example.org"]
      }
    }
  ]
}
```

This creates:
- `vpn_domains` (IPv4) and `vpn_domains6` (IPv6) — for the first list's domains
- `proxy_domains` (IPv4 only) — for the second list's domains

> **Backward compatibility:** the old `ipv4name`/`ipv6name` fields still work; with them, rules come from the root `rules` section.

> **Note:** ipset works on Linux only. ipset entry TTLs go through the TTL normalization (see [Caching](#caching)).

##### Keepalive: extending entries with live traffic

Set entries have timeouts, and a long-lived connection without fresh DNS
queries (streaming, websockets, torrents) can drop out of the set mid-flight.
Fix it with a `SET --exist` rule in the same chain where the set is matched:
every packet to an address in the set resets its timer.

```bash
iptables -t mangle -A PREROUTING \
  -m set --match-set vpn_domains dst \
  -j SET --add-set vpn_domains dst --exist --timeout 7200
```

nftables equivalent (fw4 firmwares):

```
table inet mangle {
  chain prerouting {
    type filter hook prerouting priority mangle; policy accept;
    add @ipset_vpn_domains { ip daddr timeout 7200 }
  }
}
```

The rule is redundant for `persistent: true` lists — their entries have no lifetime.

##### Static CIDRs (`net_lists`) and the state file (`state`)

`net_lists` parameters:

| Parameter | Type | Description |
|----------|------|-------------|
| `persistent` | `bool` | Default `true` — static CIDRs are added without a lifetime. Explicit `false` restores the old timeout behavior |
| `refresh_minutes` | `int` | Periodic re-add interval for all CIDRs (insurance against reboots and external `ipset flush`). Default 60, `0` = disabled |
| `maxelem` | `uint32` | Set size limit, `0` = kernel default (65536). Large ASN lists need this raised. Applied at set creation only (see above) |

The `state` section enables an on-disk mirror of ipset entries for reboot
recovery (while clients still hold their own DNS caches, sets are repopulated
from the file before the first query):

```json
"state": {
  "enabled": true,
  "path": "/data/dns-box/ipset-state.json",
  "flush_interval_minutes": 10,
  "max_entries_per_set": 20000
}
```

The file is written atomically (tmp → fsync → rename) and only on changes; a
corrupt file at startup is a warning and an empty mirror, not fatal.

During restore, entries already present in the kernel set are skipped: after a
process restart (as opposed to a reboot) the sets are alive and kernel timers
are more accurate than the file — including extensions made by the
`-j SET --exist` rule, which the mirror cannot see. If the set cannot be read,
all entries are restored.

#### `blocklist`

| Parameter | Type | Description |
|-----------|------|-------------|
| `enabled` | `bool` | Enable/disable blocking |
| `urls` | `[]string` | Blocklist URLs (HTTP/HTTPS or local files) |
| `refresh_hours` | `int` | Refresh interval in hours |

**Blocklist format:** standard hosts format:
```
0.0.0.0 tracker.example.com
0.0.0.0 ads.example.com
```

**Failed refresh behavior.** A failed run is not published: if no source could
be read or the stream broke midway, the previous list stays in memory and an
`ERROR` is logged. A day-stale list is cheaper than silently disabled blocking.
If some sources are alive, whatever was read is published with a `WARN`
(otherwise one permanently dead URL would freeze updates forever). On the very
first run there is nothing to keep, so blocking stays off until the next
refresh — also logged.

#### `github_backup`

| Parameter | Type | Description |
|-----------|------|-------------|
| `enabled` | `bool` | Enable GitHub backup |
| `token` | `string` | GitHub Personal Access Token (`repo` scope) |
| `owner` | `string` | User or organization name |
| `repo` | `string` | Repository name |
| `path` | `string` | Path to the config file inside the repository |
| `branch` | `string` | Repository branch |

> **Getting a token:** Settings → Developer settings → Personal access tokens → Generate new token → select the `repo` scope.

> **Keep the token out of the config file.** The `DNS_BOX_GITHUB_TOKEN`
> environment variable takes precedence over the `token` field, while
> `config.json` itself goes into backups and backup rotation. Note: with
> `github_backup.enabled: true` a token is mandatory — without one,
> `Validate()` aborts startup and the router service won't come up.

#### `api`

| Parameter | Type | Description |
|-----------|------|-------------|
| `address` | `string` | HTTP API listen address, default `:8090` |
| `token` | `string` | Optional bearer token; `DNS_BOX_API_TOKEN` env takes precedence |

---

## Running

### Plain

```bash
./dns-box -config config.json
```

### As a service (systemd)

Create `/etc/systemd/system/dns-box.service`:

```ini
[Unit]
Description=DNS Box - DNS Server with VPN routing
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/dns-box -config /etc/dns-box/config.json
Restart=on-failure
RestartSec=5
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable dns-box
sudo systemctl start dns-box
sudo systemctl status dns-box
```

---

## HTTP API

The API listens on port `8090` by default. The address is set in `api.address`;
access can be protected with a bearer token:

```json
"api": {
  "address": "127.0.0.1:8090",
  "token": ""
}
```

Prefer the `DNS_BOX_API_TOKEN` environment variable over the `token` field (env
wins). When a token is set, every request must carry
`Authorization: Bearer <token>`.

### Managing domains (legacy root rules)

#### List all domains

```bash
curl http://localhost:8090/domains
```

**Response:**
```json
["example.com", "rutracker.org"]
```

#### Add domains

```bash
curl -X POST http://localhost:8090/domains \
  -d "example.com
another.com
third.org"
```

**Response:** `ok` or `domain example.com exist` (if already present)

#### Remove domains

```bash
curl -X DELETE http://localhost:8090/domains \
  -d "example.com
another.com"
```

**Response:** `ok` or `domain example.com not found`

---

### Managing suffixes (legacy root rules)

#### List all suffixes

```bash
curl http://localhost:8090/suffixes
```

**Response:**
```json
[".youtube.com", ".googlevideo.com"]
```

#### Add suffixes

```bash
curl -X POST http://localhost:8090/suffixes \
  -d ".example.com
.google.com"
```

> A suffix is automatically prefixed with a dot if missing: `example.com` → `.example.com`

#### Remove suffixes

```bash
curl -X DELETE http://localhost:8090/suffixes \
  -d ".example.com"
```

---

### Managing ipset lists

#### List all ipset lists

```bash
curl http://localhost:8090/ipset/lists
```

**Response:**
```json
[
  {
    "name": "vpn_domains",
    "enable_ipv6": true,
    "timeout": 7200,
    "rules": {
      "domain": ["rutracker.org"],
      "domain_suffix": [".youtube.com"]
    }
  },
  {
    "name": "proxy_domains",
    "enable_ipv6": false,
    "timeout": 3600,
    "rules": {
      "domain": ["example.com"],
      "domain_suffix": []
    }
  }
]
```

#### Create a list

```bash
curl -X POST http://localhost:8090/ipset/lists \
  -H "Content-Type: application/json" \
  -d '{"name": "zapret_domains", "enable_ipv6": true, "timeout": 7200, "maxelem": 65536}'
```

Creates both the config entry and the kernel sets (`zapret_domains` +
`zapret_domains6`) immediately — no restart needed. Then fill it via
`/ipset/zapret_domains/domains` and `/ipset/zapret_domains/suffixes`.

#### Delete a list

```bash
curl -X DELETE http://localhost:8090/ipset/lists \
  -H "Content-Type: application/json" \
  -d '{"name": "zapret_domains"}'
```

Removes the list from the config and caches; kernel sets are not destroyed —
entries expire by timeout.

> **GitHub restore is a merge, not a replacement:** at startup, lists from the
> backup are merged with local ones by name (GitHub wins on name conflicts), so
> a list created on the router and not yet in the backup does not disappear. It
> is pushed to GitHub on the first `SaveConfig` after creation.

#### List domains of a specific list

```bash
curl http://localhost:8090/ipset/vpn_domains/domains
```

#### Add domains to a list

```bash
curl -X POST http://localhost:8090/ipset/vpn_domains/domains \
  -d "newdomain.com
another.org"
```

#### Remove domains from a list

```bash
curl -X DELETE http://localhost:8090/ipset/vpn_domains/domains \
  -d "newdomain.com"
```

#### List / add / remove suffixes of a specific list

```bash
curl http://localhost:8090/ipset/proxy_domains/suffixes
curl -X POST http://localhost:8090/ipset/vpn_domains/suffixes -d ".newdomain.com"
curl -X DELETE http://localhost:8090/ipset/vpn_domains/suffixes -d ".newdomain.com"
```

> **Note:** the old `/domains` and `/suffixes` endpoints keep working for the
> legacy configuration (`ipv4name`/`ipv6name`).

---

### Managing blocklists

#### List blocklist URLs

```bash
curl http://localhost:8090/blocklist/urls
```

#### Add / remove a blocklist URL

```bash
curl -X POST http://localhost:8090/blocklist/urls \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/blocklist.txt"}'

curl -X DELETE http://localhost:8090/blocklist/urls \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/blocklist.txt"}'
```

---

### Managing static CIDRs (net_lists)

#### List all net_lists

```bash
curl http://localhost:8090/ipset/net_lists
```

**Response:**
```json
[{"name":"vpn_subnets","enable_ipv6":true,"timeout":7200,"asn":"","cidr":["91.108.56.0/22"],"persistent":null,"refresh_minutes":null}]
```

#### List CIDRs of a specific net list

```bash
curl http://localhost:8090/ipset/net/vpn_subnets/cidrs
```

#### Add CIDRs

```bash
curl -X POST http://localhost:8090/ipset/net/vpn_subnets/cidrs \
  -d "149.154.160.0/20
2a0a:f280::/32"
```

IPv6 CIDRs automatically go into the `vpn_subnets6` set (when the list has
`enable_ipv6: true`). Entries are added to ipset immediately; for persistent
lists — without a lifetime.

#### Remove CIDRs

```bash
curl -X DELETE http://localhost:8090/ipset/net/vpn_subnets/cidrs \
  -d "149.154.160.0/20"
```

---

## ipset Integration

### What is ipset?

ipset is a Linux mechanism for storing sets of IP addresses, used with
iptables/nftables for efficient routing.

### How dns-box uses ipset

1. At startup, ipsets are created from the `ipset.lists` configuration
2. Each list creates an IPv4 set with the given name and optionally an IPv6 set (name + `6`)
3. On a DNS query for a domain matching a list's rules, the answer's IPs are added to that ipset
4. The ipset entry TTL is normalized (minimum 5 minutes, maximum 1 hour)
5. iptables routes traffic to those IPs through the VPN

### iptables setup for VPN routing

```bash
# Create the ipsets (dns-box does this automatically)
sudo ipset create vpn_domains hash:ip timeout 7200
sudo ipset create vpn_domains6 hash:ip family inet6 timeout 7200

# Mark traffic
sudo iptables -t mangle -A OUTPUT -m set --match-set vpn_domains dst -j MARK --set-mark 100
sudo ip6tables -t mangle -A OUTPUT -m set --match-set vpn_domains6 dst -j MARK --set-mark 100

# Policy-based routing
sudo ip rule add fwmark 100 table vpn
sudo ip route add default via 10.8.0.1 dev tun0 table vpn
```

### Verifying ipset contents

```bash
sudo ipset list vpn_domains
sudo ipset list vpn_domains | grep "Number of entries"
```

---

## Ad and Tracker Blocking

dns-box loads hosts-format blocklists and answers queries for listed domains
with `0.0.0.0`.

### Popular blocklists

```json
"blocklist": {
  "urls": [
    "https://blocklistproject.github.io/Lists/tracking.txt",
    "https://blocklistproject.github.io/Lists/ads.txt",
    "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"
  ]
}
```

### Local blocklist

```json
"blocklist": {
  "urls": ["/etc/dns-box/my-blocklist.txt"]
}
```

Blocked domains are logged at `debug` level:

```
DEBUG Blocked domain: tracker.example.com
```

---

## GitHub Backup

### Automatic saving

On every change via the API the configuration is saved to:
1. the local `config.json`
2. the GitHub repository (if `github_backup.enabled: true`)

### File format in GitHub

`hosts_config.json` supports **two formats**:

**New format (recommended)** — with `ipset.lists`:

```json
{
  "ipset_lists": [
    {
      "name": "vpn_domains",
      "enable_ipv6": true,
      "timeout": 7200,
      "rules": {
        "domain": ["rutracker.org", "rutor.is"],
        "domain_suffix": [".youtube.com"]
      }
    }
  ]
}
```

**Legacy format** — with `ipv4name`/`ipv6name`:

```json
{
  "domain": ["example.com", "rutracker.org"],
  "domain_suffix": [".youtube.com", ".googlevideo.com"]
}
```

> **What gets saved to GitHub:**
> - New format (`ipset.lists`) — **all lists** with their rules
> - Legacy format — only the root `rules`
> - The local `config.json` is always saved in full (all sections)

At startup the backup is **merged** into the local config by name (GitHub wins
on conflicts); local-only lists survive and get pushed on the next save.

---

## Caching

### DNS cache

- **Library:** VictoriaMetrics/fastcache
- **Size:** 8 MB (set in code)
- **TTL:** taken from the DNS answer, run through normalization
- **Key:** `domain|query_type` (e.g. `google.com|1` for A records)

### TTL normalization

All TTLs from DNS answers pass through the normalization policy before being
used in the cache and ipsets:

```
TTL ≤ 0     → 3600  (guard against zero/negative values)
TTL < 180   → 900   (short TTLs pinned to 15 minutes)
TTL ≥ 180   → TTL   (used as-is)

Then hard clamps:
effective = max(effective, 300)   → minimum 5 minutes
effective = min(effective, 3600)  → maximum 1 hour
```

**Why:**

| Problem | Fix |
|---------|-----|
| Some domains return TTL = 0 | Records don't expire instantly |
| TTL < 3 minutes (e.g. 30 s) | Cache and ipset live at least 5 minutes |
| TTL > 1 hour (e.g. 86400) | IPs don't linger if the domain moved |

**Examples:**

| Original TTL | After normalization |
|--------------|----------------------|
| 0 | 3600 (1 hour) |
| 30 | 900 → clamp → **300** (5 min) |
| 120 | 900 → clamp → **900** (15 min) |
| 300 | **300** (5 min) |
| 1800 | **1800** (30 min) |
| 86400 | **3600** (1 hour) |

Normalization applies to:
- ✅ **ipset** entries (IPv4 and IPv6)
- ✅ the **DNS cache** (positive answers)
- ✅ the **negative cache** (NXDOMAIN answers)

### Domain cache

- **Library:** VictoriaMetrics/fastcache
- **Size:** 8 MB
- **Contents:** exact domains and suffixes from `rules`
- **Purpose:** fast lookup of whether an IP should go into an ipset

### Negative caching

`NXDOMAIN` answers are cached with the SOA TTL, run through normalization.

---

## Build and Deploy

### Local build

```bash
make local-build
```

### Cross-compiling for ARM

```bash
make arm-build
```

Builds a softfloat Linux ARM binary (routers and embedded devices).

> **Careful:** `make build` and `make all` are not builds but the author's full
> router deployment cycle (see below). For building, use `local-build` or
> `arm-build`.

### Compressing the binary (UPX)

```bash
make pack
```

### Deploying to a router

```bash
make copyToRouter   # scp the binary
make setRights      # chmod +x
make restart        # restart the service
```

> **Note:** configure the `be` SSH host and paths in the Makefile first.

### Full cycle

```bash
make all
```

Runs: `arm-build` → `pack` → `copy` → `copyToRouter` → `setRights` → `restart`

---

## Testing

```bash
make test
# or
go test ./...

# races — parts of the code run in multiple goroutines
go test -race ./...

# one package / one test
go test ./internal/config/ -run TestSaveConfigKeepsListsNotNull -v
```

Tests don't touch the network: blocklists and upstreams are served locally
(`httptest`, a local DNS server on 127.0.0.1), so the suite runs offline and
doesn't depend on third-party blocklist contents.

### Testing the DNS server

```bash
dig @127.0.0.1 -p 953 example.com
dig @127.0.0.1 -p 953 AAAA google.com

# Blocking check — expect 0.0.0.0
dig @127.0.0.1 -p 953 blocked-tracker.com
```

### Testing the API

```bash
curl -X POST http://localhost:8090/domains -d "newdomain.com"
dig @127.0.0.1 -p 953 newdomain.com
sudo ipset list vpn_domains   # the IP should appear here
```

---

## Project Layout

```
dns-box/
├── cmd/
│   └── dns-box/
│       └── main.go              # Entry point, init, graceful shutdown
├── internal/
│   ├── api/
│   │   ├── server.go            # HTTP API server + bearer auth
│   │   └── handlers.go          # Endpoint handlers
│   ├── blocklist/
│   │   └── blocklist.go         # Blocklist download and refresh
│   ├── cache/
│   │   ├── domain_cache.go      # Domain/suffix cache (fastcache)
│   │   └── dns_cache.go         # DNS answer cache (fastcache)
│   ├── config/
│   │   └── config.go            # Config load/save/mutations, GitHub merge
│   ├── dns/
│   │   ├── server.go            # DNS server (UDP + TCP)
│   │   ├── handler.go           # Resolving, two-wave upstream race, ipset
│   │   ├── hosts.go             # /etc/hosts-style local overrides
│   │   └── ratelimit.go         # Per-client token bucket
│   ├── github/
│   │   └── client.go            # GitHub API client (load/save, 409 retry)
│   ├── ipset/
│   │   ├── manager.go           # Manager interface (fake in tests)
│   │   ├── ipset.go             # Linux ipset wrapper (//go:build linux)
│   │   └── ipset_stub.go        # No-op stub (//go:build !linux)
│   └── ipsetstate/
│       └── ipsetstate.go        # ipset state mirror for reboot recovery
├── deploy/
│   └── dns-box.init.d           # procd script for OpenWrt
├── install.sh                   # One-line installer for routers
├── .github/workflows/test.yml   # CI: tests + automated release
├── config.json                  # Example configuration
├── Makefile                     # Build and deploy
├── go.mod / go.sum              # Go modules
├── README.md                    # This file (English)
└── README.ru.md                 # Russian documentation
```

> Any new `ipset` method must be added to both `ipset.go` and
> `ipset_stub.go` — otherwise non-Linux builds break.

---

## Logging

| Level | Description |
|-------|-------------|
| `trace` | Detailed cache and resolver internals |
| `debug` | Rule matching, ipset additions, blocklist hits |
| `info` | Server startup, blocklist updates, GitHub operations |
| `warn` | Upstream failures, config issues |
| `error` | Critical failures |

Example:

```
INFO[0000] dns-box v1.0.15 (commit a1b2c3d, built 2026-01-01T00:00:00Z, go1.27.1 linux/arm)
INFO[0000] DNS server started on 127.0.0.1:953
INFO[0000] Local config has no domains. Attempting to load from GitHub...
INFO[0000] Loaded 15 domains and 8 suffixes from GitHub
INFO[0000] Starting blocklist service...
INFO[0000] Updating blocklists...
INFO[0001] Loaded 12543 domains from https://blocklistproject.github.io/Lists/tracking.txt
INFO[0001] Blocklists updated successfully. Total domains: 12543
DEBUG[0002] Processing question: www.youtube.com.
DEBUG[0002] Domain matches suffix config, process: www.youtube.com (suffix: .youtube.com)
DEBUG[0002] Added IPv4 address 142.250.74.46 with timeout 300 for domain: www.youtube.com., to ipset: vpn_domains
```

---

## License

MIT — see [LICENSE](LICENSE).
