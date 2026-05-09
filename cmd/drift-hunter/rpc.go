package main

// Thin JSON-RPC v2 client for hived's webserver_plugin. We deliberately
// don't pull in any third-party HTTP/JSON-RPC libraries — net/http +
// encoding/json keep the tool dependency-free.
//
// The client retries once with a 2-second backoff so a freshly-spun-up
// hived API (which can take a couple of seconds to bind 18090) doesn't
// fail the tool on the very first call.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// rpcClient bundles the endpoint URL with a re-usable http.Client. We
// hold a single instance per drift-hunter run.
type rpcClient struct {
	endpoint string
	http     *http.Client
}

func newRPCClient(endpoint string) *rpcClient {
	return &rpcClient{
		endpoint: endpoint,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// call posts a single JSON-RPC v2 request and returns the raw `result`
// envelope. Errors from hived (env.Error != null) are returned as Go
// errors so the caller can short-circuit cleanly.
func (c *rpcClient) call(method string, params interface{}) (json.RawMessage, error) {
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Post(c.endpoint, "application/json", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", c.endpoint, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("decode %s: %w (body=%q)", method, err, string(respBody))
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, fmt.Errorf("%s: %s", method, string(env.Error))
	}
	return env.Result, nil
}

// probe sanity-checks that hived is up. Used at startup so the tool
// fails fast with a clear message rather than reporting cryptic post-
// errors for every comparator.
func (c *rpcClient) probe() error {
	_, err := c.call("database_api.get_config", map[string]interface{}{})
	if err == nil {
		return nil
	}
	// Cold-start grace period: hived's webserver may need a couple of
	// seconds to bind after the snapshot finishes loading.
	time.Sleep(2 * time.Second)
	_, err = c.call("database_api.get_config", map[string]interface{}{})
	return err
}
