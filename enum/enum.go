// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package enum

import (
	"context"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/OWASP/Amass/v3/config"
	"github.com/OWASP/Amass/v3/eventbus"
	"github.com/OWASP/Amass/v3/graph"
	"github.com/OWASP/Amass/v3/queue"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/stringfilter"
	"github.com/OWASP/Amass/v3/stringset"
	"github.com/OWASP/Amass/v3/systems"
)

var filterMaxSize int64 = 1 << 23

// Enumeration is the object type used to execute a DNS enumeration with Amass.
type Enumeration struct {
	// Information sent in the context
	Config     *config.Config
	Bus        *eventbus.EventBus
	Sys        systems.System
	Graph      *graph.Graph
	closedOnce sync.Once

	ctx     context.Context
	dnsMgr  requests.Service
	dataMgr requests.Service
	asMgr   *ASService
	srcs    []requests.Service

	// The filter for new outgoing DNS queries
	resFilter      stringfilter.Filter
	resFilterCount int64
	resFilterLock  sync.Mutex

	// Queue for the log messages
	logQueue *queue.Queue

	// Broadcast channel that indicates no further writes to the output channel
	done     chan struct{}
	doneOnce sync.Once

	managers       []FQDNManager
	resolvedMgrs   []FQDNManager
	resolvedFilter stringfilter.Filter
	addrMgr        *AddressManager
	nameMgr        *NameManager
	subMgr         *SubdomainManager
	domainMgr      *DomainManager

	enumStateChannels *enumStateChans
	memUsage          uint64
	altSourcesQueue   *queue.Queue
}

// NewEnumeration returns an initialized Enumeration that has not been started yet.
func NewEnumeration(cfg *config.Config, sys systems.System) *Enumeration {
	e := &Enumeration{
		Config:         cfg,
		Sys:            sys,
		Bus:            eventbus.NewEventBus(),
		Graph:          graph.NewGraph(graph.NewCayleyGraphMemory()),
		srcs:           selectedDataSources(cfg, sys),
		resFilter:      stringfilter.NewBloomFilter(filterMaxSize),
		logQueue:       queue.NewQueue(),
		done:           make(chan struct{}),
		resolvedFilter: stringfilter.NewBloomFilter(filterMaxSize),
		enumStateChannels: &enumStateChans{
			GetLastActive: make(chan chan time.Time, 10),
			UpdateLast:    queue.NewQueue(),
			GetPerSec:     make(chan chan *getPerSec, 10),
			IncPerSec:     queue.NewQueue(),
			ClearPerSec:   make(chan struct{}, 10),
		},
		altSourcesQueue: queue.NewQueue(),
	}
	go e.manageEnumState(e.enumStateChannels)
	go e.processDupNames()

	if cfg.Passive {
		return e
	}

	e.dataMgr = NewDataManagerService(sys, e.Graph)
	if err := e.dataMgr.Start(); err != nil {
		return nil
	}

	e.dnsMgr = NewDNSService(sys)
	if err := e.dnsMgr.Start(); err != nil {
		return nil
	}

	e.asMgr = NewASService(sys, e.Graph, e.Config.UUID.String())
	if err := e.asMgr.Start(); err != nil {
		return nil
	}

	return e
}

// Close cleans up resources instantiated by the Enumeration.
func (e *Enumeration) Close() {
	e.closedOnce.Do(func() {
		e.Graph.Close()
	})
}

// Done safely closes the done broadcast channel.
func (e *Enumeration) Done() {
	e.doneOnce.Do(func() {
		close(e.done)
	})
}

// Using the config and system provided, this function returns the data sources used in the enumeration
func selectedDataSources(cfg *config.Config, sys systems.System) []requests.Service {
	specified := stringset.New()
	specified.InsertMany(cfg.SourceFilter.Sources...)

	available := stringset.New()
	for _, src := range sys.DataSources() {
		available.Insert(src.String())
	}

	if specified.Len() > 0 && cfg.SourceFilter.Include {
		available.Intersect(specified)
	} else {
		available.Subtract(specified)
	}

	var results []requests.Service
	for _, src := range sys.DataSources() {
		if available.Has(src.String()) {
			results = append(results, src)
		}
	}

	rand.Shuffle(len(results), func(i, j int) {
		results[i], results[j] = results[j], results[i]
	})
	return results
}

