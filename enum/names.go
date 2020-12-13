// Copyright 2017-2020 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package enum

import (
	"strings"
	"time"

	"github.com/OWASP/Amass/v3/eventbus"
	"github.com/OWASP/Amass/v3/queue"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/stringfilter"
)

// FQDNManager is the object type for taking in, generating and providing new DNS FQDNs.
type FQDNManager interface {
	// InputName shares a newly discovered FQDN with the NameManager
	InputName(req *requests.DNSRequest)

	// OutputNames requests new FQDNs from the NameManager
	OutputNames(num int) []*requests.DNSRequest

	// Obtain the number of names currently waiting to be output
	NameQueueLen() int

	// Send requests to data sources
	OutputRequests(num int) int

	// Obtain the number of requests currently waiting to be output
	RequestQueueLen() int

	Stop() error
}

// DomainManager handles the release of new domains names to data sources used in the enumeration.
type DomainManager struct {
	enum      *Enumeration
	queue     *queue.Queue
	curDomain string
	srcIndex  int
	filter    stringfilter.Filter
	last      time.Time
}

// NewDomainManager returns an initialized DomainManager.
func NewDomainManager(e *Enumeration) *DomainManager {
	return &DomainManager{
		enum:   e,
		queue:  queue.NewQueue(),
		filter: stringfilter.NewStringFilter(),
		last:   time.Now(),
	}
}

// InputName implements the FQDNManager interface.
func (r *DomainManager) InputName(req *requests.DNSRequest) {
	if req == nil || req.Name == "" || req.Domain == "" {
		return
	}

	if r.filter.Duplicate(req.Domain) {
		return
	}

	r.queue.Append(req)
}

// OutputNames implements the FQDNManager interface.
func (r *DomainManager) OutputNames(num int) []*requests.DNSRequest {
	return []*requests.DNSRequest{}
}

// NameQueueLen implements the FQDNManager interface.
func (r *DomainManager) NameQueueLen() int {
	return 0
}

// OutputRequests implements the FQDNManager interface.
func (r *DomainManager) OutputRequests(num int) int {
	// Check that we are not releasing the domain names too quickly
	if r.enum.dnsMgr != nil && r.last.Add(5*time.Second).After(time.Now()) {
		return 0
	}

	domain, index := r.nextDomainAndSrc()
	if domain == "" {
		return 0
	}

	r.last = time.Now()
	// Release the current domain name to the next data source
	r.enum.srcs[index].DNSRequest(r.enum.ctx, &requests.DNSRequest{
		Name:   domain,
		Domain: domain,
		Tag:    requests.DNS,
		Source: "DNS",
	})

	return 1
}

func (r *DomainManager) nextDomainAndSrc() (string, int) {
	r.srcIndex--

	if r.curDomain == "" || r.srcIndex < 0 {
		element, ok := r.queue.Next()
		if !ok {
			return "", 0
		}

		r.srcIndex = len(r.enum.srcs) - 1
		r.curDomain = (element.(*requests.DNSRequest)).Domain
	}

	return r.curDomain, r.srcIndex
}

// RequestQueueLen implements the FQDNManager interface.
func (r *DomainManager) RequestQueueLen() int {
	return r.queue.Len()
}

// Stop implements the FQDNManager interface.
func (r *DomainManager) Stop() error {
	r.queue = queue.NewQueue()
	r.filter = stringfilter.NewStringFilter()
	return nil
}

// SubdomainManager handles newly discovered proper subdomain names in the enumeration.
type SubdomainManager struct {
	enum      *Enumeration
	queue     *queue.Queue
	rqueue    *queue.Queue
	subqueue  *queue.Queue
	timesChan chan *timesReq
	done      chan struct{}
}

// NewSubdomainManager returns an initialized SubdomainManager.
func NewSubdomainManager(e *Enumeration) *SubdomainManager {
	r := &SubdomainManager{
		enum:      e,
		queue:     queue.NewQueue(),
		rqueue:    queue.NewQueue(),
		subqueue:  queue.NewQueue(),
		timesChan: make(chan *timesReq, 10),
		done:      make(chan struct{}, 2),
	}

	go r.timesManager()
	return r
}

// InputName implements the FQDNManager interface.
func (r *SubdomainManager) InputName(req *requests.DNSRequest) {
	if req == nil || req.Name == "" || req.Domain == "" {
		return
	}
	// Clean up the newly discovered name and domain
	requests.SanitizeDNSRequest(req)
	// Send every resolved name and associated DNS records to the data manager
	r.enum.dataMgr.DNSRequest(r.enum.ctx, req)

	if !r.enum.Config.IsDomainInScope(req.Name) {
		return
	}

	labels := strings.Split(req.Name, ".")
	// Do not further evaluate service subdomains
	if labels[1] == "_tcp" || labels[1] == "_udp" || labels[1] == "_tls" {
		return
	}

	r.rqueue.Append(req)
	// Keep track of all domains and proper subdomains discovered
	go r.checkSubdomain(req)
}

// OutputNames implements the FQDNManager interface.
func (r *SubdomainManager) OutputNames(num int) []*requests.DNSRequest {
	var results []*requests.DNSRequest

	for i := 0; i < num; i++ {
		element, ok := r.queue.Next()
		if !ok {
			break
		}

		req := element.(*requests.DNSRequest)
		results = append(results, req)
	}

	return results
}

