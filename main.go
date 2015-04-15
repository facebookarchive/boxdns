// Command boxdns provides a DNS server for a subset of docker containers.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/facebookgo/dockerutil"
	"github.com/facebookgo/errgroup"
	"github.com/facebookgo/stackerr"
	"github.com/miekg/dns"
	"github.com/samalba/dockerclient"
)

const (
	qtypeIPv4 = 1
	qtypeIPv6 = 28
)

// Overrides specify the custom hostname => IP address our DNS server will
// override.
type Overrides map[string]Host

// Host is the per host override entries.
type Host struct {
	IPv4 net.IP
	IPv6 net.IP
}

// DNSClient allows us to forward unhandled DNS queries.
type DNSClient interface {
	Exchange(m *dns.Msg, a string) (r *dns.Msg, rtt time.Duration, err error)
}

// DockerClient allows us to build our overrides and monitor for container
// changes.
type DockerClient interface {
	StartMonitorEvents(dockerclient.Callback, chan error, ...interface{})
	ListContainers(all bool, size bool, filters string) ([]dockerclient.Container, error)
	InspectContainer(id string) (*dockerclient.ContainerInfo, error)
}

// App is our DNS server.
type App struct {
	Addr        string
	Prefix      string
	Domain      string
	Nameservers []string
	Overrides   atomic.Value
	Log         *log.Logger

	rebuildMutex sync.Mutex
	docker       DockerClient
	dnsUDPclient DNSClient
	dnsTCPclient DNSClient
}

func (a *App) run(nameservers string) error {
	// default to /etc/resolv.conf if explicit nameservers were not provided
	if nameservers == "" {
		cc, err := dns.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			return stackerr.Wrap(err)
		}
		for _, s := range cc.Servers {
			a.Nameservers = append(a.Nameservers, net.JoinHostPort(s, cc.Port))
		}
	} else {
		a.Nameservers = strings.Split(nameservers, ",")
	}

	c, err := dockerutil.BestEffortDockerClient()
	if err != nil {
		return err
	}

	a.docker = c
	a.dnsTCPclient = &dns.Client{Net: "tcp", SingleInflight: true}
	a.dnsUDPclient = &dns.Client{Net: "udp", SingleInflight: true}

	// monitor first, then rebuild to ensure we dont miss any updates
	a.docker.StartMonitorEvents(a.onDockerEvent, make(chan error, 1))
	if err := a.rebuild(); err != nil {
		return err
	}

	var eg errgroup.Group
	eg.Add(2)
	go a.listenAndServe("udp", &eg)
	go a.listenAndServe("tcp", &eg)
	if err := eg.Wait(); err != nil {
		return err
	}

	return nil
}

func (a *App) listenAndServe(net string, eg *errgroup.Group) {
	defer eg.Done()
	if err := dns.ListenAndServe(a.Addr, net, a); err != nil {
		eg.Error(stackerr.Wrap(err))
	}
}

func (a *App) rebuild() error {
	// only 1 rebuild at a time
	a.rebuildMutex.Lock()
	defer a.rebuildMutex.Unlock()

	containers, err := a.docker.ListContainers(false, false, "")
	if err != nil {
		return stackerr.Wrap(err)
	}

	overrides := make(Overrides)
	for _, container := range containers {
		var ci *dockerclient.ContainerInfo
		for _, name := range container.Names {
			if strings.HasPrefix(name, a.Prefix) && strings.Index(name[1:], "/") == -1 {
				if ci == nil {
					if ci, err = a.docker.InspectContainer(container.Id); err != nil {
						return stackerr.Wrap(err)
					}
				}

				var h Host
				if ci.NetworkSettings.IpAddress != "" {
					ip := net.ParseIP(ci.NetworkSettings.IpAddress)
					if ip == nil {
						return stackerr.Newf(
							"invalid IP address from docker %q for %q",
							ci.NetworkSettings.IpAddress,
							name,
						)
					}
					h.IPv4 = ip
				}
				if ci.NetworkSettings.GlobalIPv6Address != "" {
					ip := net.ParseIP(ci.NetworkSettings.GlobalIPv6Address)
					if ip == nil {
						return stackerr.Newf(
							"invalid IP address from docker %q for %q",
							ci.NetworkSettings.GlobalIPv6Address,
							name,
						)
					}
					h.IPv6 = ip
				}
				overrides[strings.TrimPrefix(name, a.Prefix)+a.Domain+"."] = h
			}
		}
	}
	a.Overrides.Store(overrides)
	return nil
}

