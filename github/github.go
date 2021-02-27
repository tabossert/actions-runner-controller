package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-github/v33/github"
	"github.com/summerwind/actions-runner-controller/github/metrics"
	"golang.org/x/oauth2"
)

// Config contains configuration for Github client
type Config struct {
	EnterpriseURL     string `split_words:"true"`
	AppID             int64  `split_words:"true"`
	AppInstallationID int64  `split_words:"true"`
	AppPrivateKey     string `split_words:"true"`
	Token             string
	SecondaryToken    string `split_words:"true"`
}

// Client wraps GitHub client with some additional
type Client struct {
	*github.Client
	regTokens map[string]*github.RegistrationToken
	mu        sync.Mutex
	// GithubBaseURL to Github without API suffix.
	GithubBaseURL string
}

// NewClient creates a Github Client
func (c *Config) NewClient(whichToken string) (*Client, error) {
	var transport http.RoundTripper
	if len(c.Token) > 0 && whichToken == "primary" {
		transport = oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.Token})).Transport
	} else if len(c.SecondaryToken) > 0 && whichToken == "secondary" {
		transport = oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.SecondaryToken})).Transport
	} else {
		tr, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, c.AppID, c.AppInstallationID, c.AppPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("authentication failed: %v", err)
		}
		if len(c.EnterpriseURL) > 0 {
			githubAPIURL, err := getEnterpriseApiUrl(c.EnterpriseURL)
			if err != nil {
				return nil, fmt.Errorf("enterprise url incorrect: %v", err)
			}
			tr.BaseURL = githubAPIURL
		}
		transport = tr
	}
	transport = metrics.Transport{Transport: transport}
	httpClient := &http.Client{Transport: transport}

	var client *github.Client
	var githubBaseURL string
	if len(c.EnterpriseURL) > 0 {
		var err error
		client, err = github.NewEnterpriseClient(c.EnterpriseURL, c.EnterpriseURL, httpClient)
		if err != nil {
			return nil, fmt.Errorf("enterprise client creation failed: %v", err)
		}
		githubBaseURL = fmt.Sprintf("%s://%s%s", client.BaseURL.Scheme, client.BaseURL.Host, strings.TrimSuffix(client.BaseURL.Path, "api/v3/"))
	} else {
		client = github.NewClient(httpClient)
		githubBaseURL = "https://github.com/"
	}

	return &Client{
		Client:        client,
		regTokens:     map[string]*github.RegistrationToken{},
		mu:            sync.Mutex{},
		GithubBaseURL: githubBaseURL,
	}, nil
}

// GetRegistrationToken returns a registration token tied with the name of repository and runner.
func (c *Client) GetRegistrationToken(ctx context.Context, enterprise, org, repo, name string) (*github.RegistrationToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := getRegistrationKey(org, repo, enterprise)
	rt, ok := c.regTokens[key]

	// we like to give runners a chance that are just starting up and may miss the expiration date by a bit
	runnerStartupTimeout := 3 * time.Minute

	if ok && rt.GetExpiresAt().After(time.Now().Add(runnerStartupTimeout)) {
		return rt, nil
	}

	enterprise, owner, repo, err := getEnterpriseOrganisationAndRepo(enterprise, org, repo)

	if err != nil {
		return rt, err
	}

	rt, res, err := c.createRegistrationToken(ctx, enterprise, owner, repo)

	if err != nil {
		return nil, fmt.Errorf("failed to create registration token: %v", err)
	}

	if res.StatusCode != 201 {
		return nil, fmt.Errorf("unexpected status: %d", res.StatusCode)
	}

	c.regTokens[key] = rt
	go func() {
		c.cleanup()
	}()

	return rt, nil
}

// RemoveRunner removes a runner with specified runner ID from repository.
func (c *Client) RemoveRunner(ctx context.Context, enterprise, org, repo string, runnerID int64) error {
	enterprise, owner, repo, err := getEnterpriseOrganisationAndRepo(enterprise, org, repo)

	if err != nil {
		return err
	}

	res, err := c.removeRunner(ctx, enterprise, owner, repo, runnerID)

	if err != nil {
		return fmt.Errorf("failed to remove runner: %w", err)
	}

	if res.StatusCode != 204 {
		return fmt.Errorf("unexpected status: %d", res.StatusCode)
	}

	return nil
}

// ListRunners returns a list of runners of specified owner/repository name.
func (c *Client) ListRunners(ctx context.Context, enterprise, org, repo string) ([]*github.Runner, error) {
	enterprise, owner, repo, err := getEnterpriseOrganisationAndRepo(enterprise, org, repo)

	if err != nil {
		return nil, err
	}

	var runners []*github.Runner

	opts := github.ListOptions{PerPage: 100}
	for {
		list, res, err := c.listRunners(ctx, enterprise, owner, repo, &opts)

		if err != nil {
			return runners, fmt.Errorf("failed to list runners: %w", err)
		}

		runners = append(runners, list.Runners...)
		if res.NextPage == 0 {
			break
		}
		opts.Page = res.NextPage
	}

	return runners, nil
}

// cleanup removes expired registration tokens.
func (c *Client) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, rt := range c.regTokens {
		if rt.GetExpiresAt().Before(time.Now()) {
			delete(c.regTokens, key)
		}
	}
}

// wrappers for github functions (switch between enterprise/organization/repository mode)
// so the calling functions don't need to switch and their code is a bit cleaner

