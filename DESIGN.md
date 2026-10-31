# minicon — Architecture & Design

A design walkthrough of `minicon`, a minimal container tool in Go. Read this
top-to-bottom to understand *what* it is, *how* it is shaped, and *why* each
piece exists. All diagrams are ASCII so they render in any terminal or editor.

- Quick start / commands → see `README.md`
- This doc → architecture, specs, high-level design, detailed design.

---

## 1. What problem it solves

A container does two jobs:

1. **Package** an app + everything it needs into one portable artifact (image).
2. **Run** that artifact isolated from the host and from other apps.

`minicon` implements exactly those two jobs — nothing else — so the mechanism
is readable. Real Docker adds layers, a registry, cgroups, seccomp, and a full
network stack on top of the *same* core idea.

```
   DOCKER                               MINICON (this project)
   ┌──────────────────────────┐        ┌──────────────────────────┐
   │ docker build  → image     │        │ minicon package → .img    │
   │ registry push/pull        │   ≈    │ scp the .img file         │
   │ docker run    → container │        │ minicon run     → process │
   │   + cgroups + netns +     │        │   + namespaces + chroot   │
   │     seccomp + layers      │        │   (core subset only)      │
   └──────────────────────────┘        └──────────────────────────┘
```

---

## 2. System specification

| Item | Spec |
|------|------|
| Language | Go (standard library only, no external modules) |
| Platforms | `package`: any OS. `run`: **Linux only**, needs **root** |
| CPU arch | Native build; target host must match (e.g. arm64 ↔ arm64) |
| Image format | gzip( tar( `manifest.json` + `rootfs/…` ) ), one file `*.img` |
| Isolation | Namespaces: UTS, PID, MOUNT, IPC. Filesystem: `chroot`. |
| Resource limits | cgroups v2: CPU (`cpu.max`) + memory (`memory.max`) |
| NOT isolated | Network (shares host, like `--net=host`) |
| Out of scope | seccomp, pivot_root, image layers, registry, veth |
| Entry CLI | `minicon package` · `minicon run` · `minicon child` (internal) |

### Functional requirements
- FR1: Package a binary + optional extra files into a single image file.
- FR2: Store entrypoint, env, and port as metadata inside the image.
- FR3: Transport the image to another Linux host by plain file copy.
- FR4: Run the image with its own hostname, PID space, and mount table.
- FR5: Expose the app's TCP port on the host so it is reachable.
- FR6: Optionally cap CPU and memory for the container (cgroups v2).