func (a *App) onDockerEvent(e *dockerclient.Event, errch chan error, args ...interface{}) {
	go func() {
		if err := a.rebuild(); err != nil {
			a.Log.Printf("error rebuilding overrides: %s\n", err)
		}
	}()
}

// ServeDNS serves DNS requests.
func (a *App) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	if req.Opcode == dns.OpcodeQuery {
		a.handleDNSQuery(w, req)
		return
	}
	a.forwardDNSRequest(w, req)
}

func (a *App) handleDNSQuery(w dns.ResponseWriter, req *dns.Msg) {
	// TODO: do we need to handle 1 of many questions and forward the rest?
	if len(req.Question) == 1 {
		o := a.Overrides.Load().(Overrides)
		q := req.Question[0]
		if h, ok := o[q.Name]; ok {
			switch q.Qtype {
			default:
				a.Log.Printf("unhandled Qtype %d for overridden host %q\n", q.Qtype, q.Name)
			case qtypeIPv4:
				if h.IPv4 == nil {
					a.writeNotFoundResponse(w, req)
					return
				}

				res := new(dns.Msg)
				res.SetReply(req)
				res.RecursionAvailable = true
				res.Answer = []dns.RR{
					&dns.A{
						A: h.IPv4,
						Hdr: dns.RR_Header{
							Name:     q.Name,
							Rrtype:   qtypeIPv4,
							Class:    1,
							Ttl:      100,
							Rdlength: 4,
						},
					},
				}
				w.WriteMsg(res)
				return
			case qtypeIPv6:
				if h.IPv6 == nil {
					a.writeNotFoundResponse(w, req)
					return
				}

				res := new(dns.Msg)
				res.SetReply(req)
				res.RecursionAvailable = true
				res.Answer = []dns.RR{
					&dns.AAAA{
						AAAA: h.IPv6,
						Hdr: dns.RR_Header{
							Name:     q.Name,
							Rrtype:   qtypeIPv6,
							Class:    1,
							Ttl:      100,
							Rdlength: 16,
						},
					},
				}
				w.WriteMsg(res)
				return
			}
		}
	}
	a.forwardDNSRequest(w, req)
}

func (a *App) writeNotFoundResponse(w dns.ResponseWriter, req *dns.Msg) {
	res := new(dns.Msg)
	res.SetReply(req)
	res.RecursionAvailable = true
	res.Rcode = dns.RcodeNameError
	w.WriteMsg(res)
}

func (a *App) forwardDNSRequest(w dns.ResponseWriter, req *dns.Msg) {
	// proxy using the same protocol
	exchange := a.dnsUDPclient.Exchange
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		exchange = a.dnsTCPclient.Exchange
	}

	// try all nameservers in order
	for _, ns := range a.Nameservers {
		res, _, err := exchange(req, ns)
		if err == nil {
			res.Compress = true
			w.WriteMsg(res)
			return
		}
		a.Log.Printf("error from %q: %v", ns, err)
	}

	// failed
	m := new(dns.Msg)
	m.SetReply(req)
	m.SetRcode(req, dns.RcodeServerFailure)
	w.WriteMsg(m)
}

func main() {
	nameservers := flag.String(
		"nameservers", "", "nameservers to foward unhandled requests to")
	a := App{Log: log.New(os.Stderr, "", log.LstdFlags)}
	flag.StringVar(&a.Addr, "addr", ":53", "dns address to listen on")
	flag.StringVar(&a.Prefix, "prefix", "/", "docker container prefix to include")
	flag.StringVar(&a.Domain, "domain", ".local", "domain suffix for hostname")
	flag.Parse()

	if err := a.run(*nameservers); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
