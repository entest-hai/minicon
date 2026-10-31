package main

// run.go -- COMPONENT 2: the runtime.
//
// `minicon run --image hello.img` does what a container runtime does, minimally:
//
//   1. Extract the image's rootfs into a fresh temp directory.
//   2. Re-exec THIS program inside new Linux namespaces (UTS, PID, mount, IPC)
//      using the "child" hidden subcommand. New namespaces give the app its own
//      hostname, its own PID 1, and its own mount table.
//   3. In the child: set hostname, chroot into the rootfs, mount a private
//      /proc, then exec the app's entrypoint as PID 1 of its namespace.
//
// This is the core idea behind Docker/containerd: namespaces + chroot + a
// private /proc. We deliberately keep the host network namespace so the web
// app's port is reachable from the EC2 host (like `docker run --net=host`).
//
// Requires Linux + root (namespaces and chroot are privileged). That is why
// you run it with sudo on Amazon Linux 2023.

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// cmdRun is the parent side: extract image, then re-exec into namespaces.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	image := fs.String("image", "", "path to the image file (required)")
	hostname := fs.String("hostname", "minicon", "hostname inside the container")
	keep := fs.Bool("keep", false, "keep the extracted rootfs after exit (for debugging)")
	memory := fs.String("memory", "", "memory limit, e.g. 128m or 1g (cgroups v2)")
	cpus := fs.Float64("cpus", 0, "CPU limit in cores, e.g. 0.5 or 2 (cgroups v2)")
	rootless := fs.Bool("rootless", false, "run without root via a user namespace (maps you to uid 0 inside)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *image == "" {
		return fmt.Errorf("--image is required")
	}

	// Build the resource limits from the flags (empty/zero = unlimited).
	limits, err := buildLimits(*memory, *cpus)
	if err != nil {
		return err
	}

	// Extract the rootfs to a working directory.
	work, err := os.MkdirTemp("", "minicon-*")
	if err != nil {
		return err
	}
	rootfs := filepath.Join(work, "rootfs")
	if err := os.MkdirAll(rootfs, 0755); err != nil {
		return err
	}

	m, err := extractImage(*image, rootfs)
	if err != nil {
		return fmt.Errorf("extracting image: %w", err)
	}
	fmt.Printf("▶  running %s  (entrypoint %v, port %s)\n", m.Name, m.Entrypoint, m.Port)
	fmt.Printf("   rootfs: %s\n", rootfs)
	if limits.active() {
		fmt.Printf("   limits: memory=%s cpu=%s\n",
			orNone(*memory), orNone(cpusStr(*cpus)))
	}

	// Create the cgroup (if any limits were asked for) BEFORE starting the
	// child, so we can drop its PID into the group the moment it exists.
	cgName := fmt.Sprintf("%d", os.Getpid())
	if err := limits.create(cgName); err != nil {
		return fmt.Errorf("cgroup setup: %w", err)
	}
	defer limits.cleanup()

	// Re-exec ourselves as the "child" inside new namespaces.
	// /proc/self/exe is the current binary; this is the canonical Go trick.
	// We pass the cgroup dir so the child can join it itself (no race before exec).
	childArgs := []string{"child", rootfs, *hostname, limits.dir, strings.Join(m.Entrypoint, " ")}
	cmd := exec.Command("/proc/self/exe", childArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), m.Env...)

	// The namespaces that isolate the child:
	//   NEWUTS   -> own hostname
	//   NEWPID   -> own process-id space (app becomes PID 1)
	//   NEWNS    -> own mount table (so our /proc mount stays private)
	//   NEWIPC   -> own System V IPC / message queues
	// We intentionally do NOT add NEWNET, so the app shares host networking
	// and its port is reachable on the EC2's IP.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:   syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC,
		Unshareflags: syscall.CLONE_NEWNS, // don't let mounts leak back to host
	}

	// Rootless mode: add a USER namespace and map the current uid/gid to 0
	// inside it. This grants the capabilities needed for chroot/sethostname
	// WITHOUT real root -- the same trick rootless Docker/Podman use. Handy on
	// a locked-down host where sudo is unavailable.
	if *rootless {
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
		// Under a user namespace the kernel rejects a separate Unshareflags
		// mount-namespace request (EPERM). The mount namespace is already
		// created by CLONE_NEWNS in Cloneflags above, so clear Unshareflags.
		cmd.SysProcAttr.Unshareflags = 0
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		}
		fmt.Printf("   mode:   rootless (user namespace, you→uid 0 inside)\n")
	}

	// Start (not Run) so we get the child's PID and can place it in the
	// cgroup before it does any real work. Then wait for it to finish.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting child: %w", err)
	}
	runErr := cmd.Wait()

	if !*keep {
		_ = os.RemoveAll(work)
	} else {
		fmt.Printf("   (kept rootfs at %s)\n", work)
	}
	return runErr
}

// orNone renders an empty limit as "none" for the status line.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// cpusStr formats the --cpus value, or "" if unset.
func cpusStr(c float64) string {
	if c <= 0 {
		return ""
	}
	return fmt.Sprintf("%g cores", c)
}

// cmdChild runs INSIDE the new namespaces. Arguments:
//   args[0] = rootfs path
//   args[1] = hostname
//   args[2] = cgroup dir ("" if no limits)
//   args[3] = entrypoint (space-joined)
func cmdChild(args []string) error {
	if len(args) < 4 {
		return fmt.Errorf("child: expected rootfs, hostname, cgroupDir, entrypoint")
	}
	rootfs := args[0]
	hostname := args[1]
	cgroupDir := args[2]
	entry := strings.Fields(args[3])
	if len(entry) == 0 {
		return fmt.Errorf("child: empty entrypoint")
	}

	// 0. Join the cgroup (if any) by writing our own PID -- before chroot,
	//    while /sys/fs/cgroup is still reachable. The kernel then enforces the
	//    CPU/memory caps on us and everything we exec afterwards.
	if cgroupDir != "" {
		if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"),
			[]byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not join cgroup: %v\n", err)
		}
	}

	// 1. Set the container hostname (uses the new UTS namespace).
	if err := syscall.Sethostname([]byte(hostname)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}

	// 2. Enter the rootfs. We use chroot + chdir for simplicity. (pivot_root
	//    is stricter but chroot is enough to show the concept.)
	if err := syscall.Chroot(rootfs); err != nil {
		return fmt.Errorf("chroot (are you root?): %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir: %w", err)
	}

	// 3. Mount a private /proc so tools like `ps` see only our namespace PIDs.
	_ = os.MkdirAll("/proc", 0555)
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		// Non-fatal: the app can still run without /proc, just warn.
		fmt.Fprintf(os.Stderr, "warning: mount /proc failed: %v\n", err)
	}
	// A minimal /tmp and /dev could be added here; kept out for brevity.

	fmt.Fprintf(os.Stderr, "   [child] hostname=%s pid=%d entry=%v\n", hostname, os.Getpid(), entry)

	// 4. Exec the app. This REPLACES the child process image, so the app
	//    becomes PID 1 of its PID namespace -- exactly like a container.
	bin := entry[0]
	if err := syscall.Exec(bin, entry, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", bin, err)
	}
	return nil
}
