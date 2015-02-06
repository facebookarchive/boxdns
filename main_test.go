package main

import (
	"errors"
	"io/ioutil"
	"log"
	"net"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/facebookgo/ensure"
	"github.com/miekg/dns"
	"github.com/samalba/dockerclient"
)

type fDockerClient struct {
	startMonitorEvents func(dockerclient.Callback, chan error, ...interface{})
	listContainers     func(all bool, size bool, filters string) ([]dockerclient.Container, error)
	inspectContainer   func(id string) (*dockerclient.ContainerInfo, error)
}

func (f fDockerClient) StartMonitorEvents(cb dockerclient.Callback, errch chan error, args ...interface{}) {
	f.startMonitorEvents(cb, errch, args...)
}

func (f fDockerClient) ListContainers(all bool, size bool, filters string) ([]dockerclient.Container, error) {
	return f.listContainers(all, size, filters)
}

func (f fDockerClient) InspectContainer(id string) (*dockerclient.ContainerInfo, error) {
	return f.inspectContainer(id)
}

type fDNSClient struct {
	exchange func(m *dns.Msg, a string) (*dns.Msg, time.Duration, error)
}

func (f fDNSClient) Exchange(m *dns.Msg, a string) (*dns.Msg, time.Duration, error) {
	return f.exchange(m, a)
}

type fDNSResponseWriter struct {
	dns.ResponseWriter
	remoteAddr net.Addr
	msg        *dns.Msg
}

func (f *fDNSResponseWriter) RemoteAddr() net.Addr {
	return f.remoteAddr
}

func (f *fDNSResponseWriter) WriteMsg(m *dns.Msg) error {
	f.msg = m
	return nil
}

func TestRebuildPrefixDomain(t *testing.T) {
	t.Parallel()
	const (
		prefix = "/p-"
		name   = "foo"
		ip     = "1.2.3.4"
	)
	a := App{
		Prefix: prefix,
		Domain: ".local",
		docker: fDockerClient{
			listContainers: func(bool, bool, string) ([]dockerclient.Container, error) {
				return []dockerclient.Container{
					{
						Id:    "xyz",
						Names: []string{"foo", prefix + name},
					},
				}, nil
			},
			inspectContainer: func(string) (*dockerclient.ContainerInfo, error) {
				var ci dockerclient.ContainerInfo
				ci.NetworkSettings.IpAddress = ip
				return &ci, nil
			},
		},
	}
	ensure.Nil(t, a.rebuild())
	ensure.DeepEqual(t, a.Overrides.Load(), Overrides{
		name + a.Domain + ".": net.ParseIP(ip),
	})
}

func TestRebuildListError(t *testing.T) {
	t.Parallel()
	const errMsg = "foo"
	a := App{
		docker: fDockerClient{
			listContainers: func(bool, bool, string) ([]dockerclient.Container, error) {
				return nil, errors.New(errMsg)
			},
		},
	}
	ensure.Err(t, a.rebuild(), regexp.MustCompile(errMsg))
}

func TestRebuildInspectError(t *testing.T) {
	t.Parallel()
	const errMsg = "foo"
	a := App{
		docker: fDockerClient{
			listContainers: func(bool, bool, string) ([]dockerclient.Container, error) {
				return []dockerclient.Container{
					{
						Id:    "xyz",
						Names: []string{"foo"},
					},
				}, nil
			},
			inspectContainer: func(string) (*dockerclient.ContainerInfo, error) {
				return nil, errors.New(errMsg)
			},
		},
	}
	ensure.Err(t, a.rebuild(), regexp.MustCompile(errMsg))
}

func TestRebuildInvalidIP(t *testing.T) {
	t.Parallel()
	a := App{
		docker: fDockerClient{
			listContainers: func(bool, bool, string) ([]dockerclient.Container, error) {
				return []dockerclient.Container{
					{
						Id:    "xyz",
						Names: []string{"foo"},
					},
				}, nil
			},
			inspectContainer: func(string) (*dockerclient.ContainerInfo, error) {
				var ci dockerclient.ContainerInfo
				ci.NetworkSettings.IpAddress = "a"
				return &ci, nil
			},
		},
	}
	ensure.Err(t, a.rebuild(), regexp.MustCompile("invalid IP"))
}

