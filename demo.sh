#!/usr/bin/env bash
# demo.sh -- end-to-end: build -> package -> run -> curl.
# Run on Amazon Linux 2023 (or any Linux) with Go installed.
set -euo pipefail
cd "$(dirname "$0")"

echo "== 1. build the minicon tool =="
go build -o minicon ./cmd/minicon

echo "== 2. build the hello app (static, no libc dependency) =="
# CGO_ENABLED=0 makes a fully static binary so the image needs no shared libs.
CGO_ENABLED=0 go build -o hello ./app

echo "== 3. PACKAGE: bundle the app into an image =="
./minicon package \
  --name hello:1.0 \
  --bin ./hello \
  --entrypoint /hello \
  --port 8080 \
  --out hello.img
ls -lh hello.img

echo
echo "== 4. RUN: start the container (needs root for namespaces/chroot) =="
echo "   run this yourself:"
echo "     sudo ./minicon run --image hello.img &"
echo "     sleep 1 && curl -s http://localhost:8080 | head"
echo
echo "To deploy to another EC2:"
echo "   scp minicon hello.img ec2-user@<OTHER_EC2>:~/"
echo "   ssh ec2-user@<OTHER_EC2> 'sudo ./minicon run --image hello.img'"
