package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// verifyToken hits /api/agent/whoami to confirm the token is live.
// Returns nil on 200, error otherwise. Cheap sanity check used by
// `polar-agent login` and `polar-agent status`. /api/agent/tokens
// would require a user session — agent tokens aren't valid there.
func verifyToken(cfg AgentConfig) error {
	server := strings.TrimRight(cfg.Server, "/")
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		server+"/api/agent/whoami",
		nil,
	)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("token rejected (401)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	return nil
}
