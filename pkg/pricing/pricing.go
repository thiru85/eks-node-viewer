/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/pricing"
	"github.com/aws/aws-sdk-go/service/pricing/pricingiface"
	"go.uber.org/multierr"
)

// Provider provides actual pricing data to the AWS cloud provider to allow it to make more informed decisions
// regarding which instances to launch.  This is initialized at startup with a periodically updated static price list to
// support running in locations where pricing data is unavailable.  In those cases the static pricing data provides a
// relative ordering that is still more accurate than our previous pricing model.  In the event that a pricing update
// fails, the previous pricing information is retained and used which may be the static initial pricing data if pricing
// updates never succeed.
type Provider struct {
	ec2     ec2iface.EC2API
	pricing pricingiface.PricingAPI
	region  string

	mu                      sync.RWMutex
	onDemandUpdateTime      time.Time
	onDemandPrices          map[string]float64
	spotUpdateTime          time.Time
	spotPrices              map[string]zonalPricing
	fargateVCPUPricePerHour float64
	fargateGBPricePerHour   float64
	notify                  func()
}

// zonalPricing is used to capture the per-zone price
// for spot data as well as the default price
// based on on-demand price when the controller first
// comes up
type zonalPricing struct {
	defaultPrice float64 // Used until we get the spot pricing data
	prices       map[string]float64
}

func newZonalPricing(defaultPrice float64) zonalPricing {
	z := zonalPricing{
		prices: map[string]float64{},
	}
	z.defaultPrice = defaultPrice
	return z
}

// pricingUpdatePeriod is how often we try to update our pricing information after the initial update on startup
const pricingUpdatePeriod = 12 * time.Hour

// NewPricingAPI returns a pricing API configured based on a particular region
func NewPricingAPI(sess *session.Session, region string) pricingiface.PricingAPI {
	if sess == nil {
		return nil
	}
	// pricing API doesn't have an endpoint in all regions
	pricingAPIRegion := "us-east-1"
	if strings.HasPrefix(region, "ap-") {
		pricingAPIRegion = "ap-south-1"
	}
	return pricing.New(sess, &aws.Config{Region: aws.String(pricingAPIRegion)})
}

func NewStaticProvider() *Provider {
	return &Provider{
		onDemandUpdateTime: initialPriceUpdate,
		onDemandPrices:     initialOnDemandPrices,
		spotPrices:         map[string]zonalPricing{},
		spotUpdateTime:     initialPriceUpdate,
	}
}
func NewProvider(ctx context.Context, sess *session.Session, notify func()) *Provider {
	region := "us-west-2"
	if aws.StringValue(sess.Config.Region) != "" {
		region = aws.StringValue(sess.Config.Region)
	}
	p := &Provider{
		region:             region,
		onDemandUpdateTime: initialPriceUpdate,
		onDemandPrices:     initialOnDemandPrices,
		spotPrices:         map[string]zonalPricing{},
		spotUpdateTime:     initialPriceUpdate,
		ec2:                ec2.New(sess),
		pricing:            NewPricingAPI(sess, region),
		notify:             notify,
	}

	go func() {
		// perform an initial price update at startup
		p.updatePricing(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pricingUpdatePeriod):
				p.updatePricing(ctx)
			}
		}
	}()
	return p
}

// OnDemandLastUpdated returns the time that the on-demand pricing was last updated
func (p *Provider) OnDemandLastUpdated() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.onDemandUpdateTime
}

// SpotLastUpdated returns the time that the spot pricing was last updated
func (p *Provider) SpotLastUpdated() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.spotUpdateTime
}

// OnDemandPrice returns the last known on-demand price for a given instance type, returning an error if there is no
// known on-demand pricing for the instance type.
func (p *Provider) OnDemandPrice(instanceType string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	price, ok := p.onDemandPrices[instanceType]
	if !ok {
		return 0.0, false
	}
	return price, true
}

func (p *Provider) FargatePrice(cpu, memory float64) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.fargateGBPricePerHour == 0 || p.fargateVCPUPricePerHour == 0 {
		return 0, false
	}
	return cpu*p.fargateVCPUPricePerHour + memory*p.fargateGBPricePerHour, true
}

// SpotPrice returns the last known spot price for a given instance type and zone, returning an error
// if there is no known spot pricing for that instance type or zone
func (p *Provider) SpotPrice(instanceType string, zone string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if val, ok := p.spotPrices[instanceType]; ok {
		if p.spotUpdateTime.Equal(initialPriceUpdate) {
			return val.defaultPrice, true
		}
		if price, ok := p.spotPrices[instanceType].prices[zone]; ok {
			return price, true
		}
		return 0.0, false
	}
	return 0.0, false
}