// Start begins the vertical domain correlation process for the Enumeration object.
func (e *Enumeration) Start() error {
	if err := e.Config.CheckSettings(); err != nil {
		return err
	}
	/*
	 * This context, used throughout the enumeration, will provide the
	 * ability to cancel operations and to pass the configuration and
	 * event bus to all the components
	 */
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, requests.ContextConfig, e.Config)
	e.ctx = context.WithValue(ctx, requests.ContextEventBus, e.Bus)
	// Start the logging at this point, since data sources are used shortly
	go e.periodicLogging()
	// If requests were made for specific ASNs, then those requests are
	// send to included data sources at this point
	for _, src := range e.srcs {
		for _, asn := range e.Config.ASNs {
			src.ASNRequest(e.ctx, &requests.ASNRequest{ASN: asn})
		}
	}
	/*
	 * A sequence of enum managers is setup starting here. These managers
	 * will be used throughout the enumeration to control the release of
	 * information and handle the discovery of new names. The order that the
	 * managers have been entered into the sequence is important to how the
	 * engine selects what information to process next during the enumeration
	 *
	 * When not running in passive mode, the enumeration will require an
	 * AddressManager to receive successfully resolved FQDNs and process
	 * the IP addresses for caching of infrastructure data, setting up
	 * reverse DNS queries, and other engagements when in active mode
	 */
	if !e.Config.Passive {
		e.addrMgr = NewAddressManager(e)
		defer e.addrMgr.Stop()
		e.managers = append(e.managers, e.addrMgr)
		e.resolvedMgrs = append(e.resolvedMgrs, e.addrMgr)
		e.Bus.Subscribe(requests.NewAddrTopic, e.addrMgr.InputAddress)
		defer e.Bus.Unsubscribe(requests.NewAddrTopic, e.addrMgr.InputAddress)
		e.Bus.Subscribe(requests.NewAddrTopic, e.asMgr.InputAddress)
		defer e.Bus.Unsubscribe(requests.NewAddrTopic, e.asMgr.InputAddress)
		e.Bus.Subscribe(requests.NewASNTopic, e.Sys.Cache().Update)
		defer e.Bus.Unsubscribe(requests.NewASNTopic, e.Sys.Cache().Update)
	}
	/*
	 * Setup the NameManager for receiving newly discovered names from the
	 * event bus NewNameTopic. This manager is essential, even for passive
	 * mode
	 */
	e.nameMgr = NewNameManager(e)
	defer e.nameMgr.Stop()
	e.managers = append(e.managers, e.nameMgr)
	e.Bus.Subscribe(requests.NewNameTopic, e.nameMgr.InputName)
	defer e.Bus.Unsubscribe(requests.NewNameTopic, e.nameMgr.InputName)
	/*
	 * When not running in passive mode, the enumeration will need to keep
	 * track of all proper subdomain names found and how many unique labals
	 * each subdomain has. This information is important for other operations
	 * such as brute forcing and attempts to guess new FQDNs
	 */
	if !e.Config.Passive {
		e.subMgr = NewSubdomainManager(e)
		defer e.subMgr.Stop()
		e.managers = append(e.managers, e.subMgr)
		e.resolvedMgrs = append(e.resolvedMgrs, e.subMgr)
	}
	/*
	 * Now that the name managers has been setup, names provided by the user
	 * and names acquired from the graph database can be brought into the
	 * enumeration
	 */
	go e.submitKnownNames()
	go e.submitProvidedNames()
	/*
	 * Setup the DomainManager for releasing root domain names that are in
	 * scope and identified by the user. This manager is essential, even for
	 * passive mode
	 */
	e.domainMgr = NewDomainManager(e)
	defer e.domainMgr.Stop()
	for _, domain := range e.Config.Domains() {
		e.domainMgr.InputName(&requests.DNSRequest{
			Name:   domain,
			Domain: domain,
			Tag:    requests.DNS,
			Source: "DNS",
		})
	}
	e.managers = append(e.managers, e.domainMgr)
	// Setup the event handler for newly resolved DNS names
	e.Bus.Subscribe(requests.NameResolvedTopic, e.resolvedDispatcher)
	defer e.Bus.Unsubscribe(requests.NameResolvedTopic, e.resolvedDispatcher)
	/*
	 * These events are important to the engine in order to receive output, logs,
	 * notices about service activity, and notices about DNS query completion
	 */
	e.Bus.Subscribe(requests.LogTopic, e.queueLog)
	defer e.Bus.Unsubscribe(requests.LogTopic, e.queueLog)
	e.Bus.Subscribe(requests.SetActiveTopic, e.updateLastActive)
	defer e.Bus.Unsubscribe(requests.SetActiveTopic, e.updateLastActive)
	e.Bus.Subscribe(requests.ResolveCompleted, e.incQueriesPerSec)
	defer e.Bus.Unsubscribe(requests.ResolveCompleted, e.incQueriesPerSec)
	// Setup the DNS Service to receive the appropriate events
	if !e.Config.Passive {
		e.Bus.Subscribe(requests.ResolveNameTopic, e.dnsMgr.DNSRequest)
		defer e.Bus.Unsubscribe(requests.ResolveNameTopic, e.dnsMgr.DNSRequest)
		e.Bus.Subscribe(requests.SubDiscoveredTopic, e.dnsMgr.SubdomainDiscovered)
		defer e.Bus.Unsubscribe(requests.SubDiscoveredTopic, e.dnsMgr.SubdomainDiscovered)
	}
	// If a timeout was provided in the configuration, it will go off that
	// many minutes from this point in the enumeration process
	var timeout *time.Timer
	if e.Config.Timeout > 0 {
		timeout = time.AfterFunc(time.Duration(e.Config.Timeout)*time.Minute, func() {
			e.Config.Log.Printf("Enumeration exceeded provided timeout")
			e.Done()
		})
		defer timeout.Stop()
	}
	// Get the ball rolling before the timer fires
	completed := e.useManagers()
	more := time.NewTimer(5 * time.Second)
	t := time.NewTimer(20 * time.Second)
	perMin := time.NewTicker(time.Minute)
	defer perMin.Stop()
