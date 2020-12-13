// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package resolvers

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/OWASP/Amass/v3/eventbus"
	amassnet "github.com/OWASP/Amass/v3/net"
	amassdns "github.com/OWASP/Amass/v3/net/dns"
	"github.com/OWASP/Amass/v3/queue"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/miekg/dns"
)

// The priority levels for DNS resolution.
const (
	PriorityLow int = iota
	PriorityHigh
	PriorityCritical
)

// ResolverErrRcode is our made up rcode to indicate an interface error.
const ResolverErrRcode = 100

// NotAvailableRcode is our made up rcode to indicate an availability problem.
const NotAvailableRcode = 256

const timeoutDuration = 500 * time.Millisecond

// ResolveError contains the Rcode returned during the DNS query.
type ResolveError struct {
	Err   string
	Rcode int
}

func (e *ResolveError) Error() string {
	return e.Err
}

type resolveRequest struct {
	ID        uint16
	Timestamp time.Time
	Name      string
	Qtype     uint16
	Msg       *dns.Msg
	Result    chan *resolveResult
}

type resolveResult struct {
	Msg     *dns.Msg
	Records []requests.DNSAnswer
	Again   bool
	Err     error
}

func makeResolveResult(msg *dns.Msg, rec []requests.DNSAnswer, again bool, err string, rcode int) *resolveResult {
	return &resolveResult{
		Msg:     msg,
		Records: rec,
		Again:   again,
		Err: &ResolveError{
			Err:   err,
			Rcode: rcode,
		},
	}
}

func checkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return &ResolveError{
			Err:   "The request context was cancelled",
			Rcode: ResolverErrRcode,
		}
	default:
	}
	return nil
}

// Resolver is the object type for performing DNS resolutions.
type Resolver interface {
	fmt.Stringer

	// Address returns the IP address where the resolver is located
	Address() string

	// Port returns the port number used to communicate with the resolver
	Port() int

	// Resolve performs DNS queries using the Resolver
	Resolve(ctx context.Context, name, qtype string, priority int, retry Retry) ([]requests.DNSAnswer, error)

	// Reverse is performs reverse DNS queries using the Resolver
	Reverse(ctx context.Context, addr string, priority int, retry Retry) (string, string, error)

	// NsecTraversal attempts to retrieve a DNS zone using NSEC-walking
	NsecTraversal(ctx context.Context, domain string, priority int) ([]string, bool, error)

	// Available returns true if the Resolver can handle another DNS request
	Available() (bool, error)

	// Stats returns performance counters
	Stats() map[int]int64
	WipeStats()

	// ReportError indicates to the Resolver that it delivered an erroneous response
	ReportError()

	// MatchesWildcard returns true if the request provided resolved to a DNS wildcard
	MatchesWildcard(ctx context.Context, req *requests.DNSRequest) bool

	// GetWildcardType returns the DNS wildcard type for the provided subdomain name
	GetWildcardType(ctx context.Context, req *requests.DNSRequest) int

	// SubdomainToDomain returns the first subdomain name of the provided
	// parameter that responds to a DNS query for the NS record type
	SubdomainToDomain(name string) string

	// Stop the Resolver
	Stop() error
	IsStopped() bool
}

// BaseResolver performs DNS queries on a single resolver at high-performance.
type BaseResolver struct {
	Done             chan struct{}
	xchgQueue        *queue.Queue
	stateChannels    *resolverStateChans
	wildcardChannels *wildcardChans
	rotationChannels *rotationChans
	xchgsChannels    *xchgsChans
	readMsgs         *queue.Queue
	address          string
	port             string
}

