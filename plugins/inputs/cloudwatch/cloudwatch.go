package cloudwatch

import (
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatch"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	internalaws "github.com/influxdata/telegraf/internal/config/aws"
	"github.com/influxdata/telegraf/internal/limiter"
	"github.com/influxdata/telegraf/plugins/inputs"
)

type (
	CloudWatch struct {
		Region      string `toml:"region"`
		AccessKey   string `toml:"access_key"`
		SecretKey   string `toml:"secret_key"`
		RoleARN     string `toml:"role_arn"`
		Profile     string `toml:"profile"`
		Filename    string `toml:"shared_credential_file"`
		Token       string `toml:"token"`
		EndpointURL string `toml:"endpoint_url"`

		Period      internal.Duration `toml:"period"`
		Delay       internal.Duration `toml:"delay"`
		Namespace   string            `toml:"namespace"`
		Metrics     []*Metric         `toml:"metrics"`
		CacheTTL    internal.Duration `toml:"cache_ttl"`
		RateLimit   int               `toml:"ratelimit"`
		client      cloudwatchClient
		metricCache *MetricCache
		windowStart time.Time
		windowEnd   time.Time

		Debug bool `toml:"debug"`
	}

	Metric struct {
		MetricNames []string     `toml:"names"`
		Dimensions  []*Dimension `toml:"dimensions"`
	}

	Dimension struct {
		Name  string `toml:"name"`
		Value string `toml:"value"`
	}

	MetricCache struct {
		TTL     time.Duration
		Fetched time.Time
		Metrics []*cloudwatch.Metric
	}

	cloudwatchClient interface {
		ListMetrics(*cloudwatch.ListMetricsInput) (*cloudwatch.ListMetricsOutput, error)
		GetMetricData(*cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error)
	}
)

func (c *CloudWatch) SampleConfig() string {
	return `
  ## Amazon Region
  region = "us-east-1"

  ## Amazon Credentials
  ## Credentials are loaded in the following order
  ## 1) Assumed credentials via STS if role_arn is specified
  ## 2) explicit credentials from 'access_key' and 'secret_key'
  ## 3) shared profile from 'profile'
  ## 4) environment variables
  ## 5) shared credentials file
  ## 6) EC2 Instance Profile
  # access_key = ""
  # secret_key = ""
  # token = ""
  # role_arn = ""
  # profile = ""
  # shared_credential_file = ""

  ## Endpoint to make request against, the correct endpoint is automatically
  ## determined and this option should only be set if you wish to override the
  ## default.
  ##   ex: endpoint_url = "http://localhost:8000"
  # endpoint_url = ""

  # The minimum period for Cloudwatch metrics is 1 minute (60s). However not all
  # metrics are made available to the 1 minute period. Some are collected at
  # 3 minute, 5 minute, or larger intervals. See https://aws.amazon.com/cloudwatch/faqs/#monitoring.
  # Note that if a period is configured that is smaller than the minimum for a
  # particular metric, that metric will not be returned by the Cloudwatch API
  # and will not be collected by Telegraf.
  #
  ## Requested CloudWatch aggregation Period (required - must be a multiple of 60s)
  period = "5m"

  ## Collection Delay (required - must account for metrics availability via CloudWatch API)
  delay = "5m"

  ## Recommended: use metric 'interval' that is a multiple of 'period' to avoid
  ## gaps or overlap in pulled data
  interval = "5m"

  ## Configure the TTL for the internal cache of metrics.
  ## Defaults to 1 hr if not specified
  # cache_ttl = "10m"

  ## Metric Statistic Namespace (required)
  namespace = "AWS/ELB"

  ## Maximum requests per second. Note that the global default AWS rate limit is
  ## 400 reqs/sec, so if you define multiple namespaces, these should add up to a
  ## maximum of 400. Optional - default value is 200.
  ## See http://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/cloudwatch_limits.html
  ratelimit = 200

  ## Metrics to Pull (optional)
  ## Defaults to all Metrics in Namespace if nothing is provided
  ## Refreshes Namespace available metrics every 1h
  #[[inputs.cloudwatch.metrics]]
  #  names = ["Latency", "RequestCount"]
  #
  #  ## Dimension filters for Metric.  These are optional however all dimensions
  #  ## defined for the metric names must be specified in order to retrieve
  #  ## the metric statistics.
  #  [[inputs.cloudwatch.metrics.dimensions]]
  #    name = "LoadBalancerName"
  #    value = "p-example"
`
}

func (c *CloudWatch) Description() string {
	return "Pull Metric Statistics from Amazon CloudWatch"
}

