// Copyright 2017-2020 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package enum

import (
	"context"
	"fmt"
	"strings"

	"github.com/OWASP/Amass/v3/config"
	"github.com/OWASP/Amass/v3/eventbus"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/resolvers"
	"github.com/OWASP/Amass/v3/systems"
	"github.com/miekg/dns"
)

// InitialQueryTypes include the DNS record types that are
// initially requested for a discovered name
var InitialQueryTypes = []string{
	"CNAME",
	"TXT",
	"A",
	"AAAA",
}

// DNSService is the Service that handles all DNS name resolution requests within
// the architecture.
type DNSService struct {
	requests.BaseService

	SourceType string
	sys        systems.System
}

// NewDNSService returns he object initialized, but not yet started.
func NewDNSService(sys systems.System) *DNSService {
	ds := &DNSService{
		SourceType: requests.DNS,
		sys:        sys,
	}

	ds.BaseService = *requests.NewBaseService(ds, "DNS Service")
	return ds
}

// Type implements the Service interface.
func (ds *DNSService) Type() string {
	return ds.SourceType
}

// OnDNSRequest implements the Service interface.
func (ds *DNSService) OnDNSRequest(ctx context.Context, req *requests.DNSRequest) {
	ds.sys.Config().SemMaxDNSQueries.Acquire(1)
	go ds.processDNSRequest(ctx, req)
}

func (ds *DNSService) processDNSRequest(ctx context.Context, req *requests.DNSRequest) {
	defer ds.sys.Config().SemMaxDNSQueries.Release(1)

	if req == nil || req.Name == "" || req.Domain == "" {
		return
	}

	cfg := ctx.Value(requests.ContextConfig).(*config.Config)
	bus := ctx.Value(requests.ContextEventBus).(*eventbus.EventBus)
	if cfg == nil || bus == nil {
		return
	}

	bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, ds.String())

	if cfg.Blacklisted(req.Name) || (!requests.TrustedTag(req.Tag) &&
		ds.sys.Pool().GetWildcardType(ctx, req) == resolvers.WildcardTypeDynamic) {
		return
	}

	// Is this a root domain name?
	if req.Name == req.Domain {
		ds.subdomainQueries(ctx, req)
		ds.queryServiceNames(ctx, req)
	}

	req.Records = ds.queryInitialTypes(ctx, req)

	if len(req.Records) > 0 {
		ds.resolvedName(ctx, req)
	}
}

func (ds *DNSService) queryInitialTypes(ctx context.Context, req *requests.DNSRequest) []requests.DNSAnswer {
	var answers []requests.DNSAnswer

	bus := ctx.Value(requests.ContextEventBus).(*eventbus.EventBus)
	if bus == nil {
		return answers
	}

	for _, t := range InitialQueryTypes {
		bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, ds.String())

		if a, _, err := ds.sys.Pool().Resolve(ctx, req.Name, t, resolvers.PriorityLow); err == nil {
			answers = append(answers, a...)
		} else {
			ds.handleResolverError(ctx, err)
		}
	}

	return answers
}

func (ds *DNSService) handleResolverError(ctx context.Context, err error) {
	cfg := ctx.Value(requests.ContextConfig).(*config.Config)
	bus := ctx.Value(requests.ContextEventBus).(*eventbus.EventBus)
	if cfg == nil || bus == nil {
		return
	}

	rcode := (err.(*resolvers.ResolveError)).Rcode
	if cfg.Verbose || rcode == resolvers.NotAvailableRcode ||
		rcode == resolvers.TimeoutRcode || rcode == resolvers.ResolverErrRcode ||
		rcode == dns.RcodeRefused || rcode == dns.RcodeServerFailure || rcode == dns.RcodeNotImplemented {
		bus.Publish(requests.LogTopic, eventbus.PriorityHigh, fmt.Sprintf("DNS: %v", err))
	}
}

func (ds *DNSService) resolvedName(ctx context.Context, req *requests.DNSRequest) {
	cfg := ctx.Value(requests.ContextConfig).(*config.Config)
	bus := ctx.Value(requests.ContextEventBus).(*eventbus.EventBus)
	if cfg == nil || bus == nil {
		return
	}

	if !requests.TrustedTag(req.Tag) && ds.sys.Pool().MatchesWildcard(ctx, req) {
		return
	}

	bus.Publish(requests.NameResolvedTopic, eventbus.PriorityHigh, req)
}

// OnSubdomainDiscovered implements the Service interface.
func (ds *DNSService) OnSubdomainDiscovered(ctx context.Context, req *requests.DNSRequest, times int) {
	if req != nil && times == 1 {
		go ds.processSubdomain(ctx, req)
	}
}

func (ds *DNSService) processSubdomain(ctx context.Context, req *requests.DNSRequest) {
	cfg := ctx.Value(requests.ContextConfig).(*config.Config)
	if cfg == nil {
		return
	}

	if cfg.Blacklisted(req.Name) || (!requests.TrustedTag(req.Tag) &&
		ds.sys.Pool().GetWildcardType(ctx, req) == resolvers.WildcardTypeDynamic) {
		return
	}

	ds.subdomainQueries(ctx, req)
	ds.queryServiceNames(ctx, req)
}

