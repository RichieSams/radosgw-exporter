package pkg

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
)

type usageResponse struct {
	Entries []usageEntry `json:"entries"`
}

type usageEntry struct {
	User    string             `json:"user"`
	Buckets []bucketUsageEntry `json:"buckets"`
}

type bucketUsageEntry struct {
	ID         string               `json:"bucket"`
	Owner      string               `json:"owner"`
	Categories []usageCategoryEntry `json:"categories"`
}

type usageCategoryEntry struct {
	Name          string `json:"category"`
	BytesSent     int64  `json:"bytes_sent"`
	BytesReceived int64  `json:"bytes_received"`
	Ops           int64  `json:"ops"`
	SuccessfulOps int64  `json:"successful_ops"`
}

func getCephUsageStats(client *http.Client, rgwURL *url.URL, creds *credentials.Credentials) (*usageResponse, error) {
	destURL, err := rgwURL.Parse("admin/usage")
	if err != nil {
		return nil, fmt.Errorf("failed to construct admin URL from ceph URL - %w", err)
	}

	queryParams := destURL.Query()
	queryParams.Add("format", "json")
	queryParams.Add("show-entries", "True")
	queryParams.Add("show-summary", "False")
	destURL.RawQuery = queryParams.Encode()

	resp, err := queryCephAdminAPI(client, destURL, creds)
	if err != nil {
		return nil, fmt.Errorf("failed to get usage stats from ceph - %w", err)
	}

	usage := &usageResponse{}
	if err := json.Unmarshal(resp, usage); err != nil {
		return nil, fmt.Errorf("failed to unmarshall ceph usage response - %w", err)
	}

	return usage, nil
}

type bucketInfoEntry struct {
	Name      string                          `json:"bucket"`
	Owner     string                          `json:"owner"`
	ZoneGroup string                          `json:"zonegroup"`
	Usage     map[string]bucketInfoUsageEntry `json:"usage"`
	Quota     bucketQuotaEntry                `json:"bucket_quota"`
}

type bucketInfoUsageEntry struct {
	Size         uint64 `json:"size_actual"`
	UtilizedSize uint64 `json:"size_utilized"`
	NumObjects   uint64 `json:"num_objects"`
}

type bucketQuotaEntry struct {
	Enabled    bool  `json:"enabled"`
	MaxSize    int64 `json:"max_size"`
	MaxObjects int64 `json:"max_objects"`
}

func getCephBucketStats(client *http.Client, rgwURL *url.URL, creds *credentials.Credentials) ([]bucketInfoEntry, error) {
	destURL, err := rgwURL.Parse("admin/bucket")
	if err != nil {
		return nil, fmt.Errorf("failed to construct admin URL from ceph URL - %w", err)
	}

	queryParams := destURL.Query()
	queryParams.Add("format", "json")
	queryParams.Add("stats", "True")
	destURL.RawQuery = queryParams.Encode()

	resp, err := queryCephAdminAPI(client, destURL, creds)
	if err != nil {
		return nil, fmt.Errorf("failed to get bucket stats from ceph - %w", err)
	}

	bucketStats := []bucketInfoEntry{}
	if err := json.Unmarshal(resp, &bucketStats); err != nil {
		return nil, fmt.Errorf("failed to unmarshall ceph bucket response - %w", err)
	}

	return bucketStats, nil
}

type userStats struct {
	Enabled    bool  `json:"enabled"`
	MaxSize    int64 `json:"max_size"`
	MaxObjects int64 `json:"max_objects"`
}

func getCephUserQuotaStats(client *http.Client, rgwURL *url.URL, creds *credentials.Credentials) (map[string]userStats, error) {
	users, err := getUserList(client, rgwURL, creds)
	if err != nil {
		return nil, err
	}

	statsMap := map[string]userStats{}
	for _, user := range users {
		destURL, err := rgwURL.Parse("admin/user")
		if err != nil {
			return nil, fmt.Errorf("failed to construct admin URL from ceph URL - %w", err)
		}

		queryParams := destURL.Query()
		queryParams.Add("format", "json")
		queryParams.Add("quota", "")
		queryParams.Add("uid", user)
		queryParams.Add("quota-type", "user")
		destURL.RawQuery = queryParams.Encode()

		resp, err := queryCephAdminAPI(client, destURL, creds)
		if err != nil {
			return nil, fmt.Errorf("failed to get user stats from ceph - %w", err)
		}

		stats := userStats{}
		if err := json.Unmarshal(resp, &stats); err != nil {
			return nil, fmt.Errorf("failed to unmarshall ceph user stats response - %w", err)
		}

		statsMap[user] = stats
	}

	return statsMap, nil
}

type userListResponse struct {
	Keys []string `json:"keys"`
}

func getUserList(client *http.Client, rgwURL *url.URL, creds *credentials.Credentials) ([]string, error) {
	destURL, err := rgwURL.Parse("admin/user")
	if err != nil {
		return nil, fmt.Errorf("failed to construct admin URL from ceph URL - %w", err)
	}

	queryParams := destURL.Query()
	queryParams.Add("format", "json")
	queryParams.Add("list", "")
	destURL.RawQuery = queryParams.Encode()

	resp, err := queryCephAdminAPI(client, destURL, creds)
	if err != nil {
		return nil, fmt.Errorf("failed to get user list from ceph - %w", err)
	}

	userList := &userListResponse{}
	if err := json.Unmarshal(resp, userList); err != nil {
		return nil, fmt.Errorf("failed to unmarshall ceph user list response - %w", err)
	}

	return userList.Keys, nil
}

func queryCephAdminAPI(client *http.Client, destURL *url.URL, creds *credentials.Credentials) ([]byte, error) {
	signer := v4.NewSigner(creds)

	req, err := http.NewRequest("GET", destURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request - %w", err)
	}

	_, err = signer.Sign(req, nil, "s3", "us-east-1", time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to sign request - %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to do request - %w", err)
	}

	respBody, err := io.ReadAll(resp.Body)

	closeErr := resp.Body.Close()

	if err != nil {
		return nil, fmt.Errorf("failed to read response body - %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("failed to close response body - %w", closeErr)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s - Body: %s", resp.Status, respBody)
	}

	return respBody, nil
}