func SelectMetrics(c *CloudWatch) ([]*cloudwatch.Metric, error) {
	var metrics []*cloudwatch.Metric

	// check for provided metric filter
	if c.Metrics != nil {
		metrics = []*cloudwatch.Metric{}
		for _, m := range c.Metrics {
			if !hasWilcard(m.Dimensions) {
				dimensions := make([]*cloudwatch.Dimension, len(m.Dimensions))
				for k, d := range m.Dimensions {
					dimensions[k] = &cloudwatch.Dimension{
						Name:  aws.String(d.Name),
						Value: aws.String(d.Value),
					}
				}
				for _, name := range m.MetricNames {
					metrics = append(metrics, &cloudwatch.Metric{
						Namespace:  aws.String(c.Namespace),
						MetricName: aws.String(name),
						Dimensions: dimensions,
					})
				}
			} else {
				allMetrics, err := c.fetchNamespaceMetrics()
				if err != nil {
					return nil, err
				}
				for _, name := range m.MetricNames {
					for _, metric := range allMetrics {
						if isSelected(name, metric, m.Dimensions) {
							metrics = append(metrics, &cloudwatch.Metric{
								Namespace:  aws.String(c.Namespace),
								MetricName: aws.String(name),
								Dimensions: metric.Dimensions,
							})
						}
					}
				}
			}
		}
	} else {
		var err error
		metrics, err = c.fetchNamespaceMetrics()
		if err != nil {
			return nil, err
		}
	}
	return metrics, nil
}

func (c *CloudWatch) Gather(acc telegraf.Accumulator) error {
	if c.client == nil {
		c.initializeCloudWatch()
	}

	metrics, err := SelectMetrics(c)
	if err != nil {
		return err
	}

	now := time.Now()

	err = c.updateWindow(now)
	if err != nil {
		return err
	}

	// limit concurrency or we can easily exhaust user connection limit
	// see cloudwatch API request limits:
	// http://docs.aws.amazon.com/AmazonCloudWatch/latest/DeveloperGuide/cloudwatch_limits.html
	lmtr := limiter.NewRateLimiter(c.RateLimit, time.Second)
	defer lmtr.Stop()
	wg := sync.WaitGroup{}

	// get all of the possible queries so we can send groups of 100
	// todo: cache these as well and do the regexp replace to cover all possible bad chars.
	queries := c.getDataQueries(metrics)

	// create master list of results so aggregation works the best way.
	results := []*cloudwatch.MetricDataResult{}

	// loop through metrics and send groups of 100 at a time to gatherMetrics
	// gatherMetrics(acc, metrics[min:max]...)
	groups := len(queries) / 100
	for i := 0; i < groups; i++ {
		wg.Add(1)
		<-lmtr.C
		go func(inm []*cloudwatch.MetricDataQuery) {
			defer wg.Done()
			result, err := c.gatherMetrics(c.getDataInputs(inm))
			if err != nil {
				acc.AddError(err)
				// return
			}
			results = append(results, result...)

		}(queries[i*100 : (i+1)*100])
	}

	// gather remainder (or initial) group
	<-lmtr.C
	wg.Add(1)
	go func(inm []*cloudwatch.MetricDataQuery) {
		defer wg.Done()
		result, err := c.gatherMetrics(c.getDataInputs(inm))
		if err != nil {
			acc.AddError(err)
			// return
		}
		results = append(results, result...)
	}(queries[(groups * 100):])

	wg.Wait()

	acc.AddError(c.aggregateMetrics(acc, metrics, results))

	return nil
}

func (c *CloudWatch) updateWindow(relativeTo time.Time) error {
	windowEnd := relativeTo.Add(-c.Delay.Duration)

	if c.windowEnd.IsZero() {
		// this is the first run, no window info, so just get a single period
		c.windowStart = windowEnd.Add(-c.Period.Duration)
	} else {
		// subsequent window, start where last window left off
		c.windowStart = c.windowEnd
	}

	c.windowEnd = windowEnd

	return nil
}

func init() {
	inputs.Add("cloudwatch", func() telegraf.Input {
		ttl, _ := time.ParseDuration("1hr")
		return &CloudWatch{
			CacheTTL:  internal.Duration{Duration: ttl},
			RateLimit: 200,
		}
	})
}

/*
 * Initialize CloudWatch client
 */
func (c *CloudWatch) initializeCloudWatch() error {
	credentialConfig := &internalaws.CredentialConfig{
		Region:      c.Region,
		AccessKey:   c.AccessKey,
		SecretKey:   c.SecretKey,
		RoleARN:     c.RoleARN,
		Profile:     c.Profile,
		Filename:    c.Filename,
		Token:       c.Token,
		EndpointURL: c.EndpointURL,
	}
	configProvider := credentialConfig.Credentials()

	cfg := &aws.Config{}
	loglevel := aws.LogOff
	if c.Debug {
		loglevel = aws.LogDebug
	}
	c.client = cloudwatch.New(configProvider, cfg.WithLogLevel(loglevel))
	return nil
}

