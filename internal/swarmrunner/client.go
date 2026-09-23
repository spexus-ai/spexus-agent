package swarmrunner

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type Client struct {
	http                  *http.Client
	base, token, instance string
}
type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("coordinator rejected request: %s (%d)", e.Code, e.Status)
}
func transient(err error) bool {
	var h *HTTPError
	if errors.As(err, &h) {
		return h.Status == 429 || h.Status == 503
	}
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return false
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func NewClient(c Config) (*Client, error) {
	ca, e := os.ReadFile(c.CAFile)
	if e != nil {
		return nil, e
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid CA")
	}
	token, e := os.ReadFile(c.CredentialFile)
	if e != nil {
		return nil, e
	}
	v := strings.TrimSpace(string(token))
	if v == "" || strings.ContainsAny(v, "\r\n") {
		return nil, errors.New("invalid credential file")
	}
	return &Client{http: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: c.CoordinatorURL + swarm.APIPrefix, token: v, instance: c.InstanceID}, nil
}
func (c *Client) once(ctx context.Context, method, path string, request any, response any) error {
	var b []byte
	var err error
	if request != nil {
		b, err = json.Marshal(request)
		if err != nil {
			return err
		}
	}
	timeout := 10 * time.Second
	if strings.HasPrefix(path, "/mailbox") {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("X-Agent-Instance-ID", c.instance)
	r.Header.Set("Content-Type", "application/json")
	result, err := c.http.Do(r)
	if err != nil {
		return err
	}
	defer result.Body.Close()
	body, err := io.ReadAll(io.LimitReader(result.Body, 4*1024*1024+1))
	if err != nil {
		return err
	}
	if len(body) > 4*1024*1024 {
		return errors.New("coordinator response too large")
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		var v struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &v)
		if v.Error.Code == "" {
			v.Error.Code = "http_error"
		}
		return &HTTPError{result.StatusCode, v.Error.Code}
	}
	if response != nil {
		return json.Unmarshal(body, response)
	}
	return nil
}
func (c *Client) call(ctx context.Context, method, path string, request, response any) error {
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, method, path, request, response)
		if err == nil || !transient(err) || ctx.Err() != nil {
			return err
		}
		delay := time.Second * time.Duration(1<<min(attempt, 5))
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		delay += time.Duration(rand.Int63n(int64(delay/5) + 1))
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (c *Client) publish(ctx context.Context, e swarm.Envelope) (swarm.Receipt, error) {
	var r swarm.Receipt
	err := c.call(ctx, "POST", "/messages", e, &r)
	return r, err
}
