// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package datasrcs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/OWASP/Amass/v3/eventbus"
	"github.com/OWASP/Amass/v3/net/dns"
	"github.com/OWASP/Amass/v3/net/http"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/stringset"
	"github.com/OWASP/Amass/v3/systems"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq" // Need the postgres driver
)

// Crtsh is the Service that handles access to the Crtsh data source.
type Crtsh struct {
	requests.BaseService

	SourceType     string
	sys            systems.System
	db             *sqlx.DB
	haveConnection bool
}

// NewCrtsh returns he object initialized, but not yet started.
func NewCrtsh(sys systems.System) *Crtsh {
	c := &Crtsh{
		SourceType:     requests.CERT,
		sys:            sys,
		haveConnection: true,
	}

	c.BaseService = *requests.NewBaseService(c, "Crtsh")
	return c
}

// Type implements the Service interface.
func (c *Crtsh) Type() string {
	return c.SourceType
}

// OnStart implements the Service interface.
func (c *Crtsh) OnStart() error {
	c.BaseService.OnStart()

	var err error
	c.db, err = sqlx.Connect("postgres", "host=crt.sh user=guest dbname=certwatch sslmode=disable")
	if err != nil {
		c.sys.Config().Log.Printf("%s: Failed to connect to the database server: %v", c.String(), err)
		c.haveConnection = false
	}

	c.SetRateLimit(time.Second)
	return nil
}

// OnDNSRequest implements the Service interface.
func (c *Crtsh) OnDNSRequest(ctx context.Context, req *requests.DNSRequest) {
	_, bus, err := ContextConfigBus(ctx)
	if err != nil {
		return
	}

	c.CheckRateLimit()
	bus.Publish(requests.LogTopic, eventbus.PriorityHigh,
		fmt.Sprintf("Querying %s for %s subdomains", c.String(), req.Domain))

	// Fall back to scraping the web page if the database connection failed
	if !c.haveConnection {
		c.scrape(ctx, req.Domain)
		return
	}

	c.executeQuery(ctx, req.Domain)
}

func (c *Crtsh) executeQuery(ctx context.Context, domain string) {
	_, bus, err := ContextConfigBus(ctx)
	if err != nil {
		return
	}

	var results []struct {
		Domain string `db:"domain"`
	}

	pattern := "%." + domain
	err = c.db.Select(&results,
		`SELECT DISTINCT ci.NAME_VALUE as domain
		FROM certificate_identity ci
		WHERE reverse(lower(ci.NAME_VALUE)) LIKE reverse(lower($1))
		ORDER BY ci.NAME_VALUE`, pattern)
	if err != nil {
		bus.Publish(requests.LogTopic, eventbus.PriorityHigh,
			fmt.Sprintf("%s: Query pattern %s: %v", c.String(), pattern, err))
		return
	}

	bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, c.String())

	// Extract the subdomain names from the results
	names := stringset.New()
	for _, result := range results {
		names.Insert(dns.RemoveAsteriskLabel(result.Domain))
	}

	for name := range names {
		genNewNameEvent(ctx, c.sys, c, name)
	}
}

func (c *Crtsh) scrape(ctx context.Context, domain string) {
	_, bus, err := ContextConfigBus(ctx)
	if err != nil {
		return
	}

	url := c.getURL(domain)
	page, err := http.RequestWebPage(url, nil, nil, "", "")
	if err != nil {
		bus.Publish(requests.LogTopic, eventbus.PriorityHigh, fmt.Sprintf("%s: %s: %v", c.String(), url, err))
		return
	}

	bus.Publish(requests.SetActiveTopic, eventbus.PriorityCritical, c.String())

	// Extract the subdomain names from the results
	var results []struct {
		Name string `json:"name_value"`
	}
	if err := json.Unmarshal([]byte(page), &results); err != nil {
		return
	}
	for _, line := range results {
		genNewNameEvent(ctx, c.sys, c, line.Name)
	}
}

func (c *Crtsh) getURL(domain string) string {
	return "https://crt.sh/?q=%25." + domain + "&output=json"
}
