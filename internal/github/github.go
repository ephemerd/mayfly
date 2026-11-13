package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// Client is a minimal GitHub REST client for webhook reconciliation, backed
// by an installation access token.
type Client struct {
	token string
	http  *http.Client
}

func New(token string) *Client {
	return &Client{token: token, http: http.DefaultClient}
}

type Repo struct {
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	Name string `json:"name"`
}

type Hook struct {
	ID     int64    `json:"id"`
	Events []string `json:"events"`
	Active bool     `json:"active"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
	LastResponse struct {
		Status string `json:"status"`
	} `json:"last_response"`
}

func (c *Client) do(ctx context.Context, method, url string, out any) error {
	return c.doBody(ctx, method, url, nil, out)
}

func (c *Client) doBody(ctx context.Context, method, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, url, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ListOrgRepos returns every repo in the org (paginated).
func (c *Client) ListOrgRepos(ctx context.Context, org string) ([]Repo, error) {
	var all []Repo
	for page := 1; ; page++ {
		var batch []Repo
		url := fmt.Sprintf("https://api.github.com/orgs/%s/repos?per_page=100&page=%d", org, page)
		if err := c.do(ctx, http.MethodGet, url, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

// ListRepoHooks returns the webhooks configured on a repo.
func (c *Client) ListRepoHooks(ctx context.Context, owner, repo string) ([]Hook, error) {
	var hooks []Hook
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/hooks?per_page=100", owner, repo)
	if err := c.do(ctx, http.MethodGet, url, &hooks); err != nil {
		return nil, err
	}
	return hooks, nil
}

// DeleteRepoHook removes a webhook by id.
func (c *Client) DeleteRepoHook(ctx context.Context, owner, repo string, id int64) error {
	url := "https://api.github.com/repos/" + owner + "/" + repo + "/hooks/" + strconv.FormatInt(id, 10)
	return c.do(ctx, http.MethodDelete, url, nil)
}

// SetRepoHookActive flips a webhook's active flag without touching its config.
// Deactivating is mayfly's cordon: GitHub stops delivering events, so the
// pool's ephemerd claims no new jobs while a drain waits out the running ones.
func (c *Client) SetRepoHookActive(ctx context.Context, owner, repo string, id int64, active bool) error {
	url := "https://api.github.com/repos/" + owner + "/" + repo + "/hooks/" + strconv.FormatInt(id, 10)
	return c.doBody(ctx, http.MethodPatch, url, map[string]bool{"active": active}, nil)
}

// HasEvent reports whether the hook subscribes to an event.
func (h Hook) HasEvent(event string) bool {
	for _, e := range h.Events {
		if e == event {
			return true
		}
	}
	return false
}
