// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package datasrcs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/OWASP/Amass/v3/eventbus"
	"github.com/OWASP/Amass/v3/net/http"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/stringfilter"
	"github.com/OWASP/Amass/v3/systems"
)

const commonCrawlIndexListURL = "https://index.commoncrawl.org/collinfo.json"

// CommonCrawl is the Service that handles access to the CommonCrawl data source.
type CommonCrawl struct {
	requests.BaseService

	SourceType string
	sys        systems.System
	indexURLs  []string
}

// NewCommonCrawl returns he object initialized, but not yet started.
func NewCommonCrawl(sys systems.System) *CommonCrawl {
	c := &CommonCrawl{
		SourceType: requests.API,
		sys:        sys,
	}

	c.BaseService = *requests.NewBaseService(c, "CommonCrawl")
	return c
}

// Type implements the Service interface.
func (c *CommonCrawl) Type() string {
	return c.SourceType
}

// OnStart implements the Service interface.
func (c *CommonCrawl) OnStart() error {
	c.BaseService.OnStart()

	// Get all of the index API URLs
	page, err := http.RequestWebPage(commonCrawlIndexListURL, nil, nil, "", "")
	if err != nil {
		c.sys.Config().Log.Printf("%s: Failed to obtain the index list: %v", c.String(), err)
		return fmt.Errorf("%s: Failed to obtain the index list: %v", c.String(), err)
	}

	type index struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"cdx-api"`
	}

	var indexList []index
	if err := json.Unmarshal([]byte(page), &indexList); err != nil {
		c.sys.Config().Log.Printf("%s: Failed to unmarshal the index list: %v", c.String(), err)
		return fmt.Errorf("%s: Failed to unmarshal the index list: %v", c.String(), err)
	}

	for i, u := range indexList {
		if i >= 5 {
			break
		}
		c.indexURLs = append(c.indexURLs, u.URL)
	}

	c.SetRateLimit(500 * time.Millisecond)
	return nil
}

// OnDNSRequest implements the Service interface.
func (c *CommonCrawl) OnDNSRequest(ctx context.Context, req *requests.DNSRequest) {
	cfg, bus, err := ContextConfigBus(ctx)
	if err != nil {
		return
	}

	re := cfg.DomainRegex(req.Domain)
	if re == nil {
		return
	}
	bus.Publish(requests.LogTopic, eventbus.PriorityHigh,
		fmt.Sprintf("Querying %s for %s subdomains", c.String(), req.Domain))

	filter := stringfilter.NewStringFilter()
	for _, index := range c.indexURLs {
		select {
		case <-c.Quit():
			return
		default:
			c.CheckRateLimit()
			bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, c.String())

			u := c.getURL(req.Domain, index)
			page, err := http.RequestWebPage(u, nil, nil, "", "")
			if err != nil {
				bus.Publish(requests.LogTopic, eventbus.PriorityHigh, fmt.Sprintf("%s: %s: %v", c.String(), u, err))
				continue
			}

			for _, url := range c.parseJSON(page) {
				if name := re.FindString(url); name != "" && !filter.Duplicate(name) {
					genNewNameEvent(ctx, c.sys, c, name)
				}
			}
		}
	}
}

func (c *CommonCrawl) parseJSON(page string) []string {
	var urls []string
	filter := stringfilter.NewStringFilter()

	scanner := bufio.NewScanner(strings.NewReader(page))
	for scanner.Scan() {
		// Get the next line of JSON
		line := scanner.Text()
		if line == "" {
			continue
		}

		var m struct {
			URL string `json:"url"`
		}
		err := json.Unmarshal([]byte(line), &m)
		if err != nil {
			continue
		}

		if !filter.Duplicate(m.URL) {
			urls = append(urls, m.URL)
		}
	}
	return urls
}

func (c *CommonCrawl) getURL(domain, index string) string {
	u, _ := url.Parse(index)

	u.RawQuery = url.Values{
		"url":      {"*." + domain},
		"output":   {"json"},
		"filter":   {"status:200"},
		"fl":       {"url,status"},
		"pageSize": {"2000"},
	}.Encode()
	return u.String()
}