### Non-functional
- NFR1: Zero third-party dependencies (easy to audit & build).
- NFR2: Human-readable image format (it's just tar — `tar tzf` inspects it).
- NFR3: Small enough to read in one sitting (~5 Go files).

---

## 3. Architecture (components & boundaries)

Two user-facing components (**Packager**, **Runtime**) over one shared
**Image** module. The Runtime internally re-executes itself as a hidden
**Child** stage to enter namespaces.

```
                         ┌──────────────────────────────────────────┐
                         │                 minicon CLI               │
                         │               (cmd/minicon)               │
                         └──────────────────────────────────────────┘
                              │                 │               │
               package        │                 │ run           │ child (internal)
                              ▼                 ▼               ▼
                  ┌────────────────┐  ┌────────────────┐  ┌────────────────┐
  COMPONENT 1 →   │   Packager      │  │    Runtime      │  │  Child stage    │
                  │  package.go     │  │    run.go       │  │  run.go         │
                  │                 │  │  + cgroup.go    │  │ (in namespaces) │
                  │                 │  │  (parent half)  │  │ joins cgroup    │
                  └───────┬─────────┘  └───────┬─────────┘  └───────┬────────┘
                          │                    │                     │
                          │  writes/reads      │  reads image        │ chroot+exec
                          ▼                    ▼                     ▼
                  ┌───────────────────────────────────────────────────────┐
  SHARED      →   │                    Image module (image.go)             │
                  │   tar + gzip, Manifest{}, addPathToTar, extractImage   │
                  └───────────────────────────────────────────────────────┘
                          ▲
                          │ produces/consumes
                          ▼
                  ┌───────────────────────────────┐
  ARTIFACT    →   │   hello.img  (one file)        │
                  │   ├── manifest.json            │
                  │   └── rootfs/                   │
                  │        ├── hello  (app binary)  │
                  │        └── assets/… (optional)  │
                  └───────────────────────────────┘
```

**Boundaries**
- Packager and Runtime never call each other — they only share the file format
  via the Image module. That's why an image built on host A runs on host B.
- The Child stage is *not* a separate program; it is `minicon` re-running
  itself (`/proc/self/exe child …`) so it starts life already inside the new
  namespaces.

---

## 4. High-level design

### 4.1 The two lifecycles

```
BUILD TIME (anywhere)                     RUN TIME (Linux, root)
────────────────────                      ──────────────────────
 source  main.go                            image  hello.img
   │ go build                                 │ minicon run
   ▼                                          ▼
 binary  hello                              extract → /tmp/minicon-*/rootfs
   │ minicon package                          │ clone namespaces
   ▼                                          ▼
 image   hello.img  ───────── scp ─────────▶ chroot + /proc + exec
                     (copy to target host)    │
                                              ▼
                                          app is PID 1, serving :8080
```

### 4.2 End-to-end deployment (your assignment scenario)

```
   BUILD EC2 / laptop                         TARGET EC2 (Amazon Linux 2023)
   ┌───────────────────┐                      ┌───────────────────────────┐
   │ go build hello    │                      │                           │
   │ minicon package   │   scp minicon        │ sudo ./minicon run         │
   │   → hello.img     │ ───  + hello.img ──▶ │   --image hello.img        │
   │                   │      (2 files)       │   → chroot + namespaces    │
   └───────────────────┘                      │   → app on :8080           │
                                              └──────────────┬────────────┘
                                                             │ curl / browser
                                                             ▼
                                                      HTTP 200 + HTML
```

The target needs **only** the `minicon` binary and the `.img` — no Go, no
Docker. (Same CPU arch on both ends.)

---

## 5. Detailed design

### 5.1 Image format on disk

```
hello.img  =  gzip
                └─ tar
                    ├─ manifest.json
                    └─ rootfs/
                        ├─ hello                 (the static app binary)
                        └─ assets/style.css      (any --add files)

manifest.json
{
  "name":       "hello:1.0",
  "entrypoint": ["/hello"],      // argv[0..] run inside the container
  "env":        ["PORT=8080"],   // extra env injected at run
  "port":       "8080",          // informational
  "created_at": "2026-10-31T..."
}
```

Why tar+gzip: it is the simplest format that preserves file modes, symlinks,
and directory structure, and you can inspect it with ordinary tools:
`tar tzf hello.img`.

### 5.2 Packager flow (`package.go → cmdPackage`)

```
cmdPackage(args)
   │
   ├─ parse flags: --name --bin --entrypoint --port --out --env --add
   ├─ validate: binary exists, --out given
   │
   ├─ create out file → gzip.Writer → tar.Writer
   │
   ├─ writeManifest(tw, Manifest{…})         # metadata entry
   ├─ addPathToTar(tw, bin,  base(bin))      # rootfs/hello
   └─ for each --add src:dest:
          addPathToTar(tw, src, dest)        # rootfs/<dest>/…
   (defers close tar → gzip → file, flushing the image)
```

`addPathToTar` walks a path with `filepath.Walk`, turns each entry into a
`tar.Header` (regular file, dir, or symlink), and writes it under
`rootfs/<dest>`.

### 5.3 Runtime flow — the re-exec trick (`run.go`)

Go programs cannot safely `fork()` then configure namespaces the way C does, so
the idiomatic approach is: launch a **new copy of yourself** with the clone
flags set, and have that copy finish the setup. Two stages:

```
   PARENT (cmdRun)                          CHILD (cmdChild)
   ─────────────────                        ─────────────────
   extractImage(img) → rootfs
   build exec.Command(
       "/proc/self/exe",
       "child", rootfs, host, entry)
   SysProcAttr.Cloneflags =
       NEWUTS|NEWPID|NEWNS|NEWIPC
   cmd.Run()  ───────────────────────────▶  (starts already inside new ns)
                                            sethostname(host)
                                            chroot(rootfs); chdir("/")
                                            mount -t proc proc /proc
                                            syscall.Exec(entry…)   ← becomes PID 1
   wait … cleanup temp rootfs  ◀──────────  (app runs until it exits)
```

**Namespace responsibilities**

```
 CLONE_NEWUTS  → own hostname          (page shows "minicon", not the EC2 name)
 CLONE_NEWPID  → own PID numbering     (app is PID 1 inside; ps sees only it)
 CLONE_NEWNS   → own mount table       (our private /proc never touches host)
 CLONE_NEWIPC  → own IPC / msg queues
 (no NEWNET)   → shares host network   (so :8080 is reachable from outside)
```

**Why `exec` (not just run):** `syscall.Exec` *replaces* the child process
image with the app, so there is no wrapper process sitting around — the app
genuinely becomes PID 1 of its namespace, exactly like a real container's
init process.

### 5.4 Resource allocation — cgroups v2 (`cgroup.go`)

cgroups cap how much CPU and memory the container may use — the same mechanism
behind `docker run --memory --cpus`. On cgroups v2 (default on Amazon Linux
2023) a control group is just a directory of files under `/sys/fs/cgroup`.

```
   PARENT (cmdRun)                          CHILD (cmdChild)
   ─────────────────                        ─────────────────
   buildLimits(--memory,--cpus)
     --memory 128m → "134217728"
     --cpus 0.5    → "50000 100000"
   limits.create(name):
     mkdir /sys/fs/cgroup/minicon/<pid>/
     echo 134217728   > memory.max
     echo 50000 100000 > cpu.max
   pass cgroup dir to child ───────────────▶ echo <self-pid> > cgroup.procs
                                              (join BEFORE chroot/exec, so the
                                               kernel caps us + all children)
                                            chroot → exec app
   wait … limits.cleanup():
     rmdir the cgroup dir  ◀──────────────── (group empty after app exits)
```

Mapping the flags to kernel values:

```
 --memory 128m   →  memory.max = 134217728          (128 × 1024 × 1024 bytes)
 --cpus 0.5      →  cpu.max    = "50000 100000"      quota 50000us / period 100000us
                                                     = 0.5 of one core per 100ms
 (no flag)       →  no cgroup created  →  unlimited
```

Why the **child** joins (not the parent placing it): writing the PID from the
child, before `exec`, removes the race where the app could start doing work
before being moved into the capped group.

### 5.5 Process tree at runtime

```
host (EC2)
└─ sudo minicon run --image hello.img --memory 128m --cpus 0.5
   │                                          (parent, host PID e.g. 4120)
   │   cgroup: /sys/fs/cgroup/minicon/4120/  (memory.max, cpu.max)
   └─ [clone: NEWUTS|NEWPID|NEWNS|NEWIPC]
      └─ minicon child …                      (re-exec, host PID 4121)
         │   writes 4121 → cgroup.procs       (now capped by the kernel)
         └─ exec /hello                        (replaces child, still capped)
              ├─ inside its PID ns:  PID 1
              └─ listening on :8080 (host network)
```

### 5.6 Control flow (CLI dispatch — `main.go`)

```
main()
  switch argv[1]
    "package" → cmdPackage(rest)   # COMPONENT 1
    "run"     → cmdRun(rest)       # COMPONENT 2 (parent)
    "child"   → cmdChild(rest)     # internal, never typed by a user
    help/-h   → usage()
    default   → usage(); exit 2
```

### 5.7 Error handling & cleanup

- Every syscall wrapper returns a wrapped error (`fmt.Errorf("...: %w", err)`)
  so failures name the stage (e.g. `chroot (are you root?): ...`).
- The parent extracts into `os.MkdirTemp` and `defer`s `os.RemoveAll`, so a
  crash still cleans the temp rootfs unless `--keep` is passed for debugging.
- `/proc` mount failure is a warning, not fatal — the HTTP app still runs.

---

## 6. Data & file reference

| File | Role in the design |
|------|--------------------|
| `app/main.go` | Payload: hello web server, prints hostname + PID to prove isolation. |
| `cmd/minicon/main.go` | CLI dispatch to the three subcommands. |
| `cmd/minicon/image.go` | Shared image format: `Manifest`, tar write (`addPathToTar`), tar read (`extractImage`). |
| `cmd/minicon/package.go` | Component 1 — build an image file. |
| `cmd/minicon/run.go` | Component 2 — parent (`cmdRun`) + child (`cmdChild`) runtime. |
| `cmd/minicon/cgroup.go` | Resource allocation — cgroups v2 CPU + memory limits. |

---

## 7. Design trade-offs (talking points for the report)

| Decision | Chose | Instead of | Why |
|----------|-------|------------|-----|
| Isolation depth | namespaces + chroot + cgroups | + seccomp + caps | Show the core + resource limits; name syscall filtering as future work. |
| Filesystem switch | `chroot` | `pivot_root` | Simpler to explain; pivot_root is the hardened version. |
| Networking | host network | network namespace + veth | App port is instantly reachable; veth needs IP plumbing. |
| Image shape | one flat tar | stacked layers + registry | No dedup/caching, but trivial to copy and inspect. |
| Child setup | re-exec `/proc/self/exe` | C-style fork | The safe, idiomatic way to enter namespaces from Go. |

---

## 8. Future work (natural extensions)

1. ✅ **cgroups v2** — CPU + memory limits. *Implemented* (see §5.4).
2. **Network namespace + veth** — give the container its own IP, add port NAT.
3. **pivot_root + read-only rootfs** — stronger filesystem isolation.
4. **Image layers** — base layer + app layer, shared across images.
5. **seccomp / dropped capabilities** — restrict the syscalls the app may make.
6. **Extend cgroups** — add `pids.max`, `io.max`, and live stats readout.