func (p *Provider) updatePricing(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := p.updateOnDemandPricing(ctx); err != nil {
			log.Printf("updating on-demand pricing, %s, using existing pricing data from %s", err, p.onDemandUpdateTime.Format(time.RFC3339))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := p.updateSpotPricing(ctx); err != nil {
			log.Printf("updating spot pricing, %s, using existing pricing data from %s", err, p.spotUpdateTime.Format(time.RFC3339))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := p.updateFargatePricing(ctx); err != nil {
			log.Printf("updating fargate pricing, %s", err)
		}
	}()
	wg.Wait()
	p.notify()
}

func (p *Provider) updateOnDemandPricing(ctx context.Context) error {
	// standard on-demand instances
	var wg sync.WaitGroup
	var onDemandPrices, onDemandMetalPrices map[string]float64
	var onDemandErr, onDemandMetalErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		onDemandPrices, onDemandErr = p.fetchOnDemandPricing(ctx,
			&pricing.Filter{
				Field: aws.String("tenancy"),
				Type:  aws.String("TERM_MATCH"),
				Value: aws.String("Shared"),
			},
			&pricing.Filter{
				Field: aws.String("productFamily"),
				Type:  aws.String("TERM_MATCH"),
				Value: aws.String("Compute Instance"),
			})
	}()

	// bare metal on-demand prices
	wg.Add(1)
	go func() {
		defer wg.Done()
		onDemandMetalPrices, onDemandMetalErr = p.fetchOnDemandPricing(ctx,
			&pricing.Filter{
				Field: aws.String("tenancy"),
				Type:  aws.String("TERM_MATCH"),
				Value: aws.String("Dedicated"),
			},
			&pricing.Filter{
				Field: aws.String("productFamily"),
				Type:  aws.String("TERM_MATCH"),
				Value: aws.String("Compute Instance (bare metal)"),
			})
	}()

	wg.Wait()
	err := multierr.Append(onDemandErr, onDemandMetalErr)
	if err != nil {
		return err
	}

	if len(onDemandPrices) == 0 || len(onDemandMetalPrices) == 0 {
		return errors.New("no on-demand pricing found")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	p.onDemandPrices = map[string]float64{}
	for _, m := range []map[string]float64{onDemandPrices, onDemandMetalPrices} {
		for k, v := range m {
			p.onDemandPrices[k] = v
		}
	}
	p.onDemandUpdateTime = time.Now()
	return nil
}

func (p *Provider) fetchOnDemandPricing(ctx context.Context, additionalFilters ...*pricing.Filter) (map[string]float64, error) {
	prices := map[string]float64{}
	filters := append([]*pricing.Filter{
		{
			Field: aws.String("regionCode"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String(p.region),
		},
		{
			Field: aws.String("serviceCode"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String("AmazonEC2"),
		},
		{
			Field: aws.String("preInstalledSw"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String("NA"),
		},
		{
			Field: aws.String("operatingSystem"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String("Linux"),
		},
		{
			Field: aws.String("capacitystatus"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String("Used"),
		},
		{
			Field: aws.String("marketoption"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String("OnDemand"),
		}},
		additionalFilters...)
	if err := p.pricing.GetProductsPagesWithContext(ctx, &pricing.GetProductsInput{
		Filters:     filters,
		ServiceCode: aws.String("AmazonEC2")}, p.onDemandPage(prices)); err != nil {
		return nil, err
	}
	return prices, nil
}

// turning off cyclo here, it measures as a 12 due to all of the type checks of the pricing data which returns a deeply
// nested map[string]interface{}
// nolint: gocyclo
func (p *Provider) onDemandPage(prices map[string]float64) func(output *pricing.GetProductsOutput, b bool) bool {
	// this isn't the full pricing struct, just the portions we care about
	type priceItem struct {
		Product struct {
			Attributes struct {
				InstanceType string
			}
		}
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit struct {
						USD string
					}
				}
			}
		}
	}

	return func(output *pricing.GetProductsOutput, b bool) bool {
		for _, outer := range output.PriceList {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			if err := enc.Encode(outer); err != nil {
				log.Printf("encoding %s", err)
			}
			dec := json.NewDecoder(&buf)
			var pItem priceItem
			if err := dec.Decode(&pItem); err != nil {
				log.Printf("decoding %s", err)
			}
			if pItem.Product.Attributes.InstanceType == "" {
				continue
			}
			for _, term := range pItem.Terms.OnDemand {
				for _, v := range term.PriceDimensions {
					price, err := strconv.ParseFloat(v.PricePerUnit.USD, 64)
					if err != nil || price == 0 {
						continue
					}
					prices[pItem.Product.Attributes.InstanceType] = price
				}
			}
		}
		return true
	}
}

// nolint: gocyclo
func (p *Provider) updateSpotPricing(ctx context.Context) error {
	totalOfferings := 0

	prices := map[string]map[string]float64{}
	if err := p.ec2.DescribeSpotPriceHistoryPagesWithContext(ctx, &ec2.DescribeSpotPriceHistoryInput{
		ProductDescriptions: []*string{aws.String("Linux/UNIX"), aws.String("Linux/UNIX (Amazon VPC)")},
		// get the latest spot price for each instance type
		StartTime: aws.Time(time.Now()),
	}, func(output *ec2.DescribeSpotPriceHistoryOutput, b bool) bool {
		for _, sph := range output.SpotPriceHistory {
			spotPriceStr := aws.StringValue(sph.SpotPrice)
			spotPrice, err := strconv.ParseFloat(spotPriceStr, 64)
			// these errors shouldn't occur, but if pricing API does have an error, we ignore the record
			if err != nil {
				log.Printf("unable to parse price record %#v", sph)
				continue
			}
			if sph.Timestamp == nil {
				continue
			}
			instanceType := aws.StringValue(sph.InstanceType)
			az := aws.StringValue(sph.AvailabilityZone)
			_, ok := prices[instanceType]
			if !ok {
				prices[instanceType] = map[string]float64{}
			}
			prices[instanceType][az] = spotPrice
		}
		return true
	}); err != nil {
		return err
	}
	if len(prices) == 0 {
		return errors.New("no spot pricing found")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	for it, zoneData := range prices {
		if _, ok := p.spotPrices[it]; !ok {
			p.spotPrices[it] = newZonalPricing(0)
		}
		for zone, price := range zoneData {
			p.spotPrices[it].prices[zone] = price
		}
		totalOfferings += len(zoneData)
	}

	p.spotUpdateTime = time.Now()
	return nil
}

func (p *Provider) LivenessProbe(_ *http.Request) error {
	// ensure we don't deadlock and nolint for the empty critical section
	p.mu.Lock()
	//nolint: staticcheck
	p.mu.Unlock()
	return nil
}

func (p *Provider) updateFargatePricing(ctx context.Context) error {
	filters := append([]*pricing.Filter{
		{
			Field: aws.String("regionCode"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String(p.region),
		},
	})
	if err := p.pricing.GetProductsPagesWithContext(ctx, &pricing.GetProductsInput{
		Filters:     filters,
		ServiceCode: aws.String("AmazonEKS")}, p.fargatePage); err != nil {
		return err
	}
	return nil
}

func (p *Provider) fargatePage(output *pricing.GetProductsOutput, _ bool) bool {
	// this isn't the full pricing struct, just the portions we care about
	type priceItem struct {
		Product struct {
			ProductFamily string
			Attributes    struct {
				UsageType  string
				MemoryType string
			}
		}
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit struct {
						USD string
					}
				}
			}
		}
	}

	for _, outer := range output.PriceList {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		if err := enc.Encode(outer); err != nil {
			log.Printf("encoding %s", err)
		}
		dec := json.NewDecoder(&buf)
		var pItem priceItem
		if err := dec.Decode(&pItem); err != nil {
			log.Printf("decoding %s", err)
		}
		if !strings.Contains(pItem.Product.Attributes.UsageType, "Fargate") {
			continue
		}
		name := pItem.Product.Attributes.UsageType
		for _, term := range pItem.Terms.OnDemand {
			for _, v := range term.PriceDimensions {
				price, err := strconv.ParseFloat(v.PricePerUnit.USD, 64)
				if err != nil || price == 0 {
					continue
				}
				if strings.Contains(name, "vCPU-Hours") {
					p.mu.Lock()
					p.fargateVCPUPricePerHour = price
					p.mu.Unlock()
				} else if strings.Contains(name, "GB-Hours") {
					p.mu.Lock()
					p.fargateGBPricePerHour = price
					p.mu.Unlock()
				} else {
					log.Println("unsupported fargate price information found", name)
				}
			}
		}
	}
	return true

}
