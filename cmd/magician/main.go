// magician walks one Yealink phone through a fresh provisioning cycle.
//
// Given the phone's IP and a .cfg file (and optionally a .rom firmware
// file), it:
//   1. Starts an HTTP server on the local IP that will serve the cfg
//      (and firmware, if given) to exactly that phone.
//   2. Joins the PnP multicast group and waits for the phone's boot-time
//      SUBSCRIBE. Subscribes from any other source IP are ignored.
//   3. Replies with a NOTIFY pointing the phone at our cfg URL; the phone
//      fetches it, applies it, fetches the firmware if static.firmware.url
//      is set, reflashes, and reboots.
//
// The operator is expected to factory-reset the phone (hold OK ~10s) after
// starting the magician — we don't have credentials to trigger it remotely.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maurits/yealink-magician/internal/pnp"
)

const (
	cfgPath = "/y000000000000.cfg"
	fwPath  = "/firmware.rom"
)

func main() {
	ipFlag := flag.String("ip", "", "phone IP address (required)")
	cfgFile := flag.String("cfg", "", "path to .cfg file to push (required)")
	fwFile := flag.String("firmware", "", "path to .rom firmware to push (optional)")
	ifaceName := flag.String("interface", "", "network interface (default: system default)")
	httpPort := flag.Int("http-port", 25565, "HTTP port for cfg/firmware server (0 = ephemeral)")
	timeout := flag.Duration("timeout", 10*time.Minute, "how long to wait for the phone")
	flag.Parse()

	if *ipFlag == "" || *cfgFile == "" {
		fmt.Fprintln(os.Stderr, "error: -ip and -cfg are required")
		flag.Usage()
		os.Exit(2)
	}

	target := net.ParseIP(*ipFlag)
	if target == nil || target.To4() == nil {
		log.Fatalf("invalid IPv4 address: %s", *ipFlag)
	}
	target = target.To4()

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
	if *ifaceName != "" {
		ifi, err = net.InterfaceByName(*ifaceName)
		if err != nil {
			log.Fatalf("interface %s: %v", *ifaceName, err)
		}
		localIP, err = pnp.InterfaceIPv4(ifi)
		if err != nil {
			log.Fatalf("local ip on %s: %v", *ifaceName, err)
		}
	} else {
		ifi, localIP, err = pnp.LocalForPeer(target)
		if err != nil {
			log.Fatalf("route to phone: %v", err)
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

	cfgFetched := make(chan struct{}, 1)
	fwFetched := make(chan struct{}, 1)

	allowPeer := func(r *http.Request) bool {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		peer := net.ParseIP(host)
		return peer != nil && peer.To4().Equal(target)
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
		select {
		case cfgFetched <- struct{}{}:
		default:
		}
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
			select {
			case fwFetched <- struct{}{}:
			default:
			}
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

	notified := make(chan struct{}, 1)
	r := &pnp.Responder{
		Interface: ifi,
		Handler: func(s pnp.Subscribe) (string, bool) {
			if !s.Source.IP.To4().Equal(target) {
				log.Printf("PnP: ignoring SUBSCRIBE from %s (want %s)", s.Source.IP, target)
				return "", false
			}
			log.Printf("PnP: SUBSCRIBE from %s (%s) — sending URL", s.Source, s.UserAgent)
			select {
			case notified <- struct{}{}:
			default:
			}
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
	log.Printf("")
	log.Printf("==> Now factory-reset the phone at %s.", target)
	log.Printf("    Press and hold the OK key for ~10s, then confirm the reset prompt.")
	log.Printf("")
	log.Printf("Waiting up to %s for the phone's PnP multicast...", *timeout)

	select {
	case <-notified:
	case <-ctx.Done():
		log.Fatalf("timed out waiting for PnP from %s", target)
	}

	log.Printf("waiting for phone to fetch cfg...")
	select {
	case <-cfgFetched:
		log.Printf("phone fetched cfg")
	case <-ctx.Done():
		log.Fatalf("timed out waiting for phone to fetch cfg from %s", target)
	}

	if *fwFile == "" {
		log.Printf("done — phone will apply config and reboot")
		return
	}

	log.Printf("waiting for phone to fetch firmware...")
	select {
	case <-fwFetched:
	case <-ctx.Done():
		log.Fatalf("timed out waiting for phone to fetch firmware from %s", target)
	}
	log.Printf("phone is fetching firmware; staying up 30s for any straggling range requests")
	graceCtx, graceCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer graceCancel()
	<-graceCtx.Done()
	log.Printf("done — phone will reflash and reboot")
}

func ifaceLabel(ifi *net.Interface) string {
	if ifi == nil {
		return "default"
	}
	return ifi.Name
}
