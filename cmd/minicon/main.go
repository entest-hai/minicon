package main

// minicon -- a tiny educational container tool in Go.
//
// Two components, mirroring a real container system:
//
//   1. PACKAGE (build-time):  minicon package ...   -> produces an image file
//   2. RUN     (run-time):    minicon run --image X -> runs it in namespaces
//
// Plus a hidden "child" subcommand that minicon re-execs into to set up the
// namespaces. You never call "child" by hand.
//
// Build:   go build -o minicon ./cmd/minicon
// Linux-only for `run` (uses namespaces + chroot). `package` works anywhere.

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Fprint(os.Stderr, `minicon -- a minimal container tool

USAGE:
  minicon package --name NAME --bin ./app --entrypoint /app --out app.img [--env K=V] [--add src:dest]
  minicon run     --image app.img [--hostname NAME] [--memory 128m] [--cpus 0.5] [--keep]

COMMANDS:
  package   Bundle an app binary (+ libs/assets) into a portable image file.
  run       Extract an image and run it in its own Linux namespaces (needs root).

EXAMPLE:
  # build the hello app statically, then package it
  CGO_ENABLED=0 go build -o hello ./app
  minicon package --name hello:1.0 --bin ./hello --entrypoint /hello --port 8080 --out hello.img

  # on the target EC2 (Amazon Linux 2023), as root:
  sudo ./minicon run --image hello.img
  curl http://localhost:8080

  # with resource limits (cgroups v2): cap at 128 MiB RAM and half a CPU core
  sudo ./minicon run --image hello.img --memory 128m --cpus 0.5
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "package":
		err = cmdPackage(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "child":
		// internal: invoked by `run` after cloning namespaces.
		err = cmdChild(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