/*
 * Fetch available metrics for given CloudWatch Namespace
 */
func (c *CloudWatch) fetchNamespaceMetrics() ([]*cloudwatch.Metric, error) {
	if c.metricCache != nil && c.metricCache.IsValid() {
		return c.metricCache.Metrics, nil
	}

	metrics := []*cloudwatch.Metric{}

	var token *string
	for more := true; more; {
		params := &cloudwatch.ListMetricsInput{
			Namespace:  aws.String(c.Namespace),
			Dimensions: []*cloudwatch.DimensionFilter{},
			NextToken:  token,
			MetricName: nil,
		}

		resp, err := c.client.ListMetrics(params)
		if err != nil {
			return nil, err
		}

		metrics = append(metrics, resp.Metrics...)

		token = resp.NextToken
		more = token != nil
	}

	c.metricCache = &MetricCache{
		Metrics: metrics,
		Fetched: time.Now(),
		TTL:     c.CacheTTL.Duration,
	}

	return metrics, nil
}

/*
 * Gather given Metric and emit any error
 */
func (c *CloudWatch) gatherMetrics(
	params *cloudwatch.GetMetricDataInput,
) ([]*cloudwatch.MetricDataResult, error) {
	resp, err := c.client.GetMetricData(params)
	if err != nil {
		return nil, errors.New("Failed to get metric data - " + err.Error())
	}

	// todo: determine if we need to handle pagination.
	if resp.NextToken != nil {
		log.Println("[inputs.cloudwatch] W! UNHANDLED PAGINATED RESULTS!!", *resp.NextToken)
	}
	return resp.MetricDataResults, nil
}

func (c *CloudWatch) aggregateMetrics(
	acc telegraf.Accumulator,
	metrics []*cloudwatch.Metric,
	metricDataResults []*cloudwatch.MetricDataResult,
) error {
	for _, metric := range metrics {
		timestamp := time.Now()
		fields := map[string]interface{}{}
		tags := map[string]string{
			"region": c.Region,
		}

		// todo: groupt these once beforehand if possible to avoid looping through all results metrics number of times
		results := getResults(*metric, metricDataResults)
		for _, result := range results {
			for _, dimension := range metric.Dimensions {
				tags[snakeCase(*dimension.Name)] = *dimension.Value
			}
			if len(result.Timestamps) == 0 || result.Timestamps[0] == nil {
				continue
			}
			// todo: determine if we need to handle multiple values.
			if len(result.Values) > 1 {
				log.Printf("[inputs.cloudwatch] W! UNHANDLED MULTIPLE RESULT VALUES!! %+v\n", result.Values)
			}

			fields[*result.Label] = *result.Values[0]
			timestamp = *result.Timestamps[0]
		}

		acc.AddFields(formatMeasurement(c.Namespace), fields, tags, timestamp)
	}

	return nil
}

func getResults(metric cloudwatch.Metric, results []*cloudwatch.MetricDataResult) []*cloudwatch.MetricDataResult {
	list := []*cloudwatch.MetricDataResult{}
	for _, result := range results {
		if nameMatch(genID(metric), *result.Id) {
			list = append(list, result)
		}
	}
	return list
}

func nameMatch(name, id string) bool {
	if strings.TrimPrefix(id, "average_") == snakeCase(name) {
		return true
	}
	if strings.TrimPrefix(id, "maximum_") == snakeCase(name) {
		return true
	}
	if strings.TrimPrefix(id, "minimum_") == snakeCase(name) {
		return true
	}
	if strings.TrimPrefix(id, "sum_") == snakeCase(name) {
		return true
	}
	if strings.TrimPrefix(id, "sample_count_") == snakeCase(name) {
		return true
	}
	return false
}

/*
 * Formatting helpers
 */
func formatMeasurement(namespace string) string {
	namespace = strings.Replace(namespace, "/", "_", -1)
	namespace = snakeCase(namespace)
	return "cloudwatch_" + namespace
}

func snakeCase(s string) string {
	s = internal.SnakeCase(s)
	s = strings.Replace(s, "__", "_", -1)
	return s
}

func collect(check string) bool {
	// if check matches a specified statistic type to collect
	return true
}