// NewBaseResolver initializes a Resolver that send DNS queries to the provided IP address.
func NewBaseResolver(addr string) *BaseResolver {
	port := "53"
	parts := strings.Split(addr, ":")
	if len(parts) == 2 {
		addr = parts[0]
		port = parts[1]
	}

	r := &BaseResolver{
		Done:          make(chan struct{}, 2),
		xchgQueue:     queue.NewQueue(),
		stateChannels: initStateManagement(),
		wildcardChannels: &wildcardChans{
			WildcardReq:     queue.NewQueue(),
			IPsAcrossLevels: make(chan *ipsAcrossLevels, 10),
			TestResult:      make(chan *testResult, 10),
		},
		rotationChannels: initRotationChans(),
		xchgsChannels:    initXchgsManagement(),
		readMsgs:         queue.NewQueue(),
		address:          addr,
		port:             port,
	}

	go r.periodicRotations(r.rotationChannels)
	r.rotateConnections()
	go r.manageWildcards(r.wildcardChannels)
	go r.sendQueries()
	go r.checkForTimeouts()
	go r.readMessages(false)
	go r.readMessages(true)
	go r.processReadMessages()
	return r
}

// Address implements the Resolver interface.
func (r *BaseResolver) Address() string {
	return r.address
}

// Port implements the Resolver interface.
func (r *BaseResolver) Port() int {
	if p, err := strconv.Atoi(r.port); err == nil {
		return p
	}

	return 0
}

// String implements the Stringer interface.
func (r *BaseResolver) String() string {
	return r.Address() + ":" + strconv.Itoa(r.Port())
}

// Available always returns true.
func (r *BaseResolver) Available() (bool, error) {
	if r.IsStopped() {
		msg := fmt.Sprintf("DNS: Resolver %s has been stopped", r.String())

		return false, &ResolveError{Err: msg}
	}

	return true, nil
}

// ReportError indicates to the Resolver that it delivered an erroneous response.
func (r *BaseResolver) ReportError() {}

// SubdomainToDomain returns the first subdomain name of the provided
// parameter that responds to a DNS query for the NS record type.
func (r *BaseResolver) SubdomainToDomain(name string) string {
	return name
}

func (r *BaseResolver) returnRequest(req *resolveRequest, res *resolveResult) {
	req.Result <- res
}

// Resolve performs a DNS query using the Resolver.
func (r *BaseResolver) Resolve(ctx context.Context, name, qtype string, priority int, retry Retry) ([]requests.DNSAnswer, error) {
	if priority != PriorityCritical && priority != PriorityHigh && priority != PriorityLow {
		return []requests.DNSAnswer{}, &ResolveError{
			Err:   fmt.Sprintf("Resolver: Invalid priority parameter: %d", priority),
			Rcode: ResolverErrRcode,
		}
	}

	if avail, err := r.Available(); !avail {
		return []requests.DNSAnswer{}, &ResolveError{
			Err:   err.Error(),
			Rcode: ResolverErrRcode,
		}
	}

	qt, err := textToTypeNum(qtype)
	if err != nil {
		return nil, &ResolveError{
			Err:   err.Error(),
			Rcode: ResolverErrRcode,
		}
	}

	var bus *eventbus.EventBus
	if b := ctx.Value(requests.ContextEventBus); b != nil {
		bus = b.(*eventbus.EventBus)
	}

	again := true
	var times int
	var ans []requests.DNSAnswer
	for again {
		err = checkContext(ctx)
		if err != nil {
			break
		}

		times++
		msg := queryMessage(r.getID(), name, qt)
		result := r.queueQuery(msg, name, qt, priority)
		err = result.Err
		ans = result.Records
		// Report the completion of the DNS query
		if bus != nil {
			rcode := dns.RcodeSuccess
			if err != nil {
				rcode = (err.(*ResolveError)).Rcode
			}

			bus.Publish(requests.ResolveCompleted, eventbus.PriorityCritical, time.Now(), rcode)
		}

		if err == nil || retry == nil {
			break
		}

		resp := result.Msg
		rcode := (result.Err.(*ResolveError)).Rcode
		if resp == nil {
			resp = msg
			resp.Rcode = rcode
		}
		again = retry(times, priority, resp)
	}

	return ans, err
}

