# wifimeter

Per-network bandwidth accounting for macOS, as a single static binary that
never asks for Location Services, sudo, or packet capture.

Commercial tools that show per-network usage all request Location Services.
They have to: macOS has withheld the SSID from unentitled processes since
14.4, and a bare `launchd` agent can never obtain it because there is no UI to
present the prompt from. `wifimeter` takes a different route and identifies the
network by its **gateway** instead: IP, subnet mask, and gateway MAC.

## Install

```sh
git clone https://github.com/mr687/wifimeter
cd wifimeter
make build      # -> ~/bin/wifimeter
make install    # build, then write the launchd agent and load it
```

## Use

```sh
wifimeter doctor                 # verify every read works, no sudo
wifimeter report --days 7        # per-network totals
wifimeter label "Phone hotspot"  # name the network you are on
wifimeter report
```

```
Network                       Today                7 days              All time
─────────────  ────────────────────  ────────────────────  ────────────────────
Home               ↓ 39 GB ↑ 2.9 GB      ↓ 196 GB ↑ 13 GB      ↓ 1.0 TB ↑ 82 GB
Phone hotspot     ↓ 1.7 GB ↑ 200 MB     ↓ 5.8 GB ↑ 744 MB     ↓ 5.8 GB ↑ 744 MB
(discarded: 0.3% — 14 samples, 2 sleep/wake, 1 network-switch)
```

`doctor` reads nothing from the database and is the first thing to run when
something looks wrong.

To stop collecting:

```sh
wifimeter uninstall           # remove agent and binary, keep the database
wifimeter uninstall --wipe    # also delete the database
```

## How it works

| Read | Source | Privilege |
|---|---|---|
| Byte counters | `netstat -ibn -I <iface>` | none |
| Link status, netmask | `ifconfig <iface>` | none |
| Wi-Fi gateway | `route -n get default -ifscope <iface>` | none |
| Gateway MAC | `arp -n <gateway>` | none |
| DHCP vendor class | `ipconfig getsummary <iface>` | none |

`-ifscope` is what keeps a VPN from being metered instead of the network: with
Cloudflare WARP running, the *system* default route points at `utun7`, and only
the scoped lookup returns the actual Wi-Fi gateway.

Only the physical Wi-Fi interface is read. Tunnels are encapsulated inside its
frames, so adding `utun*` would double count; `awdl0`/`llw0` share the radio
without being network usage.

Never used: `tcpdump`, CoreWLAN, `system_profiler`, or a Network Extension.

Samples are taken every 10s and stored in SQLite. A sample is discarded, not
guessed at, when the link is down, the gateway is unknown, the network changed,
the counters went backwards, or more than 50s elapsed since the last one. A
dropped sample costs ten seconds; a phantom multi-gigabyte sample would ruin
the day's totals.

## Limits

- **No true SSID.** Two SSIDs behind the same gateway (2.4 and 5 GHz bands,
  guest vs main) collapse to one fingerprint, since they share the gateway MAC.
- **No per-app breakdown.** That needs `nettop` or a Network Extension filter.
- **10s granularity.** A network joined and left inside one interval may be
  missed.
- **Sleep detection is a heuristic**, based on the gap between samples.
- **macOS only.** Every command above is BSD-flavored. `go build` works
  anywhere; the tool does not.

## Privacy

The database stores, per network: gateway IP, subnet, gateway MAC, DHCP vendor
class, and timestamped byte deltas. Gateway MACs can identify places you have
been, so treat `usage.db` as sensitive. It lives at
`~/Library/Application Support/WifiMeter/usage.db` and never leaves the machine.

## Development

```sh
make check      # gofmt, vet, test
make test
```

Parsing is covered by golden tests over captured command output in
`testdata/`. Run `go test -update` to rewrite the goldens after an intentional
change.

Builds with `CGO_ENABLED=0` and links only `libSystem` and `libresolv`, so the
binary has no Homebrew dependency. SQLite comes from `modernc.org/sqlite`,
which is pure Go.

## License

MIT
