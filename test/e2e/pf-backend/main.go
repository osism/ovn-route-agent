// pf-backend is a minimal HTTP responder used by the port-forward E2E
// scenario (issue #109). It logs the source IP of each accepted TCP
// connection — that log line is the load-bearing assertion in
// test/e2e/scenarios/pf-external.sh: if OVN's DNAT data path silently
// SNATs the client on the way in, the recorded peer is the LR's internal
// SNAT address or the gateway chassis address instead of client-1's
// underlay IP, and the scenario fails the source-IP-preservation check.
//
// The binary is built in the same Dockerfile.gwnode build stage as the
// agent and shipped at /usr/local/bin/pf-backend, so the scenario can
// `ip netns exec vm1 /usr/local/bin/pf-backend ...` without depending
// on Python, netcat or socat being installed in the runtime image
// (which is held under the 600 MB gwnode image budget).
//
// Flags:
//
//	-addr    TCP listen address (default ":8080")
//	-log     append per-connection log lines to this file in addition
//	         to stderr. Empty disables the file sink. The scenario's
//	         EXIT trap copies the file into ARTIFACTS_DIR so the
//	         source-IP log is part of the failure artifact bundle (per
//	         the issue's "backend source-IP log is included in the
//	         artifact bundle on failure" acceptance criterion).
//
// The response body is the fixed string "ok\n" — the test does not
// assert on the response content, only on the peer address recorded
// in the log.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", ":8080", "TCP listen address")
	logPath := flag.String("log", "", "append per-connection log lines to this file (in addition to stderr)")
	flag.Parse()

	sinks := []io.Writer{os.Stderr}
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("open %s: %v", *logPath, err)
		}
		defer func() { _ = f.Close() }()
		sinks = append(sinks, f)
	}
	out := io.MultiWriter(sinks...)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// RemoteAddr is "<host>:<port>". When OVN performs pure DNAT
		// (the property under test), <host> is the client's underlay
		// IP. A stray SNAT step on the way in would substitute the
		// LR's internal address here instead.
		_, _ = fmt.Fprintf(out, "peer=%s method=%s path=%s host=%s\n",
			r.RemoteAddr, r.Method, r.URL.Path, r.Host)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})

	log.Fatal(http.ListenAndServe(*addr, nil))
}
