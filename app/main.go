// A tiny hello web app. It is the "payload" we will package into an image
// and run inside our mini-container runtime.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

const page = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Hello from minicon</title>
<style>
  body{font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#0f172a;color:#e2e8f0;
       display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}
  .card{background:#1e293b;padding:40px 48px;border-radius:16px;box-shadow:0 10px 30px rgba(0,0,0,.4);text-align:center}
  h1{margin:0 0 8px;font-size:30px}
  p{margin:4px 0;color:#94a3b8}
  code{background:#334155;padding:2px 8px;border-radius:6px;color:#7dd3fc}
</style></head>
<body>
  <div class="card">
    <h1>👋 Hello from inside a container!</h1>
    <p>Served by a Go app running in its own namespaces.</p>
    <p>Hostname: <code>%s</code> &middot; PID: <code>%d</code></p>
  </div>
</body>
</html>`

func main() {
	addr := ":8080"
	if v := os.Getenv("PORT"); v != "" {
		addr = ":" + v
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host, _ := os.Hostname()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, page, host, os.Getpid())
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	log.Printf("hello-app listening on %s (pid %d)", addr, os.Getpid())
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
