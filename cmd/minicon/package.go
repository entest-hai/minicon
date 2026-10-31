package main

// package.go -- COMPONENT 1: the packager.
//
// `minicon package` takes an app binary (and optionally extra files/dirs such
// as shared libraries or static assets) and writes a single image file:
//
//   minicon package \
//       --name hello:1.0 \
//       --bin ./hello \
//       --entrypoint /hello \
//       --port 8080 \
//       --add ./assets:/assets \
//       --out hello.img
//
// The result (hello.img) is a portable, self-contained bundle you can scp to
// another machine and run with `minicon run`.

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type addMount struct {
	src  string // path on the build host
	dest string // path inside the image rootfs
}

func cmdPackage(args []string) error {
	fs := flag.NewFlagSet("package", flag.ExitOnError)
	name := fs.String("name", "app:latest", "image name tag")
	bin := fs.String("bin", "", "path to the compiled app binary (required)")
	entry := fs.String("entrypoint", "", "command to run inside the container (default: /<binary name>)")
	port := fs.String("port", "8080", "port the app listens on (informational)")
	out := fs.String("out", "", "output image path (required), e.g. hello.img")
	var env multiFlag
	var add multiFlag
	fs.Var(&env, "env", "extra env var KEY=VALUE (repeatable)")
	fs.Var(&add, "add", "extra file/dir to include as src:dest (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *bin == "" || *out == "" {
		return fmt.Errorf("--bin and --out are required (see --help)")
	}
	if _, err := os.Stat(*bin); err != nil {
		return fmt.Errorf("binary not found: %w", err)
	}

	binBase := "/" + filepath.Base(*bin)
	if *entry == "" {
		*entry = binBase
	}

	// Parse the extra --add mounts.
	var mounts []addMount
	for _, a := range add {
		parts := strings.SplitN(a, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("bad --add %q, expected src:dest", a)
		}
		mounts = append(mounts, addMount{src: parts[0], dest: parts[1]})
	}

	// Create the output image: file -> gzip -> tar.
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	// 1. manifest.
	m := Manifest{
		Name:       *name,
		Entrypoint: strings.Fields(*entry),
		Env:        env,
		Port:       *port,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeManifest(tw, m); err != nil {
		return err
	}

	// 2. the app binary, placed at the root of the rootfs (e.g. rootfs/hello).
	if err := addPathToTar(tw, *bin, filepath.Base(*bin)); err != nil {
		return fmt.Errorf("packing binary: %w", err)
	}

	// 3. any extra files/dirs.
	for _, mt := range mounts {
		if _, err := os.Stat(mt.src); err != nil {
			return fmt.Errorf("--add source not found: %s", mt.src)
		}
		if err := addPathToTar(tw, mt.src, mt.dest); err != nil {
			return fmt.Errorf("packing %s: %w", mt.src, err)
		}
	}

	fmt.Printf("✅ built image %q -> %s\n", *name, *out)
	fmt.Printf("   entrypoint: %v\n", m.Entrypoint)
	fmt.Printf("   port:       %s\n", m.Port)
	fmt.Printf("   copy it to another host and run: sudo ./minicon run --image %s\n", filepath.Base(*out))
	return nil
}

// multiFlag lets a flag be supplied multiple times (--env A=1 --env B=2).
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
