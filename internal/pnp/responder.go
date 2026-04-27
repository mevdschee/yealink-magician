// Package pnp implements an RFC-6080-style multicast PnP responder that
// answers Yealink phones' boot-time SUBSCRIBE messages with a NOTIFY
// pointing them at a provisioning URL.
package pnp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strings"
)

const Group = "224.0.1.75:5060"

type Subscribe struct {
	Source    *net.UDPAddr
	From      string
	CallID    string
	Contact   string
	UserAgent string
	Event     string
}

// Handler decides whether to respond to a SUBSCRIBE and, if so, with which URL.
// Return ok=false to ignore the subscriber silently.
type Handler func(Subscribe) (url string, ok bool)

type Responder struct {
	Group     string         // multicast group:port; defaults to pnp.Group
	Interface *net.Interface // bind interface; nil = system default
	Handler   Handler
	Logger    *log.Logger
}

func (r *Responder) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger.Printf(format, args...)
	}
}

func (r *Responder) Run(ctx context.Context) error {
	group := r.Group
	if group == "" {
		group = Group
	}
	groupAddr, err := net.ResolveUDPAddr("udp4", group)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", group, err)
	}
	localIP, err := InterfaceIPv4(r.Interface)
	if err != nil {
		return err
	}
	listener, err := net.ListenMulticastUDP("udp4", r.Interface, groupAddr)
	if err != nil {
		return fmt.Errorf("listen multicast %s: %w", groupAddr, err)
	}
	defer listener.Close()
	_ = listener.SetReadBuffer(1 << 20)

	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP, Port: 0})
	if err != nil {
		return fmt.Errorf("sender socket: %w", err)
	}
	defer sender.Close()
	localHostPort := sender.LocalAddr().(*net.UDPAddr).String()

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	buf := make([]byte, 64*1024)
	for {
		n, src, err := listener.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if strings.Contains(err.Error(), "use of closed") {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		msg := append([]byte(nil), buf[:n]...)
		go r.handle(sender, src, msg, localHostPort)
	}
}

func (r *Responder) handle(sender *net.UDPConn, src *net.UDPAddr, msg []byte, localHostPort string) {
	sub, err := parseSubscribe(msg)
	if err != nil {
		r.logf("from %s: %v", src, err)
		return
	}
	sub.Source = src
	if !strings.Contains(strings.ToLower(sub.Event), "ua-profile") {
		return
	}
	url, ok := r.Handler(*sub)
	if !ok {
		return
	}

	target := uriFromHeader(sub.Contact)
	if target == "" {
		target = uriFromHeader(sub.From)
	}
	dst := src
	if hp, herr := hostPort(target); herr == nil {
		if a, aerr := net.ResolveUDPAddr("udp4", hp); aerr == nil {
			dst = a
		}
	}
	if _, err := sender.WriteToUDP(buildNotify(*sub, localHostPort, url), dst); err != nil {
		r.logf("send to %s: %v", dst, err)
		return
	}
	r.logf("notified %s -> %s", dst, url)
}

func parseSubscribe(b []byte) (*Subscribe, error) {
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 0, 8192), 64*1024)
	if !sc.Scan() {
		return nil, fmt.Errorf("empty datagram")
	}
	if !strings.HasPrefix(sc.Text(), "SUBSCRIBE ") {
		return nil, fmt.Errorf("not SUBSCRIBE: %q", sc.Text())
	}
	s := &Subscribe{}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "from", "f":
			s.From = val
		case "call-id", "i":
			s.CallID = val
		case "contact", "m":
			s.Contact = val
		case "user-agent":
			s.UserAgent = val
		case "event", "o":
			s.Event = val
		}
	}
	if s.CallID == "" || s.From == "" || s.Contact == "" {
		return nil, fmt.Errorf("missing required headers")
	}
	return s, nil
}

func uriFromHeader(h string) string {
	if i := strings.Index(h, "<"); i >= 0 {
		if j := strings.Index(h[i:], ">"); j >= 0 {
			return h[i+1 : i+j]
		}
	}
	return strings.SplitN(h, ";", 2)[0]
}

func hostPort(uri string) (string, error) {
	u := uri
	for _, p := range []string{"sip:", "sips:"} {
		u = strings.TrimPrefix(u, p)
	}
	if i := strings.Index(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	if i := strings.IndexAny(u, ";?>"); i >= 0 {
		u = u[:i]
	}
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		return net.JoinHostPort(u, "5060"), nil
	}
	if port == "" {
		port = "5060"
	}
	return net.JoinHostPort(host, port), nil
}

func token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func buildNotify(s Subscribe, localHostPort, profileURL string) []byte {
	target := uriFromHeader(s.Contact)
	if target == "" {
		target = uriFromHeader(s.From)
	}
	body := profileURL
	lines := []string{
		"NOTIFY " + target + " SIP/2.0",
		"Via: SIP/2.0/UDP " + localHostPort + ";branch=z9hG4bK" + token(6),
		"Max-Forwards: 70",
		"From: <sip:pnp@" + localHostPort + ">;tag=" + token(4),
		"To: " + s.From,
		"Call-ID: " + s.CallID,
		"CSeq: 1 NOTIFY",
		"Contact: <sip:pnp@" + localHostPort + ">",
		`Event: ua-profile;effective-by=0`,
		"Subscription-State: terminated;reason=timeout",
		"Content-Type: application/url",
		fmt.Sprintf("Content-Length: %d", len(body)),
		"",
		body,
	}
	return []byte(strings.Join(lines, "\r\n"))
}

// LocalForPeer asks the kernel which IPv4 address (and interface) would be
// used to reach peer. This is the routing-correct choice on multi-homed hosts.
func LocalForPeer(peer net.IP) (*net.Interface, net.IP, error) {
	c, err := net.Dial("udp4", net.JoinHostPort(peer.String(), "9"))
	if err != nil {
		return nil, nil, fmt.Errorf("route to %s: %w", peer, err)
	}
	_ = c.Close()
	local := c.LocalAddr().(*net.UDPAddr).IP.To4()
	if local == nil {
		return nil, nil, fmt.Errorf("no IPv4 route to %s", peer)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	for _, ifi := range ifaces {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipn.IP.To4() != nil && ipn.IP.Equal(local) {
				ifi := ifi
				return &ifi, local, nil
			}
		}
	}
	return nil, local, fmt.Errorf("no interface owns %s", local)
}

func InterfaceIPv4(ifi *net.Interface) (net.IP, error) {
	var addrs []net.Addr
	var err error
	if ifi != nil {
		addrs, err = ifi.Addrs()
	} else {
		addrs, err = net.InterfaceAddrs()
	}
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4, nil
		}
	}
	return nil, fmt.Errorf("no usable IPv4 address")
}
