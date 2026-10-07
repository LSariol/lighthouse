package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

// Client calls the daemon through its control socket. It implements Service.
type Client struct {
	socket string
	hc     *http.Client
}

// NewClient returns a Client for the daemon listening at socket. It doesn't
// connect until the first call.
func NewClient(socket string) *Client {
	return &Client{
		socket: socket,
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
			// No timeout: deploys take minutes. Callers use ctx instead.
		},
	}
}

// ErrUnreachable means the daemon isn't listening on the socket.
var ErrUnreachable = errors.New("daemon unreachable")

type unreachableError struct {
	socket string
	err    error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("Can't reach the Lighthouse daemon at %s (%v). Is it running? In Docker: \"docker ps\" should list lighthouse; start it with \"docker compose up -d\".", e.socket, e.err)
}

func (e *unreachableError) Is(target error) bool { return target == ErrUnreachable }

// call sends a request and decodes the response's data into out (if not nil).
func (c *Client) call(ctx context.Context, method string, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	// The host is ignored: the transport always dials the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://lighthouse"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &unreachableError{socket: c.socket, err: unwrapDial(err)}
	}
	defer resp.Body.Close()

	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("unexpected answer from the daemon (status %d): %v", resp.StatusCode, err)
	}
	if env.Error != nil {
		return env.Error
	}
	if out != nil && env.Data != nil {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// unwrapDial strips the URL wrapping from a dial error, leaving the reason
// ("connect: no such file or directory").
func unwrapDial(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		var opErr *net.OpError
		if errors.As(urlErr.Err, &opErr) && opErr.Err != nil {
			return opErr.Err
		}
		return urlErr.Err
	}
	return err
}

func project(name string) string { return "/v1/projects/" + url.PathEscape(name) }

func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.call(ctx, http.MethodGet, "/v1/status", nil, &s)
	return s, err
}

func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var p []Project
	err := c.call(ctx, http.MethodGet, "/v1/projects", nil, &p)
	return p, err
}

func (c *Client) Add(ctx context.Context, name string, repoURL string) (Project, error) {
	var p Project
	err := c.call(ctx, http.MethodPost, "/v1/projects", map[string]string{"name": name, "url": repoURL}, &p)
	return p, err
}

func (c *Client) Remove(ctx context.Context, name string, down bool) error {
	return c.call(ctx, http.MethodDelete, project(name)+"?down="+strconv.FormatBool(down), nil, nil)
}

func (c *Client) Retry(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPost, project(name)+"/retry", nil, nil)
}

func (c *Client) Check(ctx context.Context, name string) (Deployment, error) {
	var d Deployment
	err := c.call(ctx, http.MethodPost, project(name)+"/check", nil, &d)
	return d, err
}

func (c *Client) Report(ctx context.Context, name string, n int) (Deployment, error) {
	var d Deployment
	err := c.call(ctx, http.MethodGet, project(name)+"/report?n="+strconv.Itoa(n), nil, &d)
	return d, err
}

func (c *Client) Rename(ctx context.Context, name string, newName string) error {
	return c.call(ctx, http.MethodPost, project(name)+"/rename", map[string]string{"name": newName}, nil)
}

func (c *Client) SetURL(ctx context.Context, name string, repoURL string) error {
	return c.call(ctx, http.MethodPost, project(name)+"/url", map[string]string{"url": repoURL}, nil)
}

func (c *Client) Deploy(ctx context.Context, name string, ref string) error {
	return c.call(ctx, http.MethodPost, project(name)+"/deploy", map[string]string{"ref": ref}, nil)
}

func (c *Client) Rollback(ctx context.Context, name string) (string, error) {
	var to string
	err := c.call(ctx, http.MethodPost, project(name)+"/rollback", nil, &to)
	return to, err
}

func (c *Client) Start(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPost, project(name)+"/start", nil, nil)
}

func (c *Client) Stop(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPost, project(name)+"/stop", nil, nil)
}

func (c *Client) Restart(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPost, project(name)+"/restart", nil, nil)
}

func (c *Client) Logs(ctx context.Context, name string, lines int) (string, error) {
	var logs string
	err := c.call(ctx, http.MethodGet, project(name)+"/logs?lines="+strconv.Itoa(lines), nil, &logs)
	return logs, err
}

func (c *Client) History(ctx context.Context, name string, limit int) ([]Deployment, error) {
	var history []Deployment
	err := c.call(ctx, http.MethodGet, project(name)+"/history?limit="+strconv.Itoa(limit), nil, &history)
	return history, err
}

func (c *Client) Scan(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/scan", nil, nil)
}

func (c *Client) Pause(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/pause", nil, nil)
}

func (c *Client) Resume(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/resume", nil, nil)
}
