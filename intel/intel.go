// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package intel

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/OWASP/Amass/v3/config"
	eb "github.com/OWASP/Amass/v3/eventbus"
	amassnet "github.com/OWASP/Amass/v3/net"
	"github.com/OWASP/Amass/v3/net/http"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/resolvers"
	"github.com/OWASP/Amass/v3/stringfilter"
	"github.com/OWASP/Amass/v3/stringset"
	"github.com/OWASP/Amass/v3/systems"
	"github.com/miekg/dns"
)

// Collection is the object type used to execute a open source information gathering with Amass.
type Collection struct {
	sync.Mutex

	Config *config.Config
	Bus    *eb.EventBus
	Sys    systems.System

	ctx context.Context

	srcsLock sync.Mutex
	srcs     stringset.Set

	// The channel that will receive the results
	Output chan *requests.Output

	// Broadcast channel that indicates no further writes to the output channel
	done              chan struct{}
	doneAlreadyClosed bool

	wg     sync.WaitGroup
	filter stringfilter.Filter

	lastLock sync.Mutex
	last     time.Time
}

// NewCollection returns an initialized Collection object that has not been started yet.
func NewCollection(sys systems.System) *Collection {
	c := &Collection{
		Config: config.NewConfig(),
		Bus:    eb.NewEventBus(),
		Sys:    sys,
		srcs:   stringset.New(),
		Output: make(chan *requests.Output, 100),
		done:   make(chan struct{}, 2),
		last:   time.Now(),
	}

	return c
}

// Done safely closes the done broadcast channel.
func (c *Collection) Done() {
	c.Lock()
	defer c.Unlock()

	if !c.doneAlreadyClosed {
		c.doneAlreadyClosed = true
		close(c.done)
	}
}

// HostedDomains uses open source intelligence to discover root domain names in the target infrastructure.
func (c *Collection) HostedDomains() error {
	if c.Output == nil {
		return errors.New("The intelligence collection did not have an output channel")
	} else if err := c.Config.CheckSettings(); err != nil {
		return err
	}

	// Setup the stringset of included data sources
	c.srcsLock.Lock()
	srcs := stringset.New()
	c.srcs.Intersect(srcs)
	srcs.InsertMany(c.Config.SourceFilter.Sources...)
	for _, src := range c.Sys.DataSources() {
		c.srcs.Insert(src.String())
	}
	if srcs.Len() > 0 && c.Config.SourceFilter.Include {
		c.srcs.Intersect(srcs)
	} else {
		c.srcs.Subtract(srcs)
	}
	c.srcsLock.Unlock()

	// Setup the context used throughout the collection
	ctx := context.WithValue(context.Background(), requests.ContextConfig, c.Config)
	c.ctx = context.WithValue(ctx, requests.ContextEventBus, c.Bus)

	c.Bus.Subscribe(requests.SetActiveTopic, c.updateLastActive)
	defer c.Bus.Unsubscribe(requests.SetActiveTopic, c.updateLastActive)
	c.Bus.Subscribe(requests.ResolveCompleted, c.resolution)
	defer c.Bus.Unsubscribe(requests.ResolveCompleted, c.resolution)

	if c.Config.Timeout > 0 {
		time.AfterFunc(time.Duration(c.Config.Timeout)*time.Minute, func() {
			c.Config.Log.Printf("Enumeration exceeded provided timeout")
			close(c.Output)
		})
	}

	c.filter = stringfilter.NewStringFilter()
	// Start the address ranges
	for _, addr := range c.Config.Addresses {
		if c.Sys.PerformDNSQuery() == nil {
			c.wg.Add(1)
			go c.investigateAddr(addr.String())
		}
	}

	for _, cidr := range append(c.Config.CIDRs, c.asnsToCIDRs()...) {
		// Skip IPv6 netblocks, since they are simply too large
		if ip := cidr.IP.Mask(cidr.Mask); amassnet.IsIPv6(ip) {
			continue
		}

		for _, addr := range amassnet.AllHosts(cidr) {
			if c.Sys.PerformDNSQuery() == nil {
				c.wg.Add(1)
				go c.investigateAddr(addr.String())
			}
		}
	}

	c.wg.Wait()
	time.Sleep(5 * time.Second)
	close(c.Output)
	return nil
}

func (c *Collection) lastActive() time.Time {
	c.lastLock.Lock()
	defer c.lastLock.Unlock()

	return c.last
}

func (c *Collection) updateLastActive(srv string) {
	go func(t time.Time) {
		c.lastLock.Lock()
		defer c.lastLock.Unlock()

		c.last = t
	}(time.Now())
}

func (c *Collection) resolution(t time.Time, rcode int) {
	go func(t time.Time) {
		c.lastLock.Lock()
		defer c.lastLock.Unlock()

		if t.After(c.last) {
			c.last = t
		}
	}(t)
}