loop:
	for {
		select {
		case <-e.done:
			break loop
		case <-more.C:
			completed += e.useManagers()
			more.Reset(500 * time.Millisecond)
		case <-t.C:
			var inactive bool
			// Has the enumeration been inactive long enough to stop the task?
			if e.isDataManagerQueueEmpty() {
				inactive = time.Since(e.lastActive()) > 10*time.Second
			}

			if completed == 0 && inactive {
				// End the enumeration!
				e.Done()
				break loop
			}

			completed = 0
			//e.memUsage = e.Sys.GetMemoryUsage()
			t.Reset(5 * time.Second)
		case <-perMin.C:
			if !e.Config.Passive {
				var pct float64
				sec, retries := e.dnsQueriesPerSec()
				if sec > 0 {
					pct = (float64(retries) / float64(sec)) * 100
					if e.isDataManagerQueueEmpty() && (sec < 10 || pct > 90.0) {
						e.Done()
						break loop
					}
				}

				e.Config.Log.Printf("Average DNS queries performed: %d/sec, Average retries required: %.2f%%", sec, pct)
				e.clearPerSec()
			}
		}
	}

	cancel()
	if !e.Config.Passive {
		e.dnsMgr.Stop()
		e.dataMgr.Stop()
	}
	e.writeLogs(true)
	if !e.Config.Passive {
		// Attempt to fix IP address nodes without edges to netblocks
		e.Graph.HealAddressNodes(e.Sys.Cache(), e.Config.UUID.String())
	}
	return nil
}

func (e *Enumeration) isDataManagerQueueEmpty() bool {
	var l int

	if e.dataMgr != nil {
		l = e.dataMgr.RequestLen()
	}

	return l == 0
}

func (e *Enumeration) resolvedDispatcher(req *requests.DNSRequest) {
	if e.resolvedFilter.Duplicate(req.Name) {
		return
	}

	for _, mgr := range e.resolvedMgrs {
		mgr.InputName(req)
	}
}

