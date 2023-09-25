package pkg

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

type RGWMetrics struct {
	registry *prometheus.Registry

	ops        *operationsCollector
	bucketInfo *bucketsCollector
	userInfo   *userInfoCollector

	// Misc
	scrapeDurationSeconds *prometheus.GaugeVec
	scrapeCountTotal      *prometheus.CounterVec
}

func NewRGWMetrics() *RGWMetrics {
	scrapeDurationSeconds := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "radosgw_usage",
			Name:      "scrape_duration_seconds",
			Help:      "Amount of time each scrape takes",
		},
		[]string{"type"},
	)
	scrapeCountTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "radosgw_usage",
			Name:      "scrape_count_total",
			Help:      "Number of times a scrape has happened",
		},
		[]string{"type", "status"},
	)

	metrics := &RGWMetrics{
		registry: prometheus.NewRegistry(),

		ops:        newOperationsCollector(scrapeDurationSeconds, scrapeCountTotal),
		bucketInfo: newBucketsCollector(scrapeDurationSeconds, scrapeCountTotal),
		userInfo:   newUserInfoCollector(scrapeDurationSeconds, scrapeCountTotal),

		scrapeDurationSeconds: scrapeDurationSeconds,
		scrapeCountTotal:      scrapeCountTotal,
	}

	metrics.registry.MustRegister(metrics.ops)
	metrics.registry.MustRegister(metrics.bucketInfo)
	metrics.registry.MustRegister(metrics.userInfo)
	metrics.registry.MustRegister(metrics.scrapeDurationSeconds)
	metrics.registry.MustRegister(metrics.scrapeCountTotal)

	return metrics
}

// StartScraping will launch goroutines to scrape RGW metrics from Ceph at `interval` time period
func (m *RGWMetrics) StartScraping(ctx context.Context, log *logrus.Logger, client *http.Client, rgwURL *url.URL, creds *credentials.Credentials, interval time.Duration) {
	go m.ops.FetchMetrics(ctx, log, client, rgwURL, creds, interval)
	go m.bucketInfo.FetchMetrics(ctx, log, client, rgwURL, creds, interval)
	go m.userInfo.FetchMetrics(ctx, log, client, rgwURL, creds, interval)
}

func (m *RGWMetrics) Handler() http.Handler {
	return promhttp.InstrumentMetricHandler(
		m.registry, promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}),
	)
}

type operationsCollector struct {
	sync.Mutex
	metrics []prometheus.Metric

	opsTotal           *prometheus.Desc
	opsSuccessful      *prometheus.Desc
	sentBytesTotal     *prometheus.Desc
	receivedBytesTotal *prometheus.Desc

	scrapeDurationSeconds *prometheus.GaugeVec
	scrapeCountTotal      *prometheus.CounterVec
}

func newOperationsCollector(scrapeDurationSeconds *prometheus.GaugeVec, scrapeCountTotal *prometheus.CounterVec) *operationsCollector {
	return &operationsCollector{
		metrics: []prometheus.Metric{},

		opsTotal: prometheus.NewDesc(
			"radosgw_usage_opts_total",
			"Number of operations",
			[]string{"bucket", "owner", "category"},
			prometheus.Labels{},
		),
		opsSuccessful: prometheus.NewDesc(
			"radosgw_usage_successful_ops_total",
			"Number of successful operations",
			[]string{"bucket", "owner", "category"},
			prometheus.Labels{},
		),
		sentBytesTotal: prometheus.NewDesc(
			"radosgw_usage_sent_bytes_total",
			"Bytes sent by RGW",

			[]string{"bucket", "owner", "category"},
			prometheus.Labels{},
		),
		receivedBytesTotal: prometheus.NewDesc(
			"radosgw_usage_received_bytes_total",
			"Bytes received by RGW",

			[]string{"bucket", "owner", "category"},
			prometheus.Labels{},
		),

		scrapeDurationSeconds: scrapeDurationSeconds.MustCurryWith(prometheus.Labels{"type": "ops"}),
		scrapeCountTotal:      scrapeCountTotal.MustCurryWith(prometheus.Labels{"type": "ops"}),
	}
}

func (c *operationsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.opsTotal
	ch <- c.opsSuccessful
	ch <- c.sentBytesTotal
	ch <- c.receivedBytesTotal
}

func (c *operationsCollector) Collect(ch chan<- prometheus.Metric) {
	c.Lock()
	defer c.Unlock()

	for _, metric := range c.metrics {
		ch <- metric
	}
}

