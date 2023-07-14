# Ceph RadosGW Usage Exporter

[Prometheus](https://prometheus.io/) exporter that scrapes [Ceph RadosGW](https://docs.ceph.com/en/latest/radosgw/) usage information (operation, buckets, and user). This info is gathered from RGW using the [Admin Operations API](https://docs.ceph.com/en/latest/radosgw/adminops/).

## Requirements

* Ceph RGW must have the admin API enabled via config. (It is enabled by default)

```
rgw enable apis = "s3, admin"
```

* This exporter requires a user that has the following capabilities:
  * `buckets=read`
  * `users=read`
  * `usage=read` (only if you enable the usage log. See below)
* If using a loadbalancer in front of RGW, please make sure your timeouts are set appropriately. Clusters with a large number of buckets or large number of users+buckets could cause the usage query to exceed the loadbalancer timeout

## Optional

* To get operations usage metrics, you must enable the usage log

```
rgw enable usage log = true
```

## Configuration

All inputs / config is done via ENV variables

| Variable                | Default | Required? | Description                                                                                                                                                                                                                         |
| ----------------------- | ------- | --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| RGW_EXPORTER_PORT       |         | Required  | The URL of the RadosGW instance to scrape (example: https://objects.example.com/)                                                                                                                                                   |
| RGW_EXPORTER_RGW_URL    |         | Required  | The URL of the RadosGW instance to scrape (example: https://objects.example.com/)                                                                                                                                                   |
| RGW_EXPORTER_ACCESS_KEY |         | Required  | S3-style access key of the user to use for scraping                                                                                                                                                                                 |
| RGW_EXPORTER_SECRET_KEY |         | Required  | S3-style secret key of the user to use for scraping                                                                                                                                                                                 |
| RGW_EXPORTER_LOG_LEVEL  | "info"  |           | The log level to use [debug, info, warn, error, fatal]                                                                                                                                                                              |
| RGW_EXPORTER_INTERVAL   | "1m"    |           | How often to scrape ceph. NOTE: This is a *minimum* duration between scrapes. If a scrape takes longer than the interval, multiple scrapes will not overlap. The current scrape will finish and then immediately start a new scrape |

## Usage

This exporter is published as a docker container: `ghcr.io/richiesams/radosgw-exporter:v<tag>`

You can run the container directly

```bash
docker run --rm \
    -e RGW_EXPORTER_RGW_URL=https://objects.example.com/ \
    -e RGW_EXPORTER_ACCESS_KEY=MY_ACCESS_KEY \
    -e RGW_EXPORTER_ACCESS_KEY=MY_SECRET_KEY \
    -p 8080:8080 \
    $(IMAGENAME):$(TAG)
```

Or via your favorite orchestration system

Metrics will be exposed at the `/metrics` endpoint

## Final notes

The usage, buckets, and user metrics are scraped in parallel on different goroutines. Given this fact, metrics may show up in a different interval from each other.
