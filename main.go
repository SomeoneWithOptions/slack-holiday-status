package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/slack-go/slack"
)

const (
	diafestivoURL = "https://api.diafestivo.co/next"
	statusEmoji   = ":flag-co:"
	statusTextFmt = "Colombia Holiday: %s"
	tzName        = "America/Bogota"
	httpTimeout   = 10 * time.Second
)

type nextHoliday struct {
	Name      string `json:"name"`
	Date      string `json:"date"`
	IsToday   bool   `json:"isToday"`
	DaysUntil int    `json:"daysUntil"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// 1. Load timezone
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", tzName, err)
	}

	// 2. Read Slack user token
	token := os.Getenv("SLACK_USER_TOKEN")
	if token == "" {
		return fmt.Errorf("SLACK_USER_TOKEN env var is not set")
	}

	// 3. Compute end-of-day in Bogota time
	now := time.Now().In(loc)
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, loc)

	// 4. Check if today is a Colombian holiday
	h, err := fetchHoliday()
	if err != nil {
		return fmt.Errorf("fetch holiday: %w", err)
	}
	if !h.IsToday {
		slog.Info("not a holiday today", "next", h.Name, "days_until", h.DaysUntil)
		return nil
	}

	slog.Info("holiday detected", "holiday", h.Name, "date", h.Date)

	// 5. Build Slack client
	httpClient := &http.Client{Timeout: httpTimeout}
	api := slack.New(token, slack.OptionHTTPClient(httpClient))

	// 5.5. Bail out if a status is already manually set (e.g. PTO)
	profile, err := api.GetUserProfile(&slack.GetUserProfileParameters{})
	if err != nil {
		return fmt.Errorf("get slack profile: %w", err)
	}
	if profile.StatusEmoji != "" || profile.StatusText != "" {
		slog.Info("status already set, skipping holiday status",
			"current_emoji", profile.StatusEmoji,
			"current_text", profile.StatusText,
		)
		return nil
	}

	// 6. Set custom status
	statusText := fmt.Sprintf(statusTextFmt, h.Name)
	if err := api.SetUserCustomStatus(statusText, statusEmoji, endOfDay.Unix()); err != nil {
		return fmt.Errorf("set slack status: %w", err)
	}

	// 7. Set DND until end of day (round up by 1 min to never undershoot)
	minutes := int(time.Until(endOfDay).Minutes()) + 1
	if _, err := api.SetSnooze(minutes); err != nil {
		return fmt.Errorf("set slack dnd: %w", err)
	}

	slog.Info("holiday status applied",
		"holiday", h.Name,
		"status_text", statusText,
		"expires_at", endOfDay,
		"dnd_minutes", minutes,
	)
	return nil
}

func fetchHoliday() (*nextHoliday, error) {
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(diafestivoURL)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", diafestivoURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, diafestivoURL)
	}

	var h nextHoliday
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &h, nil
}