// FetchMetrics will fetch operations metrics from Ceph in an infinite loop until ctx is cancelled
// It uses a Ticker to attempt to fetch from Ceph every `interval` time period
func (c *operationsCollector) FetchMetrics(ctx context.Context, log *logrus.Logger, client *http.Client, rgwURL *url.URL, creds *credentials.Credentials, interval time.Duration) {
	ticker := time.NewTicker(interval)

	for {
		if ctx.Err() != nil {
			return
		}

		func() {
			start := time.Now()

			usageStats, err := getCephUsageStats(client, rgwURL, creds)

			c.scrapeDurationSeconds.WithLabelValues().Set(time.Since(start).Seconds())

			if err != nil {
				c.scrapeCountTotal.With(prometheus.Labels{"status": "error"}).Inc()
				log.Errorf("Failed to scrape Ceph usage stats - %v", err)
				return
			}

			c.scrapeCountTotal.With(prometheus.Labels{"status": "success"}).Inc()

			// Ceph will sometimes return duplicate entries with different counts
			// We have to combine those before returning counters to Prometheus
			type usageKey struct {
				Owner    string
				Bucket   string
				Category string
			}

			type usageValue struct {
				OpsTotal           int64
				OpsSuccessful      int64
				SentBytesTotal     int64
				ReceivedBytesTotal int64
			}

			combinedUsageStats := map[usageKey]usageValue{}

			for _, entry := range usageStats.Entries {
				owner := entry.User
				for _, bucket := range entry.Buckets {
					bucketName := bucket.ID

					for _, category := range bucket.Categories {
						key := usageKey{
							Owner:    owner,
							Bucket:   bucketName,
							Category: category.Name,
						}

						currentValue := combinedUsageStats[key]

						currentValue.OpsTotal += category.Ops
						currentValue.OpsSuccessful += category.SuccessfulOps
						currentValue.SentBytesTotal += category.BytesSent
						currentValue.ReceivedBytesTotal += category.BytesReceived

						combinedUsageStats[key] = currentValue
					}
				}
			}

			// Now create the metrics from the combined usage stats
			metrics := []prometheus.Metric{}
			for key, value := range combinedUsageStats {
				metrics = append(metrics,
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.opsTotal,
							prometheus.CounterValue,
							float64(value.OpsTotal),
							key.Bucket, key.Owner, key.Category,
						),
					),
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.opsSuccessful,
							prometheus.CounterValue,
							float64(value.OpsSuccessful),
							key.Bucket, key.Owner, key.Category,
						),
					),
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.sentBytesTotal,
							prometheus.CounterValue,
							float64(value.SentBytesTotal),
							key.Bucket, key.Owner, key.Category,
						),
					),
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.receivedBytesTotal,
							prometheus.CounterValue,
							float64(value.ReceivedBytesTotal),
							key.Bucket, key.Owner, key.Category,
						),
					),
				)
			}

			// Update the metrics
			c.Lock()
			c.metrics = metrics
			c.Unlock()
		}()

		// Wait for the next tick event or ctx cancel
		select {
		case <-ticker.C:
			// Loop
		case <-ctx.Done():
			return
		}
	}
}

type bucketsCollector struct {
	sync.Mutex
	metrics []prometheus.Metric

	bucketUsedBytes           *prometheus.Desc
	bucketUtilizedBytes       *prometheus.Desc
	bucketObjectCount         *prometheus.Desc
	bucketShardCount          *prometheus.Desc
	bucketQuotaEnabled        *prometheus.Desc
	bucketQuotaMaxSizeBytes   *prometheus.Desc
	bucketQuotaMaxObjectCount *prometheus.Desc

	scrapeDurationSeconds *prometheus.GaugeVec
	scrapeCountTotal      *prometheus.CounterVec
}