func TestServeDNSForwardUDP(t *testing.T) {
	t.Parallel()
	const ns = "a"
	res := new(dns.Msg)
	a := App{
		Nameservers: []string{ns},
		dnsUDPclient: fDNSClient{
			exchange: func(m *dns.Msg, a string) (*dns.Msg, time.Duration, error) {
				ensure.DeepEqual(t, a, ns)
				return res, time.Minute, nil
			},
		},
	}
	req := new(dns.Msg)
	req.Opcode = dns.OpcodeStatus
	var w fDNSResponseWriter
	a.ServeDNS(&w, req)
	ensure.DeepEqual(t, w.msg, res)
}

func TestServeDNSForwardTCP(t *testing.T) {
	t.Parallel()
	const ns = "a"
	res := new(dns.Msg)
	a := App{
		Nameservers:  []string{ns},
		dnsUDPclient: fDNSClient{},
		dnsTCPclient: fDNSClient{
			exchange: func(m *dns.Msg, a string) (*dns.Msg, time.Duration, error) {
				ensure.DeepEqual(t, a, ns)
				return res, time.Minute, nil
			},
		},
	}
	req := new(dns.Msg)
	req.Opcode = dns.OpcodeStatus
	w := fDNSResponseWriter{
		remoteAddr: &net.TCPAddr{},
	}
	a.ServeDNS(&w, req)
	ensure.DeepEqual(t, w.msg, res)
}

func TestServeDNSForwardTryAllAndFail(t *testing.T) {
	t.Parallel()
	ns := []string{"a", "b"}
	var current int32
	a := App{
		Log:         log.New(ioutil.Discard, "", log.LstdFlags),
		Nameservers: ns,
		dnsUDPclient: fDNSClient{
			exchange: func(m *dns.Msg, a string) (*dns.Msg, time.Duration, error) {
				ensure.DeepEqual(t, a, ns[int(atomic.AddInt32(&current, 1)-1)])
				return nil, time.Minute, errors.New("foo")
			},
		},
	}
	req := new(dns.Msg)
	req.Opcode = dns.OpcodeStatus
	var w fDNSResponseWriter
	a.ServeDNS(&w, req)
	ensure.DeepEqual(t, w.msg.Rcode, dns.RcodeServerFailure)
	ensure.DeepEqual(t, atomic.LoadInt32(&current), int32(2))
}

func TestServeDNSOverride(t *testing.T) {
	t.Parallel()
	const hostname = "foo.com."
	ip := net.ParseIP("1.2.3.4")
	var a App
	a.Overrides.Store(Overrides{hostname: ip})
	req := new(dns.Msg)
	req.Opcode = dns.OpcodeQuery
	req.Question = []dns.Question{{Name: hostname}}
	var w fDNSResponseWriter
	a.ServeDNS(&w, req)
	ensure.DeepEqual(t, w.msg.Answer, []dns.RR{
		&dns.A{
			A: ip,
			Hdr: dns.RR_Header{
				Name:     hostname,
				Rrtype:   1,
				Class:    1,
				Ttl:      100,
				Rdlength: 4,
			},
		},
	})
}

func TestServeDNSForwardNonOverrideQuery(t *testing.T) {
	t.Parallel()
	const ns = "a"
	res := new(dns.Msg)
	a := App{
		Nameservers: []string{ns},
		dnsUDPclient: fDNSClient{
			exchange: func(m *dns.Msg, a string) (*dns.Msg, time.Duration, error) {
				ensure.DeepEqual(t, a, ns)
				return res, time.Minute, nil
			},
		},
	}
	a.Overrides.Store(Overrides{})
	req := new(dns.Msg)
	req.Opcode = dns.OpcodeQuery
	req.Question = []dns.Question{{Name: "foo.com."}}
	var w fDNSResponseWriter
	a.ServeDNS(&w, req)
	ensure.DeepEqual(t, w.msg, res)
}
