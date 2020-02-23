// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package resolvers

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/OWASP/Amass/v3/limits"
	amassnet "github.com/OWASP/Amass/v3/net"
	amassdns "github.com/OWASP/Amass/v3/net/dns"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/miekg/dns"
)

var (
	retryCodes = []int{
		dns.RcodeRefused,
		dns.RcodeServerFailure,
		dns.RcodeNotImplemented,
	}

	maxRetries = 3
)

// ResolverPool manages many DNS resolvers for high-performance use, such as brute forcing attacks.
type ResolverPool struct {
	Resolvers []Resolver
	Done      chan struct{}
	// Logger for error messages
	Log          *log.Logger
	wildcardLock sync.Mutex
	wildcards    map[string]*wildcard
	// Domains discovered by the SubdomainToDomain function
	domainLock     sync.Mutex
	domainCache    map[string]struct{}
	hasBeenStopped bool
}

// SetupResolverPool initializes a ResolverPool with the type of resolvers indicated by the parameters.
func SetupResolverPool(addrs []string, scoring, ratemon bool, log *log.Logger) *ResolverPool {
	if len(addrs) <= 0 {
		return nil
	}

	// Do not allow the number of resolvers to exceed the ulimit
	temp := addrs
	addrs = []string{}
	max := int(float64(limits.GetFileLimit())*0.7) / 2
	for i, r := range temp {
		if i > max {
			break
		}
		addrs = append(addrs, r)
	}

	finished := make(chan Resolver, 100)
	for _, addr := range addrs {
		go func(ip string, ch chan Resolver) {
			if n := NewBaseResolver(ip); n != nil {
				ch <- n
				return
			}
			ch <- nil
		}(addr, finished)
	}

	l := len(addrs)
	var resolvers []Resolver
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
loop:
	for i := 0; i < l; i++ {
		select {
		case <-t.C:
			break loop
		case r := <-finished:
			if r == nil {
				continue loop
			}
			if scoring {
				if r = NewScoredResolver(r); r == nil {
					continue loop
				}
			}
			if ratemon {
				if r = NewRateMonitoredResolver(r); r == nil {
					continue loop
				}
			}
			resolvers = append(resolvers, r)
		}
	}

	if len(resolvers) == 0 {
		return nil
	}

	return NewResolverPool(resolvers, log)
}

// NewResolverPool initializes a ResolverPool that uses the provided Resolvers.
func NewResolverPool(res []Resolver, logger *log.Logger) *ResolverPool {
	rp := &ResolverPool{
		Resolvers:   res,
		Done:        make(chan struct{}, 2),
		Log:         logger,
		wildcards:   make(map[string]*wildcard),
		domainCache: make(map[string]struct{}),
	}

	// Assign a null logger when one is not provided
	if rp.Log == nil {
		rp.Log = log.New(ioutil.Discard, "", 0)
	}

	rp.SanityChecks()
	return rp
}

// Stop calls the Stop method for each Resolver object in the pool.
func (rp *ResolverPool) Stop() error {
	rp.hasBeenStopped = true

	for _, r := range rp.Resolvers {
		r.Stop()
	}

	rp.Resolvers = []Resolver{}
	return nil
}

// IsStopped implements the Resolver interface.
func (rp *ResolverPool) IsStopped() bool {
	return rp.hasBeenStopped
}

// Address implements the Resolver interface.
func (rp *ResolverPool) Address() string {
	return "N/A"
}

// Port implements the Resolver interface.
func (rp *ResolverPool) Port() int {
	return 0
}

// Available returns true if the Resolver can handle another DNS request.
func (rp *ResolverPool) Available() (bool, error) {
	return true, nil
}

// Stats returns performance counters.
func (rp *ResolverPool) Stats() map[int]int64 {
	stats := make(map[int]int64)

	for _, r := range rp.Resolvers {
		for k, v := range r.Stats() {
			if cur, found := stats[k]; found {
				stats[k] = cur + v
			} else {
				stats[k] = v
			}
		}
	}

	return stats
}

// WipeStats clears the performance counters.
func (rp *ResolverPool) WipeStats() {
	return
}

// ReportError implements the Resolver interface.
func (rp *ResolverPool) ReportError() {
	return
}

