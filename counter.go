package main

// Reading the byte counters.
//
// The kernel keeps a running total of bytes in and out per interface. Reading
// it costs nothing and needs no privilege. Go's net.Interfaces() exposes flags
// and MTU but no byte counts, and the sysctl route means hand-parsing
// if_msghdr2 with hardcoded offsets, so we take the counters from netstat.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Counters holds one reading of the interface byte counters.
type Counters struct {
	RX uint64
	TX uint64
}

// parseCounters reads `netstat -ibn -I en0`:
//
//	Name  Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
//	en0   1500  <Link#5>    00:00:5e:00:53:0a    12345     0   12345678    54321     0    8765432     0
//	en0   1500  203.0.113/24  203.0.113.42       12345     -   12345678    54321     -    8765432     -
//
// Locating the Ibytes and Obytes columns by header rather than by position
// keeps this working when netstat adds or reorders a column.
func parseCounters(out string) (Counters, error) {
	lines := strings.Split(out, "\n")
	defer func() {}()

	rxCol, txCol := -1, -1
	headerAt := -1
	for i, line := range lines {
		for j, field := range strings.Fields(line) {
			switch field {
			case "Ibytes":
				rxCol = j
			case "Obytes":
				txCol = j
			}
		}
		if rxCol >= 0 && txCol >= 0 {
			headerAt = i
			break
		}
	}
	if headerAt < 0 {
		return Counters{}, errors.New("netstat output has no Ibytes/Obytes header")
	}

	for _, row := range lines[headerAt+1:] {
		// Address rows repeat the same counters per address family; only the
		// <Link#> row is the interface total.
		if !strings.Contains(row, "<Link#") {
			continue
		}
		fields := strings.Fields(row)
		if len(fields) <= rxCol || len(fields) <= txCol {
			continue
		}
		rx, err := strconv.ParseUint(fields[rxCol], 10, 64)
		if err != nil {
			return Counters{}, fmt.Errorf("Ibytes %q: %w", fields[rxCol], err)
		}
		tx, err := strconv.ParseUint(fields[txCol], 10, 64)
		if err != nil {
			return Counters{}, fmt.Errorf("Obytes %q: %w", fields[txCol], err)
		}
		return Counters{RX: rx, TX: tx}, nil
	}
	return Counters{}, errors.New("netstat output has no link row")
}