func newBucketsCollector(scrapeDurationSeconds *prometheus.GaugeVec, scrapeCountTotal *prometheus.CounterVec) *bucketsCollector {
	return &bucketsCollector{
		metrics: []prometheus.Metric{},

		bucketUsedBytes: prometheus.NewDesc(
			"radosgw_usage_bucket_bytes",
			"Bucket used bytes",
			[]string{"bucket", "owner", "zonegroup"},
			prometheus.Labels{},
		),
		bucketUtilizedBytes: prometheus.NewDesc(
			"radosgw_usage_bucket_utilized_bytes",
			"Bucket utilized bytes",
			[]string{"bucket", "owner", "zonegroup"},
			prometheus.Labels{},
		),
		bucketObjectCount: prometheus.NewDesc(
			"radosgw_usage_bucket_objects",
			"Number of objects in the bucket",
			[]string{"bucket", "owner", "zonegroup"},
			prometheus.Labels{},
		),
		bucketShardCount: prometheus.NewDesc(
			"radosgw_usage_bucket_shards",
			"Number of index shards for the bucket",
			[]string{"bucket", "owner", "zonegroup"},
			prometheus.Labels{},
		),
		bucketQuotaEnabled: prometheus.NewDesc(
			"radosgw_usage_bucket_quota_enabled",
			"Whether a quota is enabled for the bucket",
			[]string{"bucket", "owner", "zonegroup"},
			prometheus.Labels{},
		),
		bucketQuotaMaxSizeBytes: prometheus.NewDesc(
			"radosgw_usage_bucket_quota_size_bytes",
			"Maximum allowed size of the bucket",
			[]string{"bucket", "owner", "zonegroup"},
			prometheus.Labels{},
		),
		bucketQuotaMaxObjectCount: prometheus.NewDesc(
			"radosgw_usage_bucket_quota_size_objects",
			"Maximum allowed number of objects in the bucket",
			[]string{"bucket", "owner", "zonegroup"},
			prometheus.Labels{},
		),

		scrapeDurationSeconds: scrapeDurationSeconds.MustCurryWith(prometheus.Labels{"type": "buckets"}),
		scrapeCountTotal:      scrapeCountTotal.MustCurryWith(prometheus.Labels{"type": "buckets"}),
	}
}

func (c *bucketsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.bucketUsedBytes
	ch <- c.bucketUtilizedBytes
	ch <- c.bucketObjectCount
	ch <- c.bucketShardCount
	ch <- c.bucketQuotaEnabled
	ch <- c.bucketQuotaMaxSizeBytes
	ch <- c.bucketQuotaMaxObjectCount
}

func (c *bucketsCollector) Collect(ch chan<- prometheus.Metric) {
	c.Lock()
	defer c.Unlock()

	for _, metric := range c.metrics {
		ch <- metric
	}
}

// FetchMetrics will fetch bucket metrics from Ceph in an infinite loop until ctx is cancelled
// It uses a Ticker to attempt to fetch from Ceph every `interval` time period
func (c *bucketsCollector) FetchMetrics(ctx context.Context, log *logrus.Logger, client *http.Client, rgwURL *url.URL, creds *credentials.Credentials, interval time.Duration) {
	ticker := time.NewTicker(interval)

	for {
		if ctx.Err() != nil {
			return
		}

		func() {
			start := time.Now()

			bucketStats, err := getCephBucketStats(client, rgwURL, creds)

			c.scrapeDurationSeconds.WithLabelValues().Set(time.Since(start).Seconds())

			if err != nil {
				c.scrapeCountTotal.With(prometheus.Labels{"status": "error"}).Inc()
				log.Errorf("Failed to scrape Ceph usage stats - %v", err)
				return
			}

			c.scrapeCountTotal.With(prometheus.Labels{"status": "success"}).Inc()

			metrics := []prometheus.Metric{}
			for _, bucketInfo := range bucketStats {
				metrics = append(metrics,
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.bucketShardCount,
							prometheus.GaugeValue,
							float64(bucketInfo.NumShards),
							bucketInfo.Name, bucketInfo.Owner, bucketInfo.ZoneGroup,
						),
					),
				)

				if usage, ok := bucketInfo.Usage["rgw.main"]; ok {
					metrics = append(metrics,
						prometheus.NewMetricWithTimestamp(
							start,
							prometheus.MustNewConstMetric(
								c.bucketUsedBytes,
								prometheus.GaugeValue,
								float64(usage.Size),
								bucketInfo.Name, bucketInfo.Owner, bucketInfo.ZoneGroup,
							),
						),
						prometheus.NewMetricWithTimestamp(
							start,
							prometheus.MustNewConstMetric(
								c.bucketUtilizedBytes,
								prometheus.GaugeValue,
								float64(usage.UtilizedSize),
								bucketInfo.Name, bucketInfo.Owner, bucketInfo.ZoneGroup,
							),
						),
						prometheus.NewMetricWithTimestamp(
							start,
							prometheus.MustNewConstMetric(
								c.bucketObjectCount,
								prometheus.GaugeValue,
								float64(usage.NumObjects),
								bucketInfo.Name, bucketInfo.Owner, bucketInfo.ZoneGroup,
							),
						),
					)
				}

				bucketQuotaEnabled := 1.0
				if !bucketInfo.Quota.Enabled {
					bucketQuotaEnabled = 0.0
				}

				metrics = append(metrics,
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.bucketQuotaEnabled,
							prometheus.GaugeValue,
							bucketQuotaEnabled,
							bucketInfo.Name, bucketInfo.Owner, bucketInfo.ZoneGroup,
						),
					),
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.bucketQuotaMaxSizeBytes,
							prometheus.GaugeValue,
							float64(bucketInfo.Quota.MaxSize),
							bucketInfo.Name, bucketInfo.Owner, bucketInfo.ZoneGroup,
						),
					),
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.bucketQuotaMaxObjectCount,
							prometheus.GaugeValue,
							float64(bucketInfo.Quota.MaxObjects),
							bucketInfo.Name, bucketInfo.Owner, bucketInfo.ZoneGroup,
						),
					),
				)
			}

			// Update the metrics
			c.Lock()
			c.metrics = metrics
			c.Unlock()
		}()

		// Wait for the next tick event or ctx cancel
		select {
		case <-ticker.C:
			// Loop
		case <-ctx.Done():
			return
		}
	}
}

