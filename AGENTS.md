# AGENTS.md

Bandwidth accounting per Wi-Fi network, for macOS. One static binary, one
`package main`.

The design constraint that explains most of the code: macOS has withheld the
SSID from unentitled processes since 14.4, and a `launchd` agent can never get
Location Services because there is no window to present the prompt from. So
networks are identified by **gateway fingerprint** (IP + subnet + MAC), never by
SSID, and every read must work with no privilege.

## Commands

```sh
make check          # gofmt -w . && go vet ./... && go test ./...
make test           # go test ./...
make build          # -> ~/bin/wifimeter
make install        # build, then write the launchd agent and load it
```

`make check` **rewrites files** (`fmt` is `gofmt -w .`). Run `gofmt -l .` first
if you want to see whether anything is unformatted.

Build is `CGO_ENABLED=0` with `-ldflags="-s -w -X main.version=$(VERSION)"`.
`VERSION` comes from `git describe`, so a plain `go build` reports `dev`.

Manual smoke test against a throwaway database — point `HOME` at a temp dir so
the real `usage.db` is never touched:

```sh
CGO_ENABLED=0 go build -o /tmp/wifimeter .
HOME=/tmp/wfhome /tmp/wifimeter doctor
```

## macOS only, and only the Wi-Fi interface

`net_darwin.go` is excluded on other platforms by filename, so the package does
not compile off darwin. CI runs on `macos-latest` for that reason. Every command
the sampler shells out to (`netstat`, `route`, `arp`, `ifconfig`, `ipconfig`,
`networksetup`) is BSD-flavored.

Only the physical Wi-Fi interface is metered. Tunnel traffic is encapsulated
inside its frames, so adding `utun*` would double count; `awdl0` and `llw0`
share the radio for AirDrop and carry nothing billable.

## Invariants

These are load-bearing. Breaking any of them is a correctness bug, not a style
choice.

1. **No privilege, ever.** No sudo, no Location Services, no packet capture.
   `tcpdump`, CoreWLAN, `system_profiler`, and Network Extension filters are
   deliberately unused.
2. **Fingerprint by gateway, not SSID.** `route` must keep `-ifscope`, or an
   active VPN moves the default route onto a `utun` interface and the tunnel
   gets metered instead of the Wi-Fi network.
3. **The delta gate discards rather than guesses.** Counters are cumulative and
   monotonic only while the link stays up. Anything that breaks that — reset,
   switch, sleep, first sample after start — is dropped. A lost sample costs ten
   seconds; an accepted bogus one puts gigabytes into a day nobody transferred.
4. **Discarded samples are stored**, with zero deltas and a reason, so `doctor`
   can report a discard rate. A silently lossy collector must not look like a
   clean one.
5. **One sampler at a time.** The `flock` lives beside the database, not in
   `/tmp` (which is cleared periodically). Two writers lose samples to
   `SQLITE_BUSY` and duplicate the rest.
6. **CGO stays off.** SQLite comes from `modernc.org/sqlite` (pure Go) so the
   binary carries no C toolchain dependency. Do not add a dependency that needs
   cgo.

## Layout

| File | Role |
|---|---|
| `net_darwin.go` | Fingerprinting. Pure string→value parsers over captured command output. |
| `counter.go` | `netstat` byte counters, columns located by header name, not index. |
| `sampler.go` | Runs the reads; `runner` func seam lets tests inject fixtures. |
| `delta.go` | The discard gate. Where correctness lives. |
| `store.go` | SQLite: schema, WAL, `busy_timeout`, rollup queries. |
| `report.go` / `report_daily.go` | Rendering. Pure functions over `[]Sample`. |
| `cli.go` / `doctor.go` | `report`, `label`, `doctor`. |
| `install.go` / `lock.go` | launchd agent, single-instance lock. |

## Tests

Golden tests over captured command output: fixtures in `testdata/inputs/`,
expected output in `testdata/golden/`. After an intentional change:

```sh
go test -update
```

Tests build fixtures in memory and never open the real database. Keep it that
way. Parsing and rendering are pure functions specifically so they can be tested
without a database or a live network.

## Data and privacy

`usage.db` sits at `~/Library/Application Support/WifiMeter/usage.db`. It holds
gateway IPs, subnets, gateway MACs, and timestamped byte deltas. Gateway MACs
pin down places someone has been — treat it as sensitive, and never copy it into
a test fixture or an issue.

## Commits, PRs, releases

Conventional Commits, imperative, lowercase, no trailing period:
`feat(report): …`, `fix(build): …`, `docs: …`. Scopes seen in history: `build`,
`report`.

PRs are small and scoped to one concern, squash-merged through the GitHub UI
(branches auto-delete on merge). Delete the local branch after.

Release by pushing a tag:

git tag -a vX.Y.Z -m "wifimeter vX.Y.Z" <sha>
git push origin vX.Y.Z
