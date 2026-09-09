package main

// install and uninstall: write the launchd agent and load it.
//
// A user LaunchAgent rather than /Library/LaunchDaemons, so it needs no root,
// starts after login, and inherits the user's own (zero) permissions.

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const agentLabel = "com.github.mr687.wifimeter"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"), nil
}

func installCmd() error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	// launchd does not expand ~ or follow symlinks reliably.
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}

	path, err := plistPath()
	if err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>              <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>run</string>
    </array>
    <key>KeepAlive</key>          <true/>
    <key>RunAtLoad</key>          <true/>
    <key>LowPriorityIO</key>      <true/>
    <key>ProcessType</key>        <string>Background</string>
    <key>StandardOutPath</key>    <string>/tmp/wifimeter.log</string>
    <key>StandardErrorPath</key>  <string>/tmp/wifimeter.log</string>
</dict>
</plist>
`, agentLabel, bin)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}

	if out, err := exec.Command("launchctl", "load", "-w", path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %v: %s", err, out)
	}
	fmt.Printf("installed %s -> %s\n", path, bin)
	return nil
}

// uninstall removes the agent and the installed binary. The database and its
// WAL sidecars are kept unless --wipe is passed.
func uninstallCmd(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	wipe := fs.Bool("wipe", false, "also delete the database and collected history")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path, err := plistPath()
	if err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "unload", "-w", path).CombinedOutput(); err != nil {
		fmt.Printf("launchctl unload: %v: %s\n", err, out)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("removed %s\n", path)

	data, err := dataDir()
	if err != nil {
		return err
	}
	if !*wipe {
		fmt.Printf("kept %s (pass --wipe to delete)\n", data)
	} else if err := os.RemoveAll(data); err != nil {
		return err
	} else {
		fmt.Printf("removed %s\n", data)
	}

	// Unconditional: the binary is rebuildable with make build, the data is not.
	bin, err := binPath()
	if err != nil {
		return err
	}
	if err := os.Remove(bin); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("removed %s\n", bin)
	return nil
}

// dataDir is the directory holding usage.db and its WAL sidecars.
func dataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "WifiMeter"), nil
}

// binPath is where the Makefile installs the binary.
func binPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "bin", "wifimeter"), nil
}