type userInfoCollector struct {
	sync.Mutex
	metrics []prometheus.Metric

	userQuotaEnabled      *prometheus.Desc
	userQuotaMaxSizeBytes *prometheus.Desc
	userQuotaMaxObjects   *prometheus.Desc

	scrapeDurationSeconds *prometheus.GaugeVec
	scrapeCountTotal      *prometheus.CounterVec
}

func newUserInfoCollector(scrapeDurationSeconds *prometheus.GaugeVec, scrapeCountTotal *prometheus.CounterVec) *userInfoCollector {
	return &userInfoCollector{
		metrics: []prometheus.Metric{},

		userQuotaEnabled: prometheus.NewDesc(
			"radosgw_usage_user_quota_enabled",
			"Whether a quota is enabled for the user",
			[]string{"user"},
			prometheus.Labels{},
		),
		userQuotaMaxSizeBytes: prometheus.NewDesc(
			"radosgw_usage_user_quota_size_bytes",
			"Maximum allowed size for the user",
			[]string{"user"},
			prometheus.Labels{},
		),
		userQuotaMaxObjects: prometheus.NewDesc(
			"radosgw_usage_user_quota_size_objects",
			"Maximum allowed number of objects for the user",
			[]string{"user"},
			prometheus.Labels{},
		),

		scrapeDurationSeconds: scrapeDurationSeconds.MustCurryWith(prometheus.Labels{"type": "users"}),
		scrapeCountTotal:      scrapeCountTotal.MustCurryWith(prometheus.Labels{"type": "users"}),
	}
}

func (c *userInfoCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.userQuotaEnabled
	ch <- c.userQuotaMaxSizeBytes
	ch <- c.userQuotaMaxObjects
}

func (c *userInfoCollector) Collect(ch chan<- prometheus.Metric) {
	c.Lock()
	defer c.Unlock()

	for _, metric := range c.metrics {
		ch <- metric
	}
}

// FetchMetrics will fetch user info metrics from Ceph in an infinite loop until ctx is cancelled
// It uses a Ticker to attempt to fetch from Ceph every `interval` time period
func (c *userInfoCollector) FetchMetrics(ctx context.Context, log *logrus.Logger, client *http.Client, rgwURL *url.URL, creds *credentials.Credentials, interval time.Duration) {
	ticker := time.NewTicker(interval)

	for {
		if ctx.Err() != nil {
			return
		}

		func() {
			start := time.Now()

			userQuotaInfo, err := getCephUserQuotaStats(client, rgwURL, creds)

			c.scrapeDurationSeconds.WithLabelValues().Set(time.Since(start).Seconds())

			if err != nil {
				c.scrapeCountTotal.With(prometheus.Labels{"status": "error"}).Inc()
				log.Errorf("Failed to scrape Ceph usage stats - %v", err)
				return
			}

			c.scrapeCountTotal.With(prometheus.Labels{"status": "success"}).Inc()

			metrics := []prometheus.Metric{}
			for userName, quotaInfo := range userQuotaInfo {
				userQuotaEnabled := 1.0
				if !quotaInfo.Enabled {
					userQuotaEnabled = 0.0
				}

				metrics = append(metrics,
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.userQuotaEnabled,
							prometheus.GaugeValue,
							userQuotaEnabled,
							userName,
						),
					),
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.userQuotaMaxSizeBytes,
							prometheus.GaugeValue,
							float64(quotaInfo.MaxSize),
							userName,
						),
					),
					prometheus.NewMetricWithTimestamp(
						start,
						prometheus.MustNewConstMetric(
							c.userQuotaMaxObjects,
							prometheus.GaugeValue,
							float64(quotaInfo.MaxObjects),
							userName,
						),
					),
				)
			}

			// Update the metrics
			c.Lock()
			c.metrics = metrics
			c.Unlock()
		}()

		// Wait for the next tick event or ctx cancel
		select {
		case <-ticker.C:
			// Loop
		case <-ctx.Done():
			return
		}
	}
}
