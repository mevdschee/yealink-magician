// magician walks Yealink phones through a fresh provisioning cycle.
//
// Given a .cfg file (and optionally a .rom firmware file), it:
//  1. Starts an HTTP server on the local IP that serves the cfg (and
//     firmware, if given).
//  2. Joins the PnP multicast group and waits for a phone's boot-time
//     SUBSCRIBE.
//  3. For each SUBSCRIBE it prompts the operator, then replies with a
//     NOTIFY pointing the phone at our cfg URL; the phone fetches it,
//     applies it, fetches the firmware if static.firmware.url is set,
//     reflashes, and reboots.
//
// If -ip is given it also acts as a filter: SUBSCRIBEs and HTTP fetches
// from any other address are ignored. Without -ip the magician answers
// any phone on the segment (still gated by the per-SUBSCRIBE prompt).
//
// The magician keeps running until interrupted (Ctrl-C) or the timeout
// elapses, so it can provision several phones in one session.
//
// The operator is expected to factory-reset the phone (hold OK ~10s) after
// starting the magician — we don't have credentials to trigger it remotely.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/maurits/yealink-magician/internal/pnp"
)

const (
	cfgPath = "/y000000000000.cfg"
	fwPath  = "/firmware.rom"
)

func main() {
	ipFlag := flag.String("ip", "", "phone IP address to filter on (optional; default: answer any phone)")
	cfgFile := flag.String("cfg", "", "path to .cfg file to push (required)")
	fwFile := flag.String("firmware", "", "path to .rom firmware to push (optional)")
	ifaceName := flag.String("interface", "", "network interface (default: system default)")
	httpPort := flag.Int("http-port", 25565, "HTTP port for cfg/firmware server (0 = ephemeral)")
	timeout := flag.Duration("timeout", 10*time.Minute, "how long to keep running")
	yes := flag.Bool("yes", false, "answer every SUBSCRIBE without prompting")
	flag.Parse()

	if *cfgFile == "" {
		fmt.Fprintln(os.Stderr, "error: -cfg is required")
		flag.Usage()
		os.Exit(2)
	}

	// target is the optional IP filter. nil means "answer any phone".
	var target net.IP
	if *ipFlag != "" {
		target = net.ParseIP(*ipFlag)
		if target == nil || target.To4() == nil {
			log.Fatalf("invalid IPv4 address: %s", *ipFlag)
		}
		target = target.To4()
	}

	cfgData, err := os.ReadFile(*cfgFile)
	if err != nil {
		log.Fatalf("read cfg: %v", err)
	}

	if *fwFile != "" {
		fi, err := os.Stat(*fwFile)
		if err != nil {
			log.Fatalf("stat firmware: %v", err)
		}
		if fi.IsDir() {
			log.Fatalf("firmware path %s is a directory", *fwFile)
		}
	}

	var (
		ifi     *net.Interface
		localIP net.IP
	)
	switch {
	case *ifaceName != "":
		ifi, err = net.InterfaceByName(*ifaceName)
		if err != nil {
			log.Fatalf("interface %s: %v", *ifaceName, err)
		}
		localIP, err = pnp.InterfaceIPv4(ifi)
		if err != nil {
			log.Fatalf("local ip on %s: %v", *ifaceName, err)
		}
	case target != nil:
		ifi, localIP, err = pnp.LocalForPeer(target)
		if err != nil {
			log.Fatalf("route to phone: %v", err)
		}
	default:
		// No interface and no target to route toward: fall back to the
		// first usable IPv4 the system offers.
		localIP, err = pnp.InterfaceIPv4(nil)
		if err != nil {
			log.Fatalf("local ip: %v", err)
		}
	}

	httpListener, err := net.Listen("tcp4", fmt.Sprintf("%s:%d", localIP, *httpPort))
	if err != nil {
		log.Fatalf("http listen: %v", err)
	}
	defer httpListener.Close()
	httpAddr := httpListener.Addr().String()
	cfgURL := fmt.Sprintf("http://%s%s", httpAddr, cfgPath)
	fwURL := fmt.Sprintf("http://%s%s", httpAddr, fwPath)

	if *fwFile != "" {
		cfgData = append(cfgData, []byte(fmt.Sprintf("\n# yealink-magician\nstatic.firmware.url = %s\n", fwURL))...)
	}

	// allowPeer gates HTTP fetches by the optional IP filter.
	allowPeer := func(r *http.Request) bool {
		if target == nil {
			return true
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		peer := net.ParseIP(host)
		return peer != nil && peer.To4() != nil && peer.To4().Equal(target)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(cfgPath, func(w http.ResponseWriter, r *http.Request) {
		if !allowPeer(r) {
			log.Printf("HTTP: rejecting %s from %s (want %s)", r.URL.Path, r.RemoteAddr, target)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		log.Printf("HTTP: %s %s from %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(cfgData)))
		_, _ = w.Write(cfgData)
	})
	if *fwFile != "" {
		mux.HandleFunc(fwPath, func(w http.ResponseWriter, r *http.Request) {
			if !allowPeer(r) {
				log.Printf("HTTP: rejecting %s from %s (want %s)", r.URL.Path, r.RemoteAddr, target)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			log.Printf("HTTP: %s %s from %s (range=%q, ua=%q)", r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("Range"), r.UserAgent())
			http.ServeFile(w, r, *fwFile)
		})
	}
	server := &http.Server{Handler: mux}
	go func() {
		if err := server.Serve(httpListener); err != nil && err != http.ErrServerClosed {
			log.Printf("http: %v", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
	}()

	// prompter serializes the per-SUBSCRIBE operator prompt so concurrent
	// SUBSCRIBEs don't interleave their questions on the terminal.
	var promptMu sync.Mutex
	stdin := bufio.NewReader(os.Stdin)
	confirm := func(s pnp.Subscribe) bool {
		if *yes {
			return true
		}
		promptMu.Lock()
		defer promptMu.Unlock()
		fmt.Printf("    send provisioning URL to %s (%s)? [y/N] ", s.Source, s.UserAgent)
		line, err := stdin.ReadString('\n')
		if err != nil {
			// EOF / no TTY: treat as "no" unless -yes was passed.
			return false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true
		default:
			return false
		}
	}

	r := &pnp.Responder{
		Interface: ifi,
		Handler: func(s pnp.Subscribe) (string, bool) {
			if target != nil && (s.Source.IP.To4() == nil || !s.Source.IP.To4().Equal(target)) {
				log.Printf("PnP: ignoring SUBSCRIBE from %s (want %s)", s.Source.IP, target)
				return "", false
			}
			log.Printf("PnP: SUBSCRIBE from %s (%s)", s.Source, s.UserAgent)
			if !confirm(s) {
				log.Printf("PnP: skipped %s", s.Source)
				return "", false
			}
			log.Printf("PnP: sending URL to %s -> %s", s.Source, cfgURL)
			return cfgURL, true
		},
		Logger: log.Default(),
	}
	go func() {
		if err := r.Run(ctx); err != nil && err != context.Canceled {
			log.Printf("pnp: %v", err)
		}
	}()

	log.Printf("HTTP serving cfg (%d bytes) at %s", len(cfgData), cfgURL)
	if *fwFile != "" {
		log.Printf("HTTP serving firmware %q at %s", *fwFile, fwURL)
	}
	log.Printf("PnP listening on %s (interface=%s)", pnp.Group, ifaceLabel(ifi))
	if target != nil {
		log.Printf("IP filter: answering only %s", target)
	} else {
		log.Printf("IP filter: off (answering any phone on the segment)")
	}
	log.Printf("")
	if target != nil {
		log.Printf("==> Now factory-reset the phone at %s.", target)
	} else {
		log.Printf("==> Now factory-reset the phone you want to provision.")
	}
	log.Printf("    Press and hold the OK key for ~10s, then confirm the reset prompt.")
	log.Printf("")
	log.Printf("Running until Ctrl-C or %s elapses; you'll be prompted on each SUBSCRIBE.", *timeout)

	<-ctx.Done()
	log.Printf("shutting down")
}

func ifaceLabel(ifi *net.Interface) string {
	if ifi == nil {
		return "default"
	}
	return ifi.Name
}
