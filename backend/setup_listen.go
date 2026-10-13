package backend

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Setup listens on 127.0.0.1, which is right on a laptop and a dead end on a
// server nobody sits at: its browser is somewhere else. Binding wider by
// default would put the setup form, which carries the SSO client secret and
// model API keys, on the network in plain HTTP before anyone chose that. So
// setup stays on loopback, says how to tunnel to it, and lets the operator
// add one of the machine's other addresses by number, which is only offered on
// a terminal, so a service manager never waits on a question.

// HostAddress is one address of this machine, with the interface it is on.
type HostAddress struct {
	IP    net.IP
	Iface string
}

// OtherAddresses lists the addresses of this machine that a browser on
// another one could reach: every interface address except loopback and
// link-local, IPv4 first.
func OtherAddresses() []HostAddress {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []HostAddress
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
				continue
			}
			out = append(out, HostAddress{IP: ip, Iface: iface.Name})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].IP.To4() != nil && out[j].IP.To4() == nil })
	return out
}

// ListenAlso serves on a second address alongside the first, and returns the
// address it bound. Shutdown and Close stop it with the rest of the server.
func (s *Server) ListenAlso(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	go s.http.Serve(ln)
	return ln.Addr().String(), nil
}

// setupURLOn is the setup URL with its host swapped for addr.
func (s *Server) setupURLOn(addr string) string {
	u, err := url.Parse(s.SetupURL)
	if err != nil {
		return s.SetupURL
	}
	u.Host = addr
	return u.String()
}

// OfferRemoteSetup prints how to reach setup from another computer. On a
// terminal it also lists addrs by number and, in the background, listens on
// the one the operator types, as well as on loopback.
func (s *Server) OfferRemoteSetup(in io.Reader, out io.Writer, interactive bool, addrs []HostAddress) {
	host, port, _ := net.SplitHostPort(s.Addr())
	ip := net.ParseIP(host)

	if ip != nil && ip.IsUnspecified() {
		if len(addrs) == 0 {
			return
		}
		fmt.Fprintln(out, "It answers on this machine's other addresses too, over plain HTTP:")
		fmt.Fprintln(out)
		for _, a := range addrs {
			fmt.Fprintf(out, "    %s\n", s.setupURLOn(net.JoinHostPort(a.IP.String(), port)))
		}
		fmt.Fprintln(out)
		return
	}
	if ip == nil || !ip.IsLoopback() {
		// The operator named an address; the URL above already uses it.
		return
	}

	fmt.Fprintln(out, "No browser on this machine? From your own computer, run")
	fmt.Fprintf(out, "\n    ssh -L %s:%s <you>@<this server>\n\n", port, net.JoinHostPort(host, port))
	fmt.Fprintln(out, "and open the URL above there.")
	if len(addrs) == 0 {
		fmt.Fprintln(out)
		return
	}
	if !interactive {
		fmt.Fprintf(out, "Or start setup with --listen <address>:%s to answer on another address\n", port)
		fmt.Fprintln(out, "of this machine, over plain HTTP.")
		fmt.Fprintln(out)
		return
	}

	fmt.Fprintln(out, "Or answer on another address of this machine as well:")
	fmt.Fprintln(out)
	width := 0
	for _, a := range addrs {
		width = max(width, len(a.IP.String()))
	}
	for i, a := range addrs {
		fmt.Fprintf(out, "    %d) %-*s  %s\n", i+1, width, a.IP, a.Iface)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "That runs setup over plain HTTP, and the form sends your SSO client")
	fmt.Fprintln(out, "secret and model API keys, so pick one only on a network you trust.")
	fmt.Fprintln(out, "Type its number and press Enter.")
	fmt.Fprintln(out)
	go s.readAddressChoice(in, out, addrs, port)
}

// readAddressChoice reads numbers until one of them is listened on.
func (s *Server) readAddressChoice(in io.Reader, out io.Writer, addrs []HostAddress, port string) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(addrs) {
			fmt.Fprintf(out, "Type a number from 1 to %d.\n", len(addrs))
			continue
		}
		bound, err := s.ListenAlso(net.JoinHostPort(addrs[n-1].IP.String(), port))
		if err != nil {
			fmt.Fprintf(out, "Could not listen there: %v\n", err)
			continue
		}
		fmt.Fprintln(out, "\nAlso listening there. Open this one-time URL from your own computer:")
		fmt.Fprintf(out, "\n    %s\n\n", s.setupURLOn(bound))
		fmt.Fprintln(out, "If it does not load, a firewall on this machine or in front of it is")
		fmt.Fprintf(out, "likely blocking port %s.\n", port)
		return
	}
}
