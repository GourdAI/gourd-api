package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type GitHubReleaseServiceSuite struct {
	suite.Suite
	srv    *httptest.Server
	client *githubReleaseClient
}

// testTransport redirects requests to the test server
type testTransport struct {
	testServerURL string
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Rewrite the URL to point to our test server
	testURL := t.testServerURL + req.URL.Path
	if req.URL.RawQuery != "" {
		testURL += "?" + req.URL.RawQuery
	}
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, testURL, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header
	return http.DefaultTransport.RoundTrip(newReq)
}

func newTestGitHubReleaseClient() *githubReleaseClient {
	return &githubReleaseClient{
		httpClient: &http.Client{},
	}
}

func TestGitHubReleaseClientAPIRequestAuthorization(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantAuth string
	}{
		{name: "exact HTTPS authority", url: "https://api.github.com/repos/test/repo", wantAuth: "Bearer update-secret"},
		{name: "HTTP", url: "http://api.github.com/repos/test/repo"},
		{name: "subdomain", url: "https://sub.api.github.com/repos/test/repo"},
		{name: "userinfo", url: "https://user@api.github.com/repos/test/repo"},
		{name: "explicit default port", url: "https://api.github.com:443/repos/test/repo"},
		{name: "custom port", url: "https://api.github.com:8443/repos/test/repo"},
		{name: "different host", url: "https://github.com/test/repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestGitHubReleaseClient()
			client.updateGitHubToken = "update-secret"
			req, err := client.newAPIRequest(context.Background(), tt.url)
			require.NoError(t, err)
			require.Equal(t, tt.wantAuth, req.Header.Get("Authorization"))
		})
	}

	client := newTestGitHubReleaseClient()
	req, err := client.newAPIRequest(context.Background(), "https://api.github.com/repos/test/repo")
	require.NoError(t, err)
	require.Empty(t, req.Header.Get("Authorization"))
}

func TestGitHubReleaseClientRedirectAuthorization(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantAuth string
	}{
		{name: "same HTTPS authority", url: "https://api.github.com/redirected", wantAuth: "Bearer update-secret"},
		{name: "HTTP", url: "http://api.github.com/redirected"},
		{name: "subdomain", url: "https://sub.api.github.com/redirected"},
		{name: "userinfo", url: "https://user@api.github.com/redirected"},
		{name: "custom port", url: "https://api.github.com:8443/redirected"},
		{name: "different host", url: "https://example.com/redirected"},
	}

	checkRedirect := githubAPICheckRedirect(nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tt.url, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer update-secret")

			require.NoError(t, checkRedirect(req, nil))
			require.Equal(t, tt.wantAuth, req.Header.Get("Authorization"))
		})
	}
}

func (s *GitHubReleaseServiceSuite) TearDownTest() {
	if s.srv != nil {
		s.srv.Close()
		s.srv = nil
	}
}

// newAPIOnlyClient 构造一个把 API 请求重定向到测试服务器的客户端。
func (s *GitHubReleaseServiceSuite) newAPIOnlyClient() *githubReleaseClient {
	return &githubReleaseClient{
		httpClient: &http.Client{
			Transport: &testTransport{testServerURL: s.srv.URL},
		},
	}
}

func (s *GitHubReleaseServiceSuite) TestFetchLatestRelease_Success() {
	releaseJSON := `{
		"tag_name": "v1.0.0",
		"name": "Release 1.0.0",
		"body": "Release notes",
		"html_url": "https://github.com/test/repo/releases/v1.0.0",
		"assets": [
			{
				"name": "app-linux-amd64.tar.gz",
				"browser_download_url": "https://github.com/test/repo/releases/download/v1.0.0/app-linux-amd64.tar.gz"
			}
		]
	}`

	s.srv = newLocalTestServer(s.T(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(s.T(), "/repos/test/repo/releases/latest", r.URL.Path)
		require.Equal(s.T(), "application/vnd.github.v3+json", r.Header.Get("Accept"))
		require.Equal(s.T(), "Sub2API-Updater", r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(releaseJSON))
	}))

	s.client = s.newAPIOnlyClient()

	release, err := s.client.FetchLatestRelease(context.Background(), "test/repo")
	require.NoError(s.T(), err)
	require.Equal(s.T(), "v1.0.0", release.TagName)
	require.Equal(s.T(), "Release 1.0.0", release.Name)
	require.Len(s.T(), release.Assets, 1)
	require.Equal(s.T(), "app-linux-amd64.tar.gz", release.Assets[0].Name)
}

func (s *GitHubReleaseServiceSuite) TestFetchRecentReleases_Success() {
	releasesJSON := `[
		{
			"tag_name": "v1.0.1",
			"name": "Release 1.0.1",
			"html_url": "https://github.com/test/repo/releases/v1.0.1",
			"published_at": "2026-07-08T00:00:00Z",
			"prerelease": false,
			"assets": [
				{
					"name": "app-linux-amd64.tar.gz",
					"browser_download_url": "https://github.com/test/repo/releases/download/v1.0.1/app-linux-amd64.tar.gz"
				}
			]
		},
		{
			"tag_name": "v1.0.1-rc1",
			"name": "Release 1.0.1-rc1",
			"prerelease": true
		},
		{
			"tag_name": "v1.0.0",
			"name": "Release 1.0.0"
		}
	]`

	s.srv = newLocalTestServer(s.T(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(s.T(), "/repos/test/repo/releases", r.URL.Path)
		require.Equal(s.T(), "15", r.URL.Query().Get("per_page"))
		require.Equal(s.T(), "application/vnd.github.v3+json", r.Header.Get("Accept"))
		require.Equal(s.T(), "Sub2API-Updater", r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(releasesJSON))
	}))

	s.client = s.newAPIOnlyClient()

	releases, err := s.client.FetchRecentReleases(context.Background(), "test/repo", 15)
	require.NoError(s.T(), err)
	require.Len(s.T(), releases, 3)
	require.Equal(s.T(), "v1.0.1", releases[0].TagName)
	require.False(s.T(), releases[0].Prerelease)
	require.Len(s.T(), releases[0].Assets, 1)
	require.True(s.T(), releases[1].Prerelease)
	require.Equal(s.T(), "v1.0.0", releases[2].TagName)
}

func (s *GitHubReleaseServiceSuite) TestFetchRecentReleases_Non200() {
	s.srv = newLocalTestServer(s.T(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))

	s.client = s.newAPIOnlyClient()

	_, err := s.client.FetchRecentReleases(context.Background(), "test/repo", 15)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "403")
}

func (s *GitHubReleaseServiceSuite) TestFetchLatestRelease_Non200() {
	s.srv = newLocalTestServer(s.T(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	s.client = s.newAPIOnlyClient()

	_, err := s.client.FetchLatestRelease(context.Background(), "test/repo")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "404")
}

func (s *GitHubReleaseServiceSuite) TestFetchLatestRelease_InvalidJSON() {
	s.srv = newLocalTestServer(s.T(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not valid json"))
	}))

	s.client = s.newAPIOnlyClient()

	_, err := s.client.FetchLatestRelease(context.Background(), "test/repo")
	require.Error(s.T(), err)
}

func (s *GitHubReleaseServiceSuite) TestFetchLatestRelease_ContextCancel() {
	s.srv = newLocalTestServer(s.T(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))

	s.client = s.newAPIOnlyClient()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.client.FetchLatestRelease(ctx, "test/repo")
	require.Error(s.T(), err)
}

func TestGitHubReleaseServiceSuite(t *testing.T) {
	suite.Run(t, new(GitHubReleaseServiceSuite))
}
