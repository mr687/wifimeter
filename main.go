package main

// wifimeter attributes bytes transferred to the Wi-Fi network you are on,
// without needing Location Services, sudo, or packet capture.

import (
	"flag"
	"fmt"
	"log"
	"os"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage: wifimeter <command>

  run                      sample in a loop (launchd KeepAlive)
  report [--days 7]        per-network totals
  label [--fp KEY] "name"  name a network
  doctor                   zero-permission self-check
  install                  write the launchd agent and load it
  uninstall [--wipe]       unload and remove the agent and binary

  -iface applies to run, doctor, and label; it defaults to the detected
  Wi-Fi device and falls back to en0.
`)
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:])
	case "label":
		err = labelCmd(os.Args[2:])
	case "doctor":
		err = doctorCmd(os.Args[2:])
	case "install":
		err = installCmd()
	case "uninstall":
		err = uninstallCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	interval := fs.Duration("interval", DefaultInterval, "sampling interval")
	ifaceFlag := fs.String("iface", "", "interface to meter; defaults to the detected Wi-Fi device")
	if err := fs.Parse(args); err != nil {
		return err
	}

	lock, err := acquireDaemonLock()
	if err != nil {
		return err
	}
	defer lock.Close()

	path, err := DefaultDBPath()
	if err != nil {
		return err
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()

	return runDaemon(store, *interval, ResolveIface(*ifaceFlag))
}
