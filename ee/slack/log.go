package slack

import "log/slog"

// slackLog is the Slack subsystem logger, tagged so connect/disconnect
// events and upstream Slack API failures land distinctly in the log.
var slackLog = slog.Default().With("component", "slack")
