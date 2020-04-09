package barquesubmit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/evergreen-ci/gimlet"
	"github.com/evergreen-ci/utility"
	"github.com/mongodb/amboy"
	"github.com/mongodb/curator/repobuilder"
	"github.com/pkg/errors"
)

const (
	barqueAPIKeyHeader  = "Api-Key"
	barqueAPIUserHeader = "Api-User"
)

type Client struct {
	baseURL  string
	username string
	apiKey   string
}

func New(baseURL string) (*Client, error) {
	if !strings.HasPrefix(baseURL, "http") {
		return nil, errors.New("malformed url")
	}

	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	if !strings.HasSuffix(baseURL, "/rest/v1") {
		baseURL += "rest/v1"
	}

	return &Client{
		baseURL: baseURL,
	}, nil
}

func (c *Client) getURL(p string) string {
	if strings.HasPrefix(p, c.baseURL) {
		return p
	}

	return strings.Join([]string{c.baseURL, p}, "/")
}

func (c *Client) makeRequest(ctx context.Context, url, method string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.getURL(url), body)
	if err != nil {
		return nil, errors.Wrap(err, "problem with request")
	}
	req = req.WithContext(ctx)

	if c.apiKey == "" {
		return req, nil
	}

	if c.username != "" {
		req.Header[barqueAPIUserHeader] = []string{c.username}
	}
	if c.apiKey != "" {
		req.Header[barqueAPIKeyHeader] = []string{c.apiKey}
	}

	return req, nil
}

type userCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userAPIKeyResponse struct {
	Username string `json:"username"`
	Key      string `json:"key"`
}

func (c *Client) Login(ctx context.Context, username, password string) error {
	fmt.Printf(">>> calling client login with username=%s and password=%s\n", username, password)
	client := utility.GetDefaultHTTPRetryableClient()
	defer utility.PutHTTPClient(client)

	payload, err := json.Marshal(&userCredentials{Username: username, Password: password})
	if err != nil {
		return errors.Wrap(err, "problem marshaling login payload")
	}

	fmt.Printf(">>> making request with payload: %s\n", string(payload))
	req, err := c.makeRequest(ctx, "admin/login", http.MethodPost, bytes.NewBuffer(payload))
	if err != nil {
		return errors.Wrap(err, "problem building login request")
	}
	fmt.Printf(">>> constructed req: %+v\n", req)

	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "problem making login request")
	}
	fmt.Printf(">>> got res: %+v\n", resp)

	if resp.StatusCode != http.StatusOK {
		return c.handleError(resp.StatusCode, resp.Body)
	}

	data := &userAPIKeyResponse{}
	if err = gimlet.GetJSON(resp.Body, data); err != nil {
		return errors.Wrap(err, "problem reading body of login response")
	}

	if data.Username != username {
		return errors.Errorf("service returned logically inconsistent credentials")
	}

	c.apiKey = data.Key
	c.username = data.Username
	return nil
}

func (c *Client) SetCredentials(username, key string) {
	c.username = username
	c.apiKey = key
}

func (c *Client) SubmitJob(ctx context.Context, opts repobuilder.JobOptions) (string, error) {
	client := utility.GetDefaultHTTPRetryableClient()
	defer utility.PutHTTPClient(client)

	payload, err := json.Marshal(opts)
	if err != nil {
		return "", errors.Wrap(err, "problem marshaling json")
	}

	req, err := c.makeRequest(ctx, "repobuilder", http.MethodPost, bytes.NewBuffer(payload))
	if err != nil {
		return "", errors.Wrap(err, "problem building job request")
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "problem making job submission request")
	}

	if resp.StatusCode != http.StatusOK {
		return "", c.handleError(resp.StatusCode, resp.Body)
	}

	out := struct {
		ID     string   `json:"id"`
		Scopes []string `json:"scopes"`
	}{}

	if err = gimlet.GetJSON(resp.Body, &out); err != nil {
		return "", errors.Wrap(err, "problem reading body of login response")
	}

	return out.ID, nil
}

type JobStatus struct {
	ID          string              `json:"id"`
	Status      amboy.JobStatusInfo `json:"status"`
	Timing      amboy.JobTimeInfo   `json:"timing"`
	QueueStatus amboy.QueueStats    `json:"queue_status"`
	HasErrors   bool                `json:"has_errors"`
	Error       string              `json:"error"`
}

func (c *Client) handleError(code int, body io.ReadCloser) gimlet.ErrorResponse {
	data, err := ioutil.ReadAll(body)
	if err != nil {
		panic(err)
	}
	fmt.Printf(">>> handling error: %s\n", string(data))
	gout := gimlet.ErrorResponse{}
	return gout
}

func (c *Client) CheckJobStatus(ctx context.Context, id string) (*JobStatus, error) {
	client := utility.GetDefaultHTTPRetryableClient()
	defer utility.PutHTTPClient(client)

	req, err := c.makeRequest(ctx, strings.Join([]string{"repobuilder", "check", id}, "/"), http.MethodGet, nil)
	if err != nil {
		return nil, errors.Wrap(err, "problem building job request")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "problem making job submission request")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, c.handleError(resp.StatusCode, resp.Body)
	}
	out := &JobStatus{}
	if err = gimlet.GetJSON(resp.Body, out); err != nil {
		return nil, errors.Wrap(err, "problem reading body of job status response")
	}

	return out, nil
}
