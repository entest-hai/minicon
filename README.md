# minicon — a minimal container tool in Go

A teaching-sized clone of Docker's two core ideas, written from scratch with
only the Go standard library:

| Component | Command | What it does |
|-----------|---------|--------------|
| **1. Packager** (build-time) | `minicon package` | Bundles an app binary + libs/assets into one portable **image file** (`.img`). |
| **2. Runtime** (run-time) | `minicon run` | Extracts an image and runs the app **isolated** in Linux namespaces + chroot. |

The image format is just a **gzipped tar** holding a `manifest.json`
(entrypoint, env, port) and a `rootfs/` tree — the same shape as a real OCI
image, stripped to essentials.

> 📐 **Want the architecture, specs, and detailed design with ASCII diagrams?**
> See [`DESIGN.md`](./DESIGN.md). This README is the quick-start; DESIGN.md
> explains *how* and *why* it's built the way it is.

---

## Files

```
minicon/
├── app/main.go              # the hello web app (the payload)
├── cmd/minicon/
│   ├── main.go              # CLI dispatch (package / run / child)
│   ├── image.go             # the image format: tar+gzip, manifest, extract
│   ├── package.go           # COMPONENT 1 — packager
│   └── run.go               # COMPONENT 2 — runtime (namespaces + chroot)
├── go.mod
├── demo.sh                  # build → package → run walkthrough
└── README.md
```

---

## Prerequisites on Amazon Linux 2023

```bash
sudo dnf install -y golang       # or download from go.dev
go version                       # confirm it works
```

`minicon run` needs **Linux**, and **root** (via `sudo`) by default — namespaces
+ chroot are privileged. If you cannot use `sudo`, add `--rootless` to run via a
**user namespace** instead (maps you to uid 0 inside; the same trick rootless
Docker/Podman use). `minicon package` runs anywhere.

```bash
sudo ./minicon run --image hello.img        # normal (root)
./minicon run --image hello.img --rootless  # no sudo needed
```

> In `--rootless` mode, mounting a private `/proc` and writing cgroup limits may
> be denied by the kernel (they need more than a single-uid map) — minicon warns
> and continues; the app still runs. Use real root for full `/proc` + cgroups.

> Note: your EC2 reports `aarch64` (Graviton). Building on the box itself gives a
> native arm64 binary automatically. To build on an x86 laptop *for* the arm64
> EC2, cross-compile: `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ...`.

---

## Quick start (on one machine)

```bash
cd minicon

# build the tool
go build -o minicon ./cmd/minicon

# build the hello app as a STATIC binary (no shared-lib dependency)
CGO_ENABLED=0 go build -o hello ./app

# COMPONENT 1 — package it into an image
./minicon package \
  --name hello:1.0 \
  --bin ./hello \
  --entrypoint /hello \
  --port 8080 \
  --out hello.img

# COMPONENT 2 — run it (root needed)
sudo ./minicon run --image hello.img &
sleep 1
curl -s http://localhost:8080 | head
```

You should see the hello HTML, and the page shows the container's **own
hostname** (`minicon`) and the app running as **PID 1** — proof it's in its own
namespaces.

Or just run `bash demo.sh`.

---

## Deploy to ANOTHER EC2 (the point of the exercise)

The image is self-contained, so you copy two files and run:

```bash
# from the build host
scp minicon hello.img ec2-user@<OTHER_EC2_IP>:~/

# on the other EC2
ssh ec2-user@<OTHER_EC2_IP>
sudo ./minicon run --image hello.img &
curl http://localhost:8080
```

No Go, no Docker needed on the target — just the `minicon` binary and the image.
(Both hosts must be the same CPU arch, e.g. both arm64, unless you cross-compiled.)

To reach it from your laptop, open port 8080 in the EC2 security group, or
SSH-tunnel: `ssh -L 8080:localhost:8080 ec2-user@<OTHER_EC2_IP>`.

---

## How the runtime isolates the app (the interesting bit)

`minicon run` re-executes **itself** with a hidden `child` subcommand inside new
namespaces — the standard Go pattern, because Go can't `fork()` safely:

1. **Parent** (`run.go: cmdRun`): extracts the rootfs, then launches
   `/proc/self/exe child ...` with `SysProcAttr.Cloneflags` set to:
   - `CLONE_NEWUTS` — its own **hostname**
   - `CLONE_NEWPID` — its own **PID space** (app becomes PID 1)
   - `CLONE_NEWNS`  — its own **mount table**
   - `CLONE_NEWIPC` — its own IPC
   (Network namespace is intentionally **not** isolated, so the web port is
   reachable on the host — like `docker run --net=host`.)
2. **Child** (`run.go: cmdChild`): `sethostname` → `chroot` into the rootfs →
   mount a private `/proc` → `exec` the entrypoint. After `exec`, the app *is*
   the container process.

That's the whole trick real runtimes use, plus cgroups for limits (below),
minus pivot_root, seccomp, and a network namespace with veth pairs.

---

## Resource limits — CPU & memory (cgroups v2)

Like `docker run --memory --cpus`, minicon can cap how much RAM and CPU the
container may use, using **cgroups v2** (the default on Amazon Linux 2023):

```bash
# cap at 128 MiB of memory and half of one CPU core
sudo ./minicon run --image hello.img --memory 128m --cpus 0.5
```

- `--memory` accepts `128m`, `1g`, `512k`, or raw bytes → written to `memory.max`.
- `--cpus` is in cores (`0.5` = half a core, `2` = two cores) → written to
  `cpu.max` as `"<quota> <period>"`.
- With no flags, the container is unlimited (nothing is created).

How it works (`cgroup.go` + `run.go`): the parent creates
`/sys/fs/cgroup/minicon/<pid>/`, writes the limit files, and the child writes
its own PID into `cgroup.procs` before `exec` — so the kernel enforces the caps
on the app and anything it spawns. The group is removed on exit.

Verify a cap is live while it runs:

```bash
cat /sys/fs/cgroup/minicon/*/memory.max   # e.g. 134217728  (128 MiB)
cat /sys/fs/cgroup/minicon/*/cpu.max      # e.g. 50000 100000
```

---

## What this deliberately leaves out (so you can explain it)

- **network namespace / veth** — we share host net for simplicity.
- **pivot_root** — we use `chroot`, which is weaker but clearer.
- **image layers / registry** — one flat tar instead of stacked layers + a pull.
- **seccomp / capabilities** — no syscall filtering.

(CPU/memory limits via cgroups v2 **are** implemented — see above.)
Each remaining gap is a natural "future work" talking point for the report.
