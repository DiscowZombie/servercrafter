// Package fetch holds the plumbing every source shares: an HTTP client that retries and never leaks
// credentials across redirects, atomic downloads verified against checksums and safe extraction.
package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Auth authenticates a request
type Auth interface {
	Apply(r *http.Request) error
}

// BasicAuth is HTTP basic authentication
type BasicAuth struct{ Username, Password string }

func (a BasicAuth) Apply(r *http.Request) error {
	r.SetBasicAuth(a.Username, a.Password)
	return nil
}

// HeaderAuth sends a secret in a header (PRIVATE-TOKEN, Authorization: Bearer …)
type HeaderAuth struct{ Name, Value string }

func (a HeaderAuth) Apply(r *http.Request) error {
	r.Header.Set(a.Name, a.Value)
	return nil
}

// StatusError is a non-2xx response
type StatusError struct {
	URL    string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// IsNotFound reports whether err is a 404 response
func IsNotFound(err error) bool {
	var status *StatusError
	return errors.As(err, &status) && status.Status == http.StatusNotFound
}

// Client performs GET requests with retries on transient failures
type Client struct {
	HTTP      *http.Client
	UserAgent string
	Retries   int
	Backoff   time.Duration
	Log       logrus.FieldLogger
}

type authHeaderKey struct{}

// NewClient returns a client identifying itself with userAgent
func NewClient(userAgent string, log logrus.FieldLogger) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	return &Client{
		HTTP: &http.Client{
			Transport: transport,
			// Credentials only go to the host they are configured for: Go already drops
			// Authorization on a cross-host redirect, custom token headers have to be dropped too
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				if req.URL.Host != via[0].URL.Host {
					req.Header.Del("Authorization")
					if names, ok := via[0].Context().Value(authHeaderKey{}).([]string); ok {
						for _, name := range names {
							req.Header.Del(name)
						}
					}
					for name := range req.Header {
						if strings.HasPrefix(strings.ToLower(name), "x-amz-") {
							req.Header.Del(name)
						}
					}
				}
				return nil
			},
		},
		UserAgent: userAgent,
		Retries:   3,
		Backoff:   time.Second,
		Log:       log,
	}
}

// Get sends a GET request and returns the response when its status is 2xx. The caller closes the
// body. Network errors, 429 and 5xx are retried.
func (c *Client) Get(ctx context.Context, url string, auth Auth, headers map[string]string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			wait := c.Backoff * time.Duration(1<<(attempt-1))
			c.Log.WithError(lastErr).Debugf("Retrying %s in %s", url, wait)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		response, err := c.do(ctx, url, auth, headers)
		if err == nil {
			return response, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		var status *StatusError
		if errors.As(err, &status) && status.Status != http.StatusTooManyRequests && status.Status < 500 {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) do(ctx context.Context, url string, auth Auth, headers map[string]string) (*http.Response, error) {
	if header, ok := auth.(HeaderAuth); ok {
		ctx = context.WithValue(ctx, authHeaderKey{}, []string{header.Name})
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.UserAgent != "" {
		request.Header.Set("User-Agent", c.UserAgent)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if auth != nil {
		if err = auth.Apply(request); err != nil {
			return nil, err
		}
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, &StatusError{URL: url, Status: response.StatusCode, Body: summarize(body)}
	}
	return response, nil
}

// GetJSON decodes the JSON response of url into out
func (c *Client) GetJSON(ctx context.Context, url string, auth Auth, out any) error {
	response, err := c.Get(ctx, url, auth, map[string]string{"Accept": "application/json"})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err = json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: invalid JSON response: %w", url, err)
	}
	return nil
}

// GetBytes returns the body of url, read up to limit bytes
func (c *Client) GetBytes(ctx context.Context, url string, auth Auth, limit int64) ([]byte, error) {
	response, err := c.Get(ctx, url, auth, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(io.LimitReader(response.Body, limit))
}

// Opener opens the content of an artifact
type Opener func(ctx context.Context) (io.ReadCloser, error)

// HTTPOpener opens url with the given authentication and headers
func (c *Client) HTTPOpener(url string, auth Auth, headers map[string]string) Opener {
	return func(ctx context.Context) (io.ReadCloser, error) {
		response, err := c.Get(ctx, url, auth, headers)
		if err != nil {
			return nil, err
		}
		return response.Body, nil
	}
}

func summarize(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