// Reverse is performs reverse DNS queries using the Resolver.
func (r *BaseResolver) Reverse(ctx context.Context, addr string, priority int, retry Retry) (string, string, error) {
	var name, ptr string

	if ip := net.ParseIP(addr); amassnet.IsIPv4(ip) {
		ptr = amassdns.ReverseIP(addr) + ".in-addr.arpa"
	} else if amassnet.IsIPv6(ip) {
		ptr = amassdns.IPv6NibbleFormat(ip.String()) + ".ip6.arpa"
	} else {
		return ptr, "", &ResolveError{
			Err:   fmt.Sprintf("Invalid IP address parameter: %s", addr),
			Rcode: ResolverErrRcode,
		}
	}

	answers, err := r.Resolve(ctx, ptr, "PTR", priority, retry)
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
			Rcode: ResolverErrRcode,
		}
	} else if strings.HasSuffix(name, ".in-addr.arpa") || strings.HasSuffix(name, ".ip6.arpa") {
		err = &ResolveError{
			Err:   fmt.Sprintf("Invalid target in PTR record answer: %s", name),
			Rcode: ResolverErrRcode,
		}
	}
	return ptr, name, err
}

func (r *BaseResolver) checkForTimeouts() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
loop:
	for {
		select {
		case <-r.Done:
			break loop
		case <-t.C:
			var count int
			// Discover requests that have timed out and remove them from the map
			for _, id := range r.allTimeoutIDs() {
				if req := r.pullRequestAfterTimeout(id, timeoutDuration); req != nil {
					count++
					estr := fmt.Sprintf("DNS query on resolver %s, for %s type %d timed out",
						r.address, req.Name, req.Qtype)
					r.returnRequest(req, makeResolveResult(nil, nil, true, estr, TimeoutRcode))
				}
			}
			// Complete handling of the timed out requests
			r.updateTimeouts(count)
		}
	}

	for _, id := range r.allTimeoutIDs() {
		if req := r.pullRequest(id); req != nil {
			estr := fmt.Sprintf("DNS resolver %s has stopped", r.address)
			r.returnRequest(req, makeResolveResult(nil, nil, false, estr, ResolverErrRcode))
		}
	}
}

func (r *BaseResolver) queueQuery(msg *dns.Msg, name string, qt uint16, p int) *resolveResult {
	resultChan := make(chan *resolveResult, 2)

	priority := queue.PriorityNormal
	switch p {
	case PriorityCritical:
		priority = queue.PriorityCritical
	case PriorityHigh:
		priority = queue.PriorityHigh
	case PriorityLow:
		priority = queue.PriorityLow
	}

	// Use the correct queue based on the priority
	r.xchgQueue.AppendPriority(&resolveRequest{
		Name:   name,
		Qtype:  qt,
		Msg:    msg,
		Result: resultChan,
	}, priority)

	result := <-resultChan
	r.updateStat(QueryCompletions, 1)
	return result
}

func (r *BaseResolver) sendQueries() {
	each := func(element interface{}) {
		r.writeMessage(element.(*resolveRequest))
	}

	for {
		select {
		case <-r.Done:
			return
		case <-r.xchgQueue.Signal:
			r.xchgQueue.Process(each)
		}
	}
}

func (r *BaseResolver) writeMessage(req *resolveRequest) {
	co := r.currentConnection()

	r.queueRequest(req.Msg.MsgHdr.Id, req)
	co.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := co.WriteMsg(req.Msg); err != nil {
		r.pullRequest(req.Msg.MsgHdr.Id)
		estr := fmt.Sprintf("DNS error: Failed to write query msg: %v", err)
		r.returnRequest(req, makeResolveResult(nil, nil, true, estr, TimeoutRcode))
		return
	}
	r.updateAttempts()
}

