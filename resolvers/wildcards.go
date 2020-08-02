// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package resolvers

import (
	"context"
	"math/rand"
	"strings"
	"time"

	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/stringset"
)

// Constants related to DNS labels.
const (
	MaxDNSNameLen  = 253
	MaxDNSLabelLen = 63
	MinLabelLen    = 6
	MaxLabelLen    = 24
	LDHChars       = "abcdefghijklmnopqrstuvwxyz0123456789-"
)

const numOfWildcardTests = 5

// Names for the different types of wildcards that can be detected.
const (
	WildcardTypeNone = iota
	WildcardTypeStatic
	WildcardTypeDynamic
)

var wildcardQueryTypes = []string{
	"CNAME",
	"A",
	"AAAA",
}

type wildcard struct {
	WildcardType int
	Answers      []requests.DNSAnswer
	beingTested  bool
}

type wildcardChans struct {
	WildcardReq     chan *wildcardReq
	IPsAcrossLevels chan *ipsAcrossLevels
	TestResult      chan *testResult
}

type wildcardReq struct {
	Ctx context.Context
	Sub string
	Ch  chan *wildcard
}

type ipsAcrossLevels struct {
	Req *requests.DNSRequest
	Ch  chan int
}

type testResult struct {
	Sub    string
	Result *wildcard
}

// MatchesWildcard returns true if the request provided resolved to a DNS wildcard.
func (rp *ResolverPool) MatchesWildcard(ctx context.Context, req *requests.DNSRequest) bool {
	if rp.hasWildcard(ctx, req) == WildcardTypeNone {
		return false
	}
	return true
}

// GetWildcardType returns the DNS wildcard type for the provided subdomain name.
func (rp *ResolverPool) GetWildcardType(ctx context.Context, req *requests.DNSRequest) int {
	return rp.hasWildcard(ctx, req)
}

func (rp *ResolverPool) hasWildcard(ctx context.Context, req *requests.DNSRequest) int {
	req.Name = strings.ToLower(strings.Trim(req.Name, "."))
	req.Domain = strings.ToLower(strings.Trim(req.Domain, "."))

	base := len(strings.Split(req.Domain, "."))
	labels := strings.Split(req.Name, ".")
	if len(labels) > base {
		labels = labels[1:]
	}

	// Check for a DNS wildcard at each label starting with the root domain
	for i := len(labels) - base; i >= 0; i-- {
		w := rp.fetchWildcardType(ctx, strings.Join(labels[i:], "."))

		if w.WildcardType == WildcardTypeDynamic {
			return WildcardTypeDynamic
		} else if w.WildcardType == WildcardTypeStatic {
			if len(req.Records) == 0 {
				return w.WildcardType
			}

			set := stringset.New()
			insertRecordData(set, req.Records)
			intersectRecordData(set, w.Answers)
			if set.Len() > 0 {
				return w.WildcardType
			}
		}
	}

	return rp.checkIPsAcrossLevels(req)
}

func (rp *ResolverPool) fetchWildcardType(ctx context.Context, sub string) *wildcard {
	ch := make(chan *wildcard, 2)

	rp.wildcardChannels.WildcardReq <- &wildcardReq{
		Ctx: ctx,
		Sub: sub,
		Ch:  ch,
	}

	return <-ch
}

func (rp *ResolverPool) checkIPsAcrossLevels(req *requests.DNSRequest) int {
	ch := make(chan int, 2)

	rp.wildcardChannels.IPsAcrossLevels <- &ipsAcrossLevels{
		Req: req,
		Ch:  ch,
	}

	return <-ch
}