func (c *Collection) investigateAddr(addr string) {
	defer c.wg.Done()

	ip := net.ParseIP(addr)
	if ip == nil {
		return
	}

	addrinfo := requests.AddressInfo{Address: ip}
	if _, answer, err := c.Sys.Pool().Reverse(c.ctx, addr, resolvers.PriorityLow, func(times int, priority int, msg *dns.Msg) bool {
		var retry bool

		if resolvers.RetryPolicy(times, priority, msg) {
			c.Sys.PerformDNSQuery()
			retry = true
		}
		return retry
	}); err == nil {
		if d := strings.TrimSpace(c.Sys.Pool().SubdomainToDomain(answer)); d != "" {
			if !c.filter.Duplicate(d) {
				c.Output <- &requests.Output{
					Name:      d,
					Domain:    d,
					Addresses: []requests.AddressInfo{addrinfo},
					Tag:       requests.DNS,
					Sources:   []string{"Reverse DNS"},
				}
			}
		}
	}

	if !c.Config.Active {
		return
	}

	for _, name := range http.PullCertificateNames(addr, c.Config.Ports) {
		if n := strings.TrimSpace(name); n != "" {
			d := c.Sys.Pool().SubdomainToDomain(n)

			if !c.filter.Duplicate(d) {
				c.Output <- &requests.Output{
					Name:      n,
					Domain:    d,
					Addresses: []requests.AddressInfo{addrinfo},
					Tag:       requests.CERT,
					Sources:   []string{"Active Cert"},
				}
			}
		}
	}
}

func (c *Collection) asnsToCIDRs() []*net.IPNet {
	var cidrs []*net.IPNet

	if len(c.Config.ASNs) == 0 {
		return cidrs
	}

	last := time.Now()
	var lastLock sync.Mutex

	var setLock sync.Mutex
	cidrSet := stringset.New()
	fn := func(req *requests.ASNRequest) {
		lastLock.Lock()
		last = time.Now()
		lastLock.Unlock()

		setLock.Lock()
		cidrSet.Union(req.Netblocks)
		setLock.Unlock()
	}

	c.Bus.Subscribe(requests.NewASNTopic, fn)
	defer c.Bus.Unsubscribe(requests.NewASNTopic, fn)

	// Send the ASN requests to the data sources
	c.srcsLock.Lock()
	for _, src := range c.Sys.DataSources() {
		if !c.srcs.Has(src.String()) {
			continue
		}

		for _, asn := range c.Config.ASNs {
			src.ASNRequest(c.ctx, &requests.ASNRequest{ASN: asn})
		}
	}
	c.srcsLock.Unlock()

	// Wait for the ASN requests to return responses
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
loop:
	for {
		select {
		case <-c.done:
			return cidrs
		case <-t.C:
			lastLock.Lock()
			l := last
			lastLock.Unlock()

			if time.Since(l) > 20*time.Second {
				break loop
			}
		}
	}

	filter := stringfilter.NewStringFilter()
	// Do not return CIDRs that are already in the config
	for _, cidr := range c.Config.CIDRs {
		filter.Duplicate(cidr.String())
	}

	for _, netblock := range cidrSet.Slice() {
		_, ipnet, err := net.ParseCIDR(netblock)

		if err == nil && !filter.Duplicate(ipnet.String()) {
			cidrs = append(cidrs, ipnet)
		}
	}

	return cidrs
}

// ReverseWhois returns domain names that are related to the domains provided
func (c *Collection) ReverseWhois() error {
	if err := c.Config.CheckSettings(); err != nil {
		return err
	}

	filter := stringfilter.NewStringFilter()
	collect := func(req *requests.WhoisRequest) {
		for _, d := range req.NewDomains {
			if !filter.Duplicate(d) {
				c.Output <- &requests.Output{
					Name:    d,
					Domain:  d,
					Tag:     req.Tag,
					Sources: []string{req.Source},
				}
			}
		}
	}
	c.Bus.Subscribe(requests.NewWhoisTopic, collect)
	defer c.Bus.Unsubscribe(requests.NewWhoisTopic, collect)

	c.Bus.Subscribe(requests.SetActiveTopic, c.updateLastActive)
	defer c.Bus.Unsubscribe(requests.SetActiveTopic, c.updateLastActive)

	// Setup the stringset of included data sources
	c.srcsLock.Lock()
	srcs := stringset.New()
	c.srcs.Intersect(srcs)
	srcs.InsertMany(c.Config.SourceFilter.Sources...)
	for _, src := range c.Sys.DataSources() {
		c.srcs.Insert(src.String())
	}
	if srcs.Len() > 0 && c.Config.SourceFilter.Include {
		c.srcs.Intersect(srcs)
	} else {
		c.srcs.Subtract(srcs)
	}
	c.srcsLock.Unlock()

	// Setup the context used throughout the collection
	ctx := context.WithValue(context.Background(), requests.ContextConfig, c.Config)
	c.ctx = context.WithValue(ctx, requests.ContextEventBus, c.Bus)

	// Send the whois requests to the data sources
	c.srcsLock.Lock()
	for _, src := range c.Sys.DataSources() {
		if !c.srcs.Has(src.String()) {
			continue
		}

		for _, domain := range c.Config.Domains() {
			src.WhoisRequest(c.ctx, &requests.WhoisRequest{Domain: domain})
		}
	}
	c.srcsLock.Unlock()

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
loop:
	for {
		select {
		case <-c.done:
			break loop
		case <-t.C:
			if l := c.lastActive(); time.Since(l) > 10*time.Second {
				break loop
			}
		}
	}

	close(c.Output)
	return nil
}