// SubdomainToDomain returns the first subdomain name of the provided
// parameter that responds to a DNS query for the NS record type.
func (rp *ResolverPool) SubdomainToDomain(name string) string {
	rp.domainLock.Lock()
	defer rp.domainLock.Unlock()

	var domain string
	// Obtain all parts of the subdomain name
	labels := strings.Split(strings.TrimSpace(name), ".")
	// Check the cache for all parts of the name
	for i := len(labels); i >= 0; i-- {
		sub := strings.Join(labels[i:], ".")

		if _, ok := rp.domainCache[sub]; ok {
			domain = sub
			break
		}
	}
	if domain != "" {
		return domain
	}
	// Check the DNS for all parts of the name
	for i := 0; i < len(labels)-1; i++ {
		sub := strings.Join(labels[i:], ".")

		if ns, _, err := rp.Resolve(context.TODO(), sub, "NS", PriorityHigh); err == nil {
			pieces := strings.Split(ns[0].Data, ",")
			rp.domainCache[pieces[0]] = struct{}{}
			domain = pieces[0]
			break
		}
	}
	return domain
}

// NextResolver returns a randomly selected Resolver from the pool that has availability.
func (rp *ResolverPool) NextResolver() Resolver {
	var attempts int
	max := len(rp.Resolvers)

	if max == 0 || rp.numUsableResolvers() == 0 {
		return nil
	}

	for {
		r := rp.Resolvers[rand.Int()%max]

		if stopped := r.IsStopped(); !stopped {
			return r
		}

		attempts++
		if attempts > max {
			// Check every resolver sequentially
			for _, r := range rp.Resolvers {
				if stopped := r.IsStopped(); !stopped {
					return r
				}
			}
			break
		}
	}

	return nil
}

// Reverse is performs reverse DNS queries using available Resolvers in the pool.
func (rp *ResolverPool) Reverse(ctx context.Context, addr string, priority int) (string, string, error) {
	var name, ptr string

	if ip := net.ParseIP(addr); amassnet.IsIPv4(ip) {
		ptr = amassdns.ReverseIP(addr) + ".in-addr.arpa"
	} else if amassnet.IsIPv6(ip) {
		ptr = amassdns.IPv6NibbleFormat(ip.String()) + ".ip6.arpa"
	} else {
		return ptr, "", &ResolveError{
			Err:   fmt.Sprintf("Invalid IP address parameter: %s", addr),
			Rcode: 100,
		}
	}

	answers, _, err := rp.Resolve(ctx, ptr, "PTR", priority)
	if err != nil {
		return ptr, name, err
	}

	for _, a := range answers {
		if a.Type == 12 {
			name = RemoveLastDot(a.Data)
			break
		}
	}

	if name == "" {
		err = &ResolveError{
			Err:   fmt.Sprintf("PTR record not found for IP address: %s", addr),
			Rcode: 100,
		}
	} else if strings.HasSuffix(name, ".in-addr.arpa") || strings.HasSuffix(name, ".ip6.arpa") {
		err = &ResolveError{
			Err:   fmt.Sprintf("Invalid target in PTR record answer: %s", name),
			Rcode: 100,
		}
	}

	return ptr, name, err
}

// Resolve performs a DNS request using available Resolvers in the pool.
func (rp *ResolverPool) Resolve(ctx context.Context, name, qtype string, priority int) ([]requests.DNSAnswer, bool, error) {
	var attempts int
	switch priority {
	case PriorityCritical:
		attempts = 1000
	case PriorityHigh:
		attempts = 100
	case PriorityLow:
		attempts = 25
	}

	// This loop ensures the correct number of attempts of the DNS query
	for count := 0; count < attempts; count++ {
		r := rp.NextResolver()
		if r == nil {
			// Give the system a chance to breathe before trying again
			time.Sleep(time.Duration(randomInt(1000, 1500)) * time.Millisecond)
			continue
		}

		success := true
		ans, again, err := r.Resolve(ctx, name, qtype, priority)
		if again {
			success = false
		} else if err != nil {
			if rc := (err.(*ResolveError)).Rcode; rc == NotAvailableRcode || 
				rc == dns.RcodeServerFailure || rc == dns.RcodeRefused || rc == dns.RcodeNotImplemented {
				success = false
			}
		}

		if success {
			return ans, again, err
		}
	}

	return []requests.DNSAnswer{}, false, &ResolveError{
		Err: fmt.Sprintf("Resolver: %d attempts for %s type %s returned 0 results", attempts, name, qtype),
	}
}

func (rp *ResolverPool) numUsableResolvers() int {
	var num int

	for _, r := range rp.Resolvers {
		if stopped := r.IsStopped(); !stopped {
			num++
		}
	}
	return num
}

func randomInt(min, max int) int {
	return min + rand.Intn((max-min)+1)
}
