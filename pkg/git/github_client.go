package git

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/url"

	"github.com/google/go-github/v42/github"
	"golang.org/x/oauth2"
)

type GithubClient struct {
	BaseUrl            string
	RepoOwner          string
	RepoName           string
	Revision           string
	AccessToken        string
	InsecureSkipVerify bool
}

func NewGithubClient(baseUrl string, repoOwner string, repoName string, revision string, accessToken string, insecureSkipVerify bool) *GithubClient {
	return &GithubClient{
		BaseUrl:            baseUrl,
		RepoOwner:          repoOwner,
		RepoName:           repoName,
		Revision:           revision,
		AccessToken:        accessToken,
		InsecureSkipVerify: insecureSkipVerify,
	}
}

func (githubClient *GithubClient) SetStatus(ghState, ghDescription, ghContext, ghTargetUrl string) (error, bool) {
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: githubClient.InsecureSkipVerify,
			},
		},
	}

	// create the status
	status := &github.RepoStatus{
		State:       github.String(ghState),
		Description: github.String(ghDescription),
		Context:     github.String(ghContext),
		TargetURL:   github.String(ghTargetUrl),
	}

	// create the client
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubClient.AccessToken})
	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	tc := oauth2.NewClient(ctx, ts)

	parsedUrl, err := url.Parse(githubClient.BaseUrl)
	if err != nil {
		return err, false
	}

	if parsedUrl.Host == "github.com" {
		client := github.NewClient(tc)
		_, _, err := client.Repositories.CreateStatus(ctx, githubClient.RepoOwner, githubClient.RepoName, githubClient.Revision, status)
		if err != nil {
			return err, false
		}
	} else {
		client, errClient := github.NewEnterpriseClient(githubClient.BaseUrl, "api/v3", tc)
		if errClient != nil {
			return errClient, false
		}
		_, _, err := client.Repositories.CreateStatus(ctx, githubClient.RepoOwner, githubClient.RepoName, githubClient.Revision, status)
		if err != nil {
			return err, false
		}
	}

	return nil, true
}