func (c *Client) createRegistrationToken(ctx context.Context, enterprise, org, repo string) (*github.RegistrationToken, *github.Response, error) {
	if len(repo) > 0 {
		return c.Client.Actions.CreateRegistrationToken(ctx, org, repo)
	}
	if len(org) > 0 {
		return c.Client.Actions.CreateOrganizationRegistrationToken(ctx, org)
	}
	return c.Client.Enterprise.CreateRegistrationToken(ctx, enterprise)
}

func (c *Client) removeRunner(ctx context.Context, enterprise, org, repo string, runnerID int64) (*github.Response, error) {
	if len(repo) > 0 {
		return c.Client.Actions.RemoveRunner(ctx, org, repo, runnerID)
	}
	if len(org) > 0 {
		return c.Client.Actions.RemoveOrganizationRunner(ctx, org, runnerID)
	}
	return c.Client.Enterprise.RemoveRunner(ctx, enterprise, runnerID)
}

func (c *Client) listRunners(ctx context.Context, enterprise, org, repo string, opts *github.ListOptions) (*github.Runners, *github.Response, error) {
	if len(repo) > 0 {
		return c.Client.Actions.ListRunners(ctx, org, repo, opts)
	}
	if len(org) > 0 {
		return c.Client.Actions.ListOrganizationRunners(ctx, org, opts)
	}
	return c.Client.Enterprise.ListRunners(ctx, enterprise, opts)
}

func (c *Client) ListRepositoryWorkflowRuns(ctx context.Context, user string, repoName string) ([]*github.WorkflowRun, error) {
	c.Client.Actions.ListRepositoryWorkflowRuns(ctx, user, repoName, nil)

	queued, err := c.listRepositoryWorkflowRuns(ctx, user, repoName, "queued")
	if err != nil {
		return nil, fmt.Errorf("listing queued workflow runs: %w", err)
	}

	inProgress, err := c.listRepositoryWorkflowRuns(ctx, user, repoName, "in_progress")
	if err != nil {
		return nil, fmt.Errorf("listing in_progress workflow runs: %w", err)
	}

	var workflowRuns []*github.WorkflowRun

	workflowRuns = append(workflowRuns, queued...)
	workflowRuns = append(workflowRuns, inProgress...)

	return workflowRuns, nil
}

func (c *Client) listRepositoryWorkflowRuns(ctx context.Context, user string, repoName, status string) ([]*github.WorkflowRun, error) {
	c.Client.Actions.ListRepositoryWorkflowRuns(ctx, user, repoName, nil)

	var workflowRuns []*github.WorkflowRun

	opts := github.ListWorkflowRunsOptions{
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
		Status: status,
	}

	for {
		list, res, err := c.Client.Actions.ListRepositoryWorkflowRuns(ctx, user, repoName, &opts)

		if err != nil {
			return workflowRuns, fmt.Errorf("failed to list workflow runs: %v", err)
		}

		workflowRuns = append(workflowRuns, list.WorkflowRuns...)
		if res.NextPage == 0 {
			break
		}
		opts.Page = res.NextPage
	}

	return workflowRuns, nil
}

// Validates enterprise, organisation and repo arguments. Both are optional, but at least one should be specified
func getEnterpriseOrganisationAndRepo(enterprise, org, repo string) (string, string, string, error) {
	if len(repo) > 0 {
		owner, repository, err := splitOwnerAndRepo(repo)
		return "", owner, repository, err
	}
	if len(org) > 0 {
		return "", org, "", nil
	}
	if len(enterprise) > 0 {
		return enterprise, "", "", nil
	}
	return "", "", "", fmt.Errorf("enterprise, organization and repository are all empty")
}

func getRegistrationKey(org, repo, enterprise string) string {
	return fmt.Sprintf("org=%s,repo=%s,enterprise=%s", org, repo, enterprise)
}

func splitOwnerAndRepo(repo string) (string, string, error) {
	chunk := strings.Split(repo, "/")
	if len(chunk) != 2 {
		return "", "", fmt.Errorf("invalid repository name: '%s'", repo)
	}
	return chunk[0], chunk[1], nil
}

func getEnterpriseApiUrl(baseURL string) (string, error) {
	baseEndpoint, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(baseEndpoint.Path, "/") {
		baseEndpoint.Path += "/"
	}
	if !strings.HasSuffix(baseEndpoint.Path, "/api/v3/") &&
		!strings.HasPrefix(baseEndpoint.Host, "api.") &&
		!strings.Contains(baseEndpoint.Host, ".api.") {
		baseEndpoint.Path += "api/v3/"
	}

	// Trim trailing slash, otherwise there's double slash added to token endpoint
	return fmt.Sprintf("%s://%s%s", baseEndpoint.Scheme, baseEndpoint.Host, strings.TrimSuffix(baseEndpoint.Path, "/")), nil
}

type RunnerNotFound struct {
	runnerName string
}

func (e *RunnerNotFound) Error() string {
	return fmt.Sprintf("runner %q not found", e.runnerName)
}

type RunnerOffline struct {
	runnerName string
}

func (e *RunnerOffline) Error() string {
	return fmt.Sprintf("runner %q offline", e.runnerName)
}

func (r *Client) IsRunnerBusy(ctx context.Context, enterprise, org, repo, name string) (bool, error) {
	runners, err := r.ListRunners(ctx, enterprise, org, repo)
	if err != nil {
		return false, err
	}

	for _, runner := range runners {
		if runner.GetName() == name {
			if runner.GetStatus() == "offline" {
				return false, &RunnerOffline{runnerName: name}
			}
			return runner.GetBusy(), nil
		}
	}

	return false, &RunnerNotFound{runnerName: name}
}