func (ds *DNSService) subdomainQueries(ctx context.Context, req *requests.DNSRequest) {
	cfg := ctx.Value(requests.ContextConfig).(*config.Config)
	bus := ctx.Value(requests.ContextEventBus).(*eventbus.EventBus)
	if cfg == nil || bus == nil {
		return
	}

	answers := ds.queryInitialTypes(ctx, req)

	bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, ds.String())
	// Obtain the DNS answers for the NS records related to the domain
	if ans, _, err := ds.sys.Pool().Resolve(ctx, req.Name, "NS", resolvers.PriorityHigh); err == nil {
		for _, a := range ans {
			pieces := strings.Split(a.Data, ",")
			a.Data = pieces[len(pieces)-1]

			if cfg.Active {
				go ds.attemptZoneXFR(ctx, req.Name, req.Domain, a.Data)
				//go ds.attemptZoneWalk(domain, a.Data)
			}
			answers = append(answers, a)
		}
	} else {
		ds.handleResolverError(ctx, err)
	}

	bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, ds.String())
	// Obtain the DNS answers for the MX records related to the domain
	if ans, _, err := ds.sys.Pool().Resolve(ctx, req.Name, "MX", resolvers.PriorityHigh); err == nil {
		answers = append(answers, ans...)
	} else {
		ds.handleResolverError(ctx, err)
	}

	bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, ds.String())
	// Obtain the DNS answers for the SOA records related to the domain
	if ans, _, err := ds.sys.Pool().Resolve(ctx, req.Name, "SOA", resolvers.PriorityHigh); err == nil {
		for _, a := range ans {
			pieces := strings.Split(a.Data, ",")
			a.Data = pieces[len(pieces)-1]

			answers = append(answers, a)
		}
	} else {
		ds.handleResolverError(ctx, err)
	}

	bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, ds.String())
	// Obtain the DNS answers for the SPF records related to the domain
	if ans, _, err := ds.sys.Pool().Resolve(ctx, req.Name, "SPF", resolvers.PriorityHigh); err == nil {
		answers = append(answers, ans...)
	} else {
		ds.handleResolverError(ctx, err)
	}

	if len(answers) > 0 {
		bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, ds.String())

		ds.resolvedName(ctx, &requests.DNSRequest{
			Name:    req.Name,
			Domain:  req.Domain,
			Records: answers,
			Tag:     requests.DNS,
			Source:  "DNS",
		})
	}
}

func (ds *DNSService) attemptZoneXFR(ctx context.Context, sub, domain, server string) {
	cfg := ctx.Value(requests.ContextConfig).(*config.Config)
	bus := ctx.Value(requests.ContextEventBus).(*eventbus.EventBus)
	if cfg == nil || bus == nil {
		return
	}

	addr, err := ds.nameserverAddr(ctx, server)
	if addr == "" {
		bus.Publish(requests.LogTopic, eventbus.PriorityHigh, fmt.Sprintf("DNS: Zone XFR failed: %v", err))
		return
	}

	reqs, err := resolvers.ZoneTransfer(sub, domain, addr)
	if err != nil {
		bus.Publish(requests.LogTopic, eventbus.PriorityHigh,
			fmt.Sprintf("DNS: Zone XFR failed: %s: %v", server, err))
		return
	}

	for _, req := range reqs {
		ds.resolvedName(ctx, req)
	}
}

func (ds *DNSService) attemptZoneWalk(ctx context.Context, domain, server string) {
	cfg := ctx.Value(requests.ContextConfig).(*config.Config)
	bus := ctx.Value(requests.ContextEventBus).(*eventbus.EventBus)
	if cfg == nil || bus == nil {
		return
	}

	addr, err := ds.nameserverAddr(ctx, server)
	if addr == "" {
		bus.Publish(requests.LogTopic, eventbus.PriorityHigh, fmt.Sprintf("DNS: Zone Walk failed: %v", err))
		return
	}

	reqs, err := resolvers.NsecTraversal(domain, addr)
	if err != nil {
		bus.Publish(requests.LogTopic, eventbus.PriorityHigh,
			fmt.Sprintf("DNS: Zone Walk failed: %s: %v", server, err))
		return
	}

	for _, req := range reqs {
		ds.DNSRequest(ctx, req)
	}
}

func (ds *DNSService) nameserverAddr(ctx context.Context, server string) (string, error) {
	a, _, err := ds.sys.Pool().Resolve(ctx, server, "A", resolvers.PriorityHigh)
	if err != nil {
		a, _, err = ds.sys.Pool().Resolve(ctx, server, "AAAA", resolvers.PriorityHigh)
		if err != nil {
			return "", fmt.Errorf("DNS server has no A or AAAA record: %s: %v", server, err)
		}
	}
	return a[0].Data, nil
}

func (ds *DNSService) queryServiceNames(ctx context.Context, req *requests.DNSRequest) {
	bus := ctx.Value(requests.ContextEventBus).(*eventbus.EventBus)
	if bus == nil {
		return
	}

	for _, name := range popularSRVRecords {
		srvName := name + "." + req.Name

		bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, ds.String())

		if a, _, err := ds.sys.Pool().Resolve(ctx, srvName, "SRV", resolvers.PriorityHigh); err == nil {
			ds.resolvedName(ctx, &requests.DNSRequest{
				Name:    srvName,
				Domain:  req.Domain,
				Records: a,
				Tag:     requests.DNS,
				Source:  "DNS",
			})
		} else {
			ds.handleResolverError(ctx, err)
		}
	}
}
