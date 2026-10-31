package main

// cgroup.go -- RESOURCE ALLOCATION via cgroups v2 (CPU + memory), kept minimal.
//
// A cgroup ("control group") is a Linux kernel feature that caps how much CPU
// and memory a group of processes may use. This is exactly how Docker enforces
// `--memory` and `--cpus`. On cgroups v2 (the default on Amazon Linux 2023),
// everything is a simple file tree under /sys/fs/cgroup:
//
//   /sys/fs/cgroup/minicon/<name>/
//       memory.max      <- write "134217728" to cap memory at 128 MiB
//       cpu.max         <- write "50000 100000" to cap CPU at 0.5 cores
//       cgroup.procs    <- write a PID to MOVE that process into this group
//
// Flow: parent creates the dir + writes the limits, starts the child, then
// writes the child's PID into cgroup.procs. The kernel enforces the caps on
// the child and everything it spawns. On exit we remove the dir.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const cgroupRoot = "/sys/fs/cgroup"

// cgroupLimits holds the parsed, kernel-ready limit strings (empty = no limit).
type cgroupLimits struct {
	dir       string // the cgroup directory we created, "" if none
	memoryMax string // bytes as string, e.g. "134217728"
	cpuMax    string // "<quota> <period>", e.g. "50000 100000"
}

// cpuPeriod is the scheduling window in microseconds. 100000us = 100ms is the
// kernel default. quota/period = fraction of one CPU core.
const cpuPeriod = 100000

// parseMemory turns "128m", "1g", "512k", or a raw byte count into bytes.
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad memory value %q", s)
	}
	return n * mult, nil
}

// buildLimits converts the user flags (--memory, --cpus) into kernel strings.
func buildLimits(memory string, cpus float64) (cgroupLimits, error) {
	var l cgroupLimits
	if memory != "" {
		b, err := parseMemory(memory)
		if err != nil {
			return l, err
		}
		if b > 0 {
			l.memoryMax = strconv.FormatInt(b, 10)
		}
	}
	if cpus > 0 {
		quota := int64(cpus * float64(cpuPeriod))
		if quota < 1000 {
			quota = 1000 // floor so a tiny value isn't effectively zero
		}
		l.cpuMax = fmt.Sprintf("%d %d", quota, cpuPeriod)
	}
	return l, nil
}

// active reports whether any limit was requested.
func (l *cgroupLimits) active() bool { return l.memoryMax != "" || l.cpuMax != "" }

// create makes the cgroup directory and writes the limit files.
// It must run as root and requires cgroups v2 mounted at /sys/fs/cgroup.
func (l *cgroupLimits) create(name string) error {
	if !l.active() {
		return nil
	}
	// Confirm cgroups v2 by the presence of cgroup.controllers at the root.
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return fmt.Errorf("cgroups v2 not found at %s (limits need v2): %w", cgroupRoot, err)
	}

	l.dir = filepath.Join(cgroupRoot, "minicon", name)
	if err := os.MkdirAll(l.dir, 0755); err != nil {
		return fmt.Errorf("creating cgroup dir: %w", err)
	}

	if l.memoryMax != "" {
		if err := writeFile(filepath.Join(l.dir, "memory.max"), l.memoryMax); err != nil {
			return fmt.Errorf("setting memory.max: %w", err)
		}
	}
	if l.cpuMax != "" {
		if err := writeFile(filepath.Join(l.dir, "cpu.max"), l.cpuMax); err != nil {
			// cpu controller may not be delegated; warn rather than fail hard.
			fmt.Fprintf(os.Stderr, "warning: could not set cpu.max (%v)\n", err)
		}
	}
	return nil
}

// (The child joins the cgroup itself by writing its PID into cgroup.procs --
// see cmdChild in run.go -- which avoids a race before the app execs.)

// cleanup removes the cgroup directory after the container exits.
func (l *cgroupLimits) cleanup() {
	if l.dir == "" {
		return
	}
	// rmdir only succeeds once the group is empty, which it is after exit.
	_ = os.Remove(l.dir)
}

func writeFile(path, val string) error {
	return os.WriteFile(path, []byte(val), 0644)
}
