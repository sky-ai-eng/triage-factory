package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// slackAPIBase is the Slack Web API base. A var (not a const) so tests can
// point it at a fake server; production never overrides it. No per-org
// override exists — unlike GitHub/Jira, Slack is a single fixed host.
var slackAPIBase = "https://slack.com/api"

// slackHTTPClient bounds every Slack API call so a hung upstream can't wedge
// a connect request. Slack is a third-party network hop, so this is the
// upper bound for the abnormal case — mirrors ssoHTTPClient's rationale for
// the in-network GoTrue admin call, just with a shorter budget since these
// calls run inline in a user-facing request.
var slackHTTPClient = &http.Client{Timeout: 15 * time.Second}

// authTestResult is the subset of Slack's auth.test response the connect
// handler needs: the workspace identity (team_id/team), the bot's own user
// id (so ingest can later detect its own mentions), and the Enterprise Grid
// id when present.
type authTestResult struct {
	TeamID       string
	Team         string
	UserID       string
	EnterpriseID string
}

// slackAuthTest validates botToken against Slack's auth.test and returns
// the workspace identity it resolves to. This is the ONLY way the connect
// handler learns a workspace's id — the admin never types it. A non-2xx
// HTTP response or a Slack-level {"ok":false} both surface as an error
// carrying Slack's own message where available.
func slackAuthTest(ctx context.Context, client *http.Client, botToken string) (*authTestResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/auth.test", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	var out struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error"`
		TeamID       string `json:"team_id"`
		Team         string `json:"team"`
		UserID       string `json:"user_id"`
		EnterpriseID string `json:"enterprise_id"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("slack auth.test: %s", nonEmpty(out.Error, "not ok"))
	}
	if out.TeamID == "" {
		return nil, fmt.Errorf("slack auth.test: response carried no team_id")
	}
	return &authTestResult{
		TeamID:       out.TeamID,
		Team:         out.Team,
		UserID:       out.UserID,
		EnterpriseID: out.EnterpriseID,
	}, nil
}

// slackOpenConnection validates appToken (an app-level xapp- token) via
// apps.connections.open — the Socket Mode handshake's first call. It exists
// here purely as a credential check: a successful call returns a wss:// URL
// good for one connection, which this leaf discards (the socket connection
// manager that actually dials it is a later leaf).
func slackOpenConnection(ctx context.Context, client *http.Client, appToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/apps.connections.open", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+appToken)

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		URL   string `json:"url"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack apps.connections.open: %s", nonEmpty(out.Error, "not ok"))
	}
	return nil
}

// doSlackJSON executes req and decodes the JSON body into out. Slack
// answers 200 even for most application-level failures (the {"ok":false}
// convention) — a non-2xx here means something more fundamental (rate
// limit, upstream outage), so it's surfaced with the raw status and a
// capped body rather than attempting to parse it as the {ok,...} shape.
func doSlackJSON(ctx context.Context, client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slack api request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack api: http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("slack api: decode response: %w", err)
	}
	return nil
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
