package pkg

import (
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/stretchr/testify/require"
)

func TestCephRequest(t *testing.T) {
	client := makeHTTPClient()

	//rgwURL, err := url.Parse("http://s3.ct.activision.com")
	rgwURL, err := url.Parse("https://rgw.ct.activision.com")
	require.NoError(t, err)

	//creds := credentials.NewStaticCredentials("0I20MQBJE6RY4RBYD3Q1", "oKaKhtUIRHHTAyDPru4FIfoqJli38vVniqd2obax", "")
	creds := credentials.NewStaticCredentials("2K2ZBA8G6Y380C7099OQ", "BfHkwnqG9Ro6cKTaocnWV8dWmr7hYOkAjSY7Otyp", "")

	usageStats, err := getCephUsageStats(client, rgwURL, creds)
	require.NoError(t, err)
	require.NotNil(t, usageStats)

	bucketStats, err := getCephBucketStats(client, rgwURL, creds)
	require.NoError(t, err)
	require.NotNil(t, bucketStats)

	userQuotaStats, err := getCephUserQuotaStats(client, rgwURL, creds)
	require.NoError(t, err)
	require.NotNil(t, userQuotaStats)
}
