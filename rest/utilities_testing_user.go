package rest

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/couchbase/sync_gateway/base"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func MakeUser(t *testing.T, httpClient *http.Client, serverURL, username, password string, roles []string) {
	form := url.Values{}
	form.Add("password", password)
	form.Add("roles", strings.Join(roles, ","))

	retryWorker := func() (shouldRetry bool, err error, value interface{}) {
		req, err := http.NewRequest("PUT", fmt.Sprintf("%s/settings/rbac/users/local/%s", serverURL, username), strings.NewReader(form.Encode()))
		require.NoError(t, err)

		req.SetBasicAuth(base.TestClusterUsername(), base.TestClusterPassword())
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

		resp, err := httpClient.Do(req)
		if err != nil {
			return true, err, resp
		}
		return false, err, resp
	}

	err, resp := base.RetryLoop("Admin Auth testing MakeUser", retryWorker, base.CreateSleeperFunc(10, 100))
	require.NoError(t, err)

	if resp.(*http.Response).StatusCode != http.StatusOK {
		bodyResp, err := ioutil.ReadAll(resp.(*http.Response).Body)
		assert.NoError(t, err)
		fmt.Println(string(bodyResp))
	}
	require.Equal(t, http.StatusOK, resp.(*http.Response).StatusCode)

	require.NoError(t, resp.(*http.Response).Body.Close(), "Error closing response body")
}

func DeleteUser(t *testing.T, httpClient *http.Client, serverURL, username string) {
	retryWorker := func() (shouldRetry bool, err error, value interface{}) {
		req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/settings/rbac/users/local/%s", serverURL, username), nil)
		require.NoError(t, err)

		req.SetBasicAuth(base.TestClusterUsername(), base.TestClusterPassword())

		resp, err := httpClient.Do(req)
		if err != nil {
			return true, err, resp
		}
		return false, err, resp
	}

	err, resp := base.RetryLoop("Admin Auth testing DeleteUser", retryWorker, base.CreateSleeperFunc(10, 100))
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.(*http.Response).StatusCode)

	require.NoError(t, resp.(*http.Response).Body.Close(), "Error closing response body")
}