// NameQueueLen implements the FQDNManager interface.
func (r *SubdomainManager) NameQueueLen() int {
	return r.queue.Len()
}

type subQueueElement struct {
	Req   *requests.DNSRequest
	Times int
}

// OutputRequests implements the FQDNManager interface.
func (r *SubdomainManager) OutputRequests(num int) int {
	toBeSent := num
	if srcslen := len(r.enum.srcs); srcslen > 0 {
		toBeSent = toBeSent / srcslen

		if toBeSent <= 0 {
			return 0
		}
	}

	var rlen int
	sublen := toBeSent
	qlen := r.subqueue.Len()
	if qlen < toBeSent {
		sublen = qlen
		rlen = toBeSent - qlen
	}

	var count int
	for i := 0; i < sublen; i++ {
		element, ok := r.subqueue.Next()
		if !ok {
			break
		}

		count++
		s := element.(*subQueueElement)
		for _, src := range r.enum.srcs {
			src.SubdomainDiscovered(r.enum.ctx, s.Req, s.Times)
		}
	}

	for i := 0; i < rlen; i++ {
		element, ok := r.rqueue.Next()
		if !ok {
			break
		}

		count++
		req := element.(*requests.DNSRequest)
		for _, src := range r.enum.srcs {
			src.Resolved(r.enum.ctx, req)
		}
	}

	return count
}

// RequestQueueLen implements the FQDNManager interface.
func (r *SubdomainManager) RequestQueueLen() int {
	return r.rqueue.Len() + r.subqueue.Len()
}

// Stop implements the FQDNManager interface.
func (r *SubdomainManager) Stop() error {
	close(r.done)
	r.queue = queue.NewQueue()
	r.rqueue = queue.NewQueue()
	r.subqueue = queue.NewQueue()
	return nil
}

func (r *SubdomainManager) checkSubdomain(req *requests.DNSRequest) {
	labels := strings.Split(req.Name, ".")
	num := len(labels)
	// Is this large enough to consider further?
	if num < 2 {
		return
	}
	// It cannot have fewer labels than the root domain name
	if num-1 < len(strings.Split(req.Domain, ".")) {
		return
	}

	sub := strings.TrimSpace(strings.Join(labels[1:], "."))
	// CNAMEs are not a proper subdomain
	if r.enum.Graph.IsCNAMENode(sub) {
		return
	}

	subreq := &requests.DNSRequest{
		Name:   sub,
		Domain: req.Domain,
		Tag:    req.Tag,
		Source: req.Source,
	}
	times := r.timesForSubdomain(sub)

	if sub != req.Domain {
		r.enum.Bus.Publish(requests.SubDiscoveredTopic, eventbus.PriorityHigh, r.enum.ctx, subreq, times)
	}

	r.subqueue.Append(&subQueueElement{
		Req:   subreq,
		Times: times,
	})

	if nReq := r.enum.checkResFilter(subreq); nReq != nil {
		r.queue.Append(nReq)
	}
}

func (r *SubdomainManager) timesForSubdomain(sub string) int {
	ch := make(chan int, 2)

	r.timesChan <- &timesReq{
		Sub: sub,
		Ch:  ch,
	}

	return <-ch
}

type timesReq struct {
	Sub string
	Ch  chan int
}

func (r *SubdomainManager) timesManager() {
	subdomains := make(map[string]int)

	for {
		select {
		case <-r.done:
			return
		case req := <-r.timesChan:
			times, found := subdomains[req.Sub]
			if found {
				times++
			} else {
				times = 1
			}

			subdomains[req.Sub] = times
			req.Ch <- times
		}
	}
}

// NameManager handles the filtering and release of newly discovered FQDNs in the enumeration.
type NameManager struct {
	enum  *Enumeration
	queue *queue.Queue
}

// NewNameManager returns an initialized NameManager.
func NewNameManager(e *Enumeration) *NameManager {
	return &NameManager{
		enum:  e,
		queue: queue.NewQueue(),
	}
}

// InputName implements the FQDNManager interface.
func (r *NameManager) InputName(req *requests.DNSRequest) {
	if req == nil || req.Name == "" || req.Domain == "" {
		return
	}

	// Clean up the newly discovered name and domain
	requests.SanitizeDNSRequest(req)
	// Check that this name has not already been processed
	if r.enum.checkResFilter(req) == nil {
		return
	}
	r.queue.Append(req)
}

// OutputNames implements the FQDNManager interface.
func (r *NameManager) OutputNames(num int) []*requests.DNSRequest {
	var results []*requests.DNSRequest

	for i := 0; i < num; i++ {
		element, ok := r.queue.Next()
		if !ok {
			break
		}

		results = append(results, element.(*requests.DNSRequest))
	}

	return results
}

// NameQueueLen implements the FQDNManager interface.
func (r *NameManager) NameQueueLen() int {
	return r.queue.Len()
}

// OutputRequests implements the FQDNManager interface.
func (r *NameManager) OutputRequests(num int) int {
	return 0
}

// RequestQueueLen implements the FQDNManager interface.
func (r *NameManager) RequestQueueLen() int {
	return 0
}

// Stop implements the FQDNManager interface.
func (r *NameManager) Stop() error {
	r.queue = queue.NewQueue()
	return nil
}