func (e *Enumeration) requiredNumberOfNames() int {
	if e.Config.Passive {
		return 100000
	}

	max := e.Config.MaxDNSQueries
	required := max - e.dnsNamesRemaining()
	// Ensure a minimum value of one
	if required < 0 {
		required = 0
	}

	return required
}

func (e *Enumeration) dnsNamesRemaining() int {
	var l int

	if e.dnsMgr != nil {
		l = e.dnsMgr.RequestLen()
	}

	return l
}

func (e *Enumeration) useManagers() int {
	required := e.requiredNumberOfNames()
	if required == 0 {
		return 1
	}

	var count, pending int
	// Attempt to handle address requests first
	if e.addrMgr != nil {
		count = e.addrMgr.OutputRequests(required / 2)
		pending = e.addrMgr.RequestQueueLen()
	}
	// Loop through the managers until we acquire the necessary number of names for processing
	for _, mgr := range e.managers {
		remaining := required - count
		if remaining <= 0 {
			break
		}
		// Send the FQDNs acquired from the manager
		for _, req := range mgr.OutputNames(remaining) {
			count++

			if e.Config.Passive {
				e.updateLastActive("enum")
				if e.Config.IsDomainInScope(req.Name) {
					e.Graph.InsertFQDN(req.Name, req.Source, req.Tag, e.Config.UUID.String())
				}
				continue
			}

			e.Bus.Publish(requests.ResolveNameTopic, eventbus.PriorityLow, e.ctx, req)
		}
	}
	// Make sure we have the complete number of pending queries
	for _, mgr := range e.managers {
		pending += mgr.NameQueueLen()
	}
	// Check if new requests need to be sent to data sources
	if pending < required && e.memUsage < 1500000000 {
		var sent int
		needed := required - pending

		for _, mgr := range e.managers {
			sent += mgr.OutputRequests(needed - sent)
			if sent >= needed {
				break
			}
		}

		count += sent
	}

	return count
}

func (e *Enumeration) checkResFilter(req *requests.DNSRequest) *requests.DNSRequest {
	e.resFilterLock.Lock()
	defer e.resFilterLock.Unlock()

	// Check if it's time to reset our bloom filter due to number of elements seen
	if e.resFilterCount >= filterMaxSize {
		e.resFilterCount = 0
		e.resFilter = stringfilter.NewBloomFilter(filterMaxSize)
	}

	trusted := requests.TrustedTag(req.Tag)
	// Do not submit names from untrusted sources, after already receiving the name
	// from a trusted source
	if !trusted && e.resFilter.Has(req.Name+strconv.FormatBool(true)) {
		e.altSourcesQueue.Append(req)
		return nil
	}
	// At most, a FQDN will be accepted from an untrusted source first, and then
	// reconsidered from a trusted data source
	if e.resFilter.Duplicate(req.Name + strconv.FormatBool(trusted)) {
		e.altSourcesQueue.Append(req)
		return nil
	}

	e.resFilterCount++
	return req
}

// This goroutine ensures that duplicate names from other sources are shown in the Graph.
func (e *Enumeration) processDupNames() {
	type altsource struct {
		Name      string
		Source    string
		Tag       string
		Timestamp time.Time
	}
	var pending []*altsource
	each := func(element interface{}) {
		req := element.(*requests.DNSRequest)

		pending = append(pending, &altsource{
			Name:      req.Name,
			Source:    req.Source,
			Tag:       req.Tag,
			Timestamp: time.Now(),
		})
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
loop:
	for {
		select {
		case <-e.done:
			break loop
		case <-e.altSourcesQueue.Signal:
			e.altSourcesQueue.Process(each)
		case <-t.C:
			var count int
			n := time.Now()
			for _, a := range pending {
				if a.Timestamp.Add(time.Minute).After(n) {
					break
				}
				if _, err := e.Graph.ReadNode(a.Name, "fqdn"); err == nil {
					e.Graph.InsertFQDN(a.Name, a.Source, a.Tag, e.Config.UUID.String())
				}
				count++
			}
			pending = pending[count:]
		}
	}
	e.altSourcesQueue.Process(each)
}