func (rp *ResolverPool) manageWildcards(chs *wildcardChans) {
	wildcards := make(map[string]*wildcard)
loop:
	for {
		select {
		case <-rp.Done:
			return
		case req := <-chs.WildcardReq:
			// Check if the wildcard information has been cached
			if w, found := wildcards[req.Sub]; found && !w.beingTested {
				req.Ch <- w
			} else if found && w.beingTested {
				go rp.resendWildcardReq(req)
			} else {
				wildcards[req.Sub] = &wildcard{
					WildcardType: WildcardTypeNone,
					Answers:      []requests.DNSAnswer{},
					beingTested:  true,
				}
				go rp.wildcardTest(req.Ctx, req.Sub)
				go rp.resendWildcardReq(req)
			}
		case test := <-chs.TestResult:
			wildcards[test.Sub] = test.Result
		case ips := <-chs.IPsAcrossLevels:
			if len(ips.Req.Records) == 0 {
				ips.Ch <- WildcardTypeNone
				continue loop
			}

			base := len(strings.Split(ips.Req.Domain, "."))
			labels := strings.Split(strings.ToLower(ips.Req.Name), ".")
			if len(labels) <= base || (len(labels)-base) < 3 {
				ips.Ch <- WildcardTypeNone
				continue loop
			}

			l := len(labels) - base
			records := stringset.New()
			for i := 1; i <= l; i++ {
				w, found := wildcards[strings.Join(labels[i:], ".")]
				if !found || w.Answers == nil || len(w.Answers) == 0 {
					break
				}

				intersectRecordData(records, w.Answers)
			}

			if records.Len() > 0 {
				ips.Ch <- WildcardTypeStatic
				continue loop
			}

			ips.Ch <- WildcardTypeNone
		}
	}
}

func (rp *ResolverPool) resendWildcardReq(req *wildcardReq) {
	n := numOfWildcardTests / 2

	time.Sleep(time.Duration(n) * time.Second)
	rp.wildcardChannels.WildcardReq <- req
}

func (rp *ResolverPool) wildcardTest(ctx context.Context, sub string) {
	var retRecords bool
	set := stringset.New()
	var answers []requests.DNSAnswer

	// Query multiple times with unlikely names against this subdomain
	for i := 0; i < numOfWildcardTests; i++ {
		// Generate the unlikely label / name
		name := UnlikelyName(sub)
		for name == "" {
			name = UnlikelyName(sub)
		}

		var ans []requests.DNSAnswer
		for _, t := range wildcardQueryTypes {
			if a, _, err := rp.Resolve(ctx, name, t, PriorityCritical); err == nil {
				if a != nil && len(a) > 0 {
					retRecords = true
					ans = append(ans, a...)
				}
			}
		}

		if i == 0 {
			insertRecordData(set, ans)
		} else {
			intersectRecordData(set, ans)
		}
		answers = append(answers, ans...)
		time.Sleep(time.Second)
	}

	already := stringset.New()
	var final []requests.DNSAnswer
	// Create the slice of answers common across all the unlikely name queries
	for _, a := range answers {
		a.Data = strings.Trim(a.Data, ".")

		if set.Has(a.Data) && !already.Has(a.Data) {
			final = append(final, a)
			already.Insert(a.Data)
		}
	}

	// Determine whether the subdomain has a DNS wildcard, and if so, which type is it?
	wildcardType := WildcardTypeNone
	if retRecords {
		wildcardType = WildcardTypeStatic

		if len(final) == 0 {
			wildcardType = WildcardTypeDynamic
		}
		rp.Log.Printf("DNS wildcard detected: %s: type: %d", "*."+sub, wildcardType)
	}

	rp.wildcardChannels.TestResult <- &testResult{
		Sub: sub,
		Result: &wildcard{
			WildcardType: wildcardType,
			Answers:      final,
			beingTested:  false,
		},
	}
}

// UnlikelyName takes a subdomain name and returns an unlikely DNS name within that subdomain.
func UnlikelyName(sub string) string {
	var newlabel string
	ldh := []rune(LDHChars)
	ldhLen := len(ldh)

	// Determine the max label length
	l := MaxDNSNameLen - (len(sub) + 1)
	if l > MaxLabelLen {
		l = MaxLabelLen
	} else if l < MinLabelLen {
		return ""
	}
	// Shuffle our LDH characters
	rand.Shuffle(ldhLen, func(i, j int) {
		ldh[i], ldh[j] = ldh[j], ldh[i]
	})

	l = MinLabelLen + rand.Intn((l-MinLabelLen)+1)
	for i := 0; i < l; i++ {
		sel := rand.Int() % ldhLen

		newlabel = newlabel + string(ldh[sel])
	}

	if newlabel == "" {
		return newlabel
	}
	return strings.Trim(newlabel, "-") + "." + sub
}

func intersectRecordData(set stringset.Set, ans []requests.DNSAnswer) {
	records := stringset.New()

	for _, a := range ans {
		records.Insert(strings.Trim(a.Data, "."))
	}

	set.Intersect(records)
}

func insertRecordData(set stringset.Set, ans []requests.DNSAnswer) {
	records := stringset.New()

	for _, a := range ans {
		records.Insert(strings.Trim(a.Data, "."))
	}

	set.Union(records)
}