func (r *BaseResolver) readMessages(last bool) {
	for {
		select {
		case <-r.Done:
			return
		default:
			var co *dns.Conn
			if last {
				co = r.lastConnection()
			} else {
				co = r.currentConnection()
			}

			if co != nil {
				co.SetReadDeadline(time.Now().Add(2 * time.Second))
				if read, err := co.ReadMsg(); err == nil && read != nil {
					r.readMsgs.Append(read)
				}
			}
		}
	}
}

func (r *BaseResolver) processReadMessages() {
	each := func(element interface{}) {
		r.processMessage(element.(*dns.Msg))
	}

	for {
		select {
		case <-r.Done:
			return
		case <-r.readMsgs.Signal:
			r.readMsgs.Process(each)
		}
	}
}

func (r *BaseResolver) processMessage(m *dns.Msg) {
	req := r.pullRequest(m.MsgHdr.Id)
	if req == nil {
		return
	}

	r.updateRTT(time.Since(req.Timestamp))
	r.updateStat(m.Rcode, 1)
	// Check that the query was successful
	if m.Rcode != dns.RcodeSuccess {
		var again bool
		for _, code := range RetryCodes {
			if m.Rcode == code {
				again = true
				break
			}
		}
		estr := fmt.Sprintf("DNS query on resolver %s, for %s type %d returned error %s",
			r.address, req.Name, req.Qtype, dns.RcodeToString[m.Rcode])
		r.returnRequest(req, makeResolveResult(m, nil, again, estr, m.Rcode))
		return
	}

	if m.Truncated {
		go r.tcpExchange(m.MsgHdr.Id, req)
		return
	}

	r.finishProcessing(m, req)
}

func (r *BaseResolver) finishProcessing(m *dns.Msg, req *resolveRequest) {
	var answers []requests.DNSAnswer

	for _, a := range extractRawData(m, req.Qtype) {
		answers = append(answers, requests.DNSAnswer{
			Name: a.Name,
			Type: int(req.Qtype),
			TTL:  0,
			Data: strings.TrimSpace(a.Value),
		})
	}

	if len(answers) == 0 {
		estr := fmt.Sprintf("DNS query on resolver %s, for %s type %d returned 0 records",
			r.address, req.Name, req.Qtype)
		r.returnRequest(req, makeResolveResult(m, nil, false, estr, m.Rcode))
		return
	}

	r.returnRequest(req, &resolveResult{
		Msg:     m,
		Records: answers,
		Again:   false,
		Err:     nil,
	})
}

func (r *BaseResolver) tcpExchange(id uint16, req *resolveRequest) {
	if len(req.Msg.Question) == 0 {
		return
	}
	msg := queryMessage(r.getID(), req.Msg.Question[0].Name, req.Msg.Question[0].Qtype)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	req.Msg = msg
	conn, err := amassnet.DialContext(ctx, "tcp", r.address+":"+r.port)
	if err != nil {
		estr := fmt.Sprintf("DNS: Failed to obtain TCP connection to %s:%s: %v", r.address, r.port, err)
		r.returnRequest(req, makeResolveResult(nil, nil, true, estr, NotAvailableRcode))
		return
	}
	defer conn.Close()

	co := &dns.Conn{Conn: conn}
	co.SetWriteDeadline(time.Now().Add(time.Minute))
	if err := co.WriteMsg(msg); err != nil {
		estr := fmt.Sprintf("DNS error: Failed to write query msg: %v", err)
		r.returnRequest(req, makeResolveResult(nil, nil, true, estr, TimeoutRcode))
		return
	}

	co.SetReadDeadline(time.Now().Add(time.Minute))
	read, err := co.ReadMsg()
	if read == nil || err != nil {
		estr := fmt.Sprintf("DNS error: Failed to read the reply msg: %v", err)
		r.returnRequest(req, makeResolveResult(read, nil, true, estr, TimeoutRcode))
		return
	}

	r.finishProcessing(read, req)
}
