# wifimeter

Bandwidth accounting per Wi-Fi network, for macOS. One static binary that asks
for nothing: no Location Services, no sudo, no packet capture.

Every commercial tool that shows per-network usage asks for Location Services,
and they have no choice in the matter. macOS has withheld the SSID from
unentitled processes since 14.4, and a `launchd` agent can never get it because
there is no window to present the prompt from. So `wifimeter` identifies the
network by its gateway instead: IP, subnet mask, and gateway MAC.

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

`doctor` touches no stored data, which makes it the thing to run first when a
number looks wrong.

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

The `-ifscope` flag is what keeps a VPN from being metered in place of the
network. Any active tunnel moves the system default route onto a `utun`
interface, and only the scoped lookup still returns the Wi-Fi gateway you are
actually on.

Only the physical Wi-Fi interface gets read. Tunnel traffic is encapsulated
inside its frames, so adding `utun*` would count it twice. The `awdl0` and
`llw0` interfaces share the radio for AirDrop and carry nothing that counts as
network usage.

`tcpdump`, CoreWLAN, `system_profiler`, and Network Extension filters are all
unused.

Samples land in SQLite every 10 seconds. One gets discarded rather than guessed
at when the link is down, the gateway is unknown, the network changed, the
counters moved backwards, or more than 50 seconds passed since the previous
sample. Losing a sample costs ten seconds. Accepting a bogus one would put
gigabytes into a day's totals that nobody transferred.

## Limits

- **No true SSID.** Two SSIDs behind one gateway collapse into a single
  fingerprint, because they share the gateway MAC. That includes the 2.4 and
  5 GHz bands of the same router, and guest networks.
- **No per-app breakdown.** That needs `nettop` or a Network Extension filter.
- **10s granularity.** A network joined and left inside one interval can be
  missed entirely.
- **Sleep detection is a heuristic**, inferred from the gap between samples.
- **macOS only.** Each command above is BSD-flavored. The code compiles
  anywhere Go runs; the tool does not work anywhere else.

## Privacy

Each network's row holds its gateway IP, subnet, gateway MAC, DHCP vendor
class, and timestamped byte deltas. Gateway MACs can pin down places you have
been, so treat `usage.db` as sensitive. It stays at
`~/Library/Application Support/WifiMeter/usage.db`.

## Development

```sh
make check      # gofmt, vet, test
make test
```

Golden tests over captured command output cover the parsing, in `testdata/`.
After an intentional change, `go test -update` rewrites the goldens.

The build runs with `CGO_ENABLED=0` and links only `libSystem` and
`libresolv`, so the binary carries no Homebrew dependency. SQLite comes from
`modernc.org/sqlite`, which is pure Go.

## License

MIT