// get all of the possible queries so we can send groups of 100
func (c *CloudWatch) getDataQueries(metrics []*cloudwatch.Metric) []*cloudwatch.MetricDataQuery {
	dataQueries := []*cloudwatch.MetricDataQuery{}
	for _, metric := range metrics {
		if collect("average") {
			dataQueries = append(dataQueries, &cloudwatch.MetricDataQuery{
				Id:    aws.String("average_" + genID(*metric)),
				Label: aws.String(snakeCase(*metric.MetricName + "_average")),
				MetricStat: &cloudwatch.MetricStat{
					Metric: metric,
					Period: aws.Int64(int64(c.Period.Duration.Seconds())),
					Stat:   aws.String(cloudwatch.StatisticAverage),
				},
			})
		}
		if collect("maximum") {
			dataQueries = append(dataQueries, &cloudwatch.MetricDataQuery{
				Id:    aws.String("maximum_" + genID(*metric)),
				Label: aws.String(snakeCase(*metric.MetricName + "_maximum")),
				MetricStat: &cloudwatch.MetricStat{
					Metric: metric,
					Period: aws.Int64(int64(c.Period.Duration.Seconds())),
					Stat:   aws.String(cloudwatch.StatisticMaximum),
				},
			})
		}
		if collect("minimum") {
			dataQueries = append(dataQueries, &cloudwatch.MetricDataQuery{
				Id:    aws.String("minimum_" + genID(*metric)),
				Label: aws.String(snakeCase(*metric.MetricName + "_minimum")),
				MetricStat: &cloudwatch.MetricStat{
					Metric: metric,
					Period: aws.Int64(int64(c.Period.Duration.Seconds())),
					Stat:   aws.String(cloudwatch.StatisticMinimum),
				},
			})
		}
		if collect("sum") {
			dataQueries = append(dataQueries, &cloudwatch.MetricDataQuery{
				Id:    aws.String("sum_" + genID(*metric)),
				Label: aws.String(snakeCase(*metric.MetricName + "_sum")),
				MetricStat: &cloudwatch.MetricStat{
					Metric: metric,
					Period: aws.Int64(int64(c.Period.Duration.Seconds())),
					Stat:   aws.String(cloudwatch.StatisticSum),
				},
			})
		}
		if collect("sample_count") {
			dataQueries = append(dataQueries, &cloudwatch.MetricDataQuery{
				Id:    aws.String("sample_count_" + genID(*metric)),
				Label: aws.String(snakeCase(*metric.MetricName + "_sample_count")),
				MetricStat: &cloudwatch.MetricStat{
					Metric: metric,
					Period: aws.Int64(int64(c.Period.Duration.Seconds())),
					Stat:   aws.String(cloudwatch.StatisticSampleCount),
				},
			})
		}
	}
	return dataQueries
}

func (c *CloudWatch) getDataInputs(dataQueries []*cloudwatch.MetricDataQuery) *cloudwatch.GetMetricDataInput {
	return &cloudwatch.GetMetricDataInput{
		StartTime:         aws.Time(c.windowStart),
		EndTime:           aws.Time(c.windowEnd),
		MetricDataQueries: dataQueries,
	}
}

// while a regex of "[^a-zA-Z0-9_]+" doing a ReplaceAllString would cover more, strings.Replacer is ~10x faster.
var replacer = strings.NewReplacer("/", "_", "-", "_", ".", "_", " ", "_", ":", "_", "{", "", "}", "", "%", "")

func genID(metric cloudwatch.Metric) string {
	dVals := []string{}
	for _, dimension := range metric.Dimensions {
		dVals = append(dVals, replacer.Replace(*dimension.Value))
	}
	if strings.Contains(*metric.MetricName, "EBSIOBalance") || strings.Contains(*metric.MetricName, "EBSByteBalance") {
		*metric.MetricName = strings.TrimRight(*metric.MetricName, "%")
	}
	if len(dVals) > 0 {
		id := snakeCase(*metric.MetricName + "_" + strings.Join(dVals, "_"))
		if len(id) > 255 {
			log.Printf("[inputs.cloudwatch] W! ID TOO LARGE, TRIMMING; POSSIBLE DUPLICATE ID!! %+v\n", len(id))
			return id[:255]
		}
		return id
	}
	return snakeCase(*metric.MetricName)
}

/*
 * Check Metric Cache validity
 */
func (c *MetricCache) IsValid() bool {
	return c.Metrics != nil && time.Since(c.Fetched) < c.TTL
}

func hasWilcard(dimensions []*Dimension) bool {
	for _, d := range dimensions {
		if d.Value == "" || d.Value == "*" {
			return true
		}
	}
	return false
}

func isSelected(name string, metric *cloudwatch.Metric, dimensions []*Dimension) bool {
	if name != *metric.MetricName {
		return false
	}
	if len(metric.Dimensions) != len(dimensions) {
		return false
	}
	for _, d := range dimensions {
		selected := false
		for _, d2 := range metric.Dimensions {
			if d.Name == *d2.Name {
				if d.Value == "" || d.Value == "*" || d.Value == *d2.Value {
					selected = true
				}
			}
		}
		if !selected {
			return false
		}
	}
	return true
}
