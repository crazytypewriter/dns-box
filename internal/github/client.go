package github

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/google/go-github/v62/github"
	"golang.org/x/oauth2"
)

const defaultTimeout = 10 * time.Second

// Client is a wrapper around the GitHub API client.
type Client struct {
	client *github.Client
}

// NewClient creates a new GitHub client with timeout.
func NewClient(token string) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}

	var tc *http.Client
	if token != "" {
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: token},
		)
		tc = &http.Client{
			Timeout: defaultTimeout,
			Transport: &oauth2.Transport{
				Source: ts,
				Base:   transport,
			},
		}
	} else {
		tc = &http.Client{
			Timeout:   defaultTimeout,
			Transport: transport,
		}
	}
	return &Client{
		client: github.NewClient(tc),
	}
}

// SaveFile creates or updates a file in a GitHub repository.
func (c *Client) SaveFile(ctx context.Context, owner, repo, path, branch string, content []byte) error {
	log.Printf("[github] SaveFile START: %s/%s/%s (branch=%s)", owner, repo, path, branch)

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	log.Printf("[github] Calling GetContents...")
	opts := &github.RepositoryContentGetOptions{Ref: branch}
	fileContent, _, _, err := c.client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		log.Printf("[github] GetContents error: %v", err)
	} else {
		log.Printf("[github] GetContents success")
	}

	var fileSHA string
	if err == nil && fileContent != nil {
		fileSHA = *fileContent.SHA
		log.Printf("[github] File exists, SHA: %s", fileSHA)
	}

	if fileSHA != "" {
		log.Printf("[github] Calling UpdateFile...")
		_, _, err = c.client.Repositories.UpdateFile(ctx, owner, repo, path, &github.RepositoryContentFileOptions{
			Message: github.String("Update hosts_config.json"),
			Content: content,
			SHA:     github.String(fileSHA),
			Branch:  github.String(branch),
		})
	} else {
		log.Printf("[github] Calling CreateFile...")
		_, _, err = c.client.Repositories.CreateFile(ctx, owner, repo, path, &github.RepositoryContentFileOptions{
			Message: github.String("Create hosts_config.json"),
			Content: content,
			Branch:  github.String(branch),
		})
	}

	if err != nil {
		log.Printf("[github] SaveFile error: %v", err)
	} else {
		log.Printf("[github] SaveFile success")
	}

	return err
}

// LoadFile retrieves a file from a GitHub repository.
func (c *Client) LoadFile(ctx context.Context, owner, repo, path, branch string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	opts := &github.RepositoryContentGetOptions{Ref: branch}
	fileContent, _, _, err := c.client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return nil, err
	}
	if fileContent == nil {
		return nil, errors.New("file content is nil")
	}

	content, decodeErr := fileContent.GetContent()
	if decodeErr != nil {
		return nil, decodeErr
	}

	return []byte(content), nil
}
