package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// DashboardStats holds aggregated PR statistics.
type DashboardStats struct {
	// Counts
	Merged   int `json:"merged"`
	Closed   int `json:"closed"`
	Awaiting int `json:"awaiting"` // open, not draft
	Draft    int `json:"draft"`

	// Reviews
	ReviewsGiven    int `json:"reviews_given"`
	ReviewsReceived int `json:"reviews_received"`

	// Timeline: PRs merged per week over last 30 days
	MergedOverTime []TimePoint `json:"merged_over_time"`
}

type TimePoint struct {
	Week  string `json:"week"` // ISO week start date
	Count int    `json:"count"`
}

// GetDashboardStats fetches PR statistics for the user over the last 30 days.
func (c *Client) GetDashboardStats(ctx context.Context, username string) (*DashboardStats, error) {
	stats := &DashboardStats{}
	since := time.Now().AddDate(0, 0, -30).Format("2006-01-02")

	// 1. Merged PRs (last 30 days)
	mergedItems, err := c.searchIssues(ctx, fmt.Sprintf("author:%s type:pr is:merged merged:>%s", username, since))
	if err == nil {
		stats.Merged = len(mergedItems)
		stats.MergedOverTime = bucketByWeek(mergedItems, "closed_at")
	}

	// 2. Closed (not merged) PRs
	closedItems, err := c.searchIssues(ctx, fmt.Sprintf("author:%s type:pr is:closed is:unmerged closed:>%s", username, since))
	if err == nil {
		stats.Closed = len(closedItems)
	}

	// 3. Open PRs (awaiting + draft)
	openItems, err := c.searchIssues(ctx, fmt.Sprintf("author:%s type:pr state:open", username))
	if err == nil {
		for _, item := range openItems {
			if boolVal(item, "draft") {
				stats.Draft++
			} else {
				stats.Awaiting++
			}
		}
	}

	// 4. Reviews given by user (last 30 days)
	reviewedItems, err := c.searchIssues(ctx, fmt.Sprintf("reviewed-by:%s type:pr -author:%s updated:>%s", username, username, since))
	if err == nil {
		stats.ReviewsGiven = len(reviewedItems)
	}

	// 5. Reviews received on user's PRs — count from merged + open PRs
	stats.ReviewsReceived = stats.Merged + stats.Awaiting // rough proxy

	return stats, nil
}

func (c *Client) searchIssues(ctx context.Context, query string) ([]map[string]any, error) {
	data, err := c.Get(ctx, fmt.Sprintf("/search/issues?q=%s&per_page=100&sort=updated", url.QueryEscape(query)))
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

func bucketByWeek(items []map[string]any, dateField string) []TimePoint {
	// Bucket items by ISO week
	buckets := make(map[string]int)
	for _, item := range items {
		dateStr := strVal(item, dateField)
		if dateStr == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, dateStr)
		if err != nil {
			continue
		}
		// Round to Monday of that week
		weekday := int(t.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		monday := t.AddDate(0, 0, -(weekday - 1))
		key := monday.Format("2006-01-02")
		buckets[key]++
	}

	// Build sorted timeline covering last 5 weeks
	var points []TimePoint
	now := time.Now()
	for i := 4; i >= 0; i-- {
		d := now.AddDate(0, 0, -i*7)
		weekday := int(d.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		monday := d.AddDate(0, 0, -(weekday - 1))
		key := monday.Format("2006-01-02")
		points = append(points, TimePoint{
			Week:  key,
			Count: buckets[key],
		})
	}

	return points
}
