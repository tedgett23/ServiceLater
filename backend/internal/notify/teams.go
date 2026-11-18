package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// TeamsKind is the job that posts one queued message to a group's Teams channel (app_notify_group_channel).
const TeamsKind = "notify.teams"

// TeamsPost is a message for a Teams channel: a title, some text, and a button to open it.
type TeamsPost struct {
	Title, Text, LinkLabel, Link string
}

// Card is the post as Teams' Workflows webhook takes it: a message with one Adaptive Card.
func (p TeamsPost) Card() map[string]any {
	body := []any{map[string]any{"type": "TextBlock", "text": p.Title, "weight": "Bolder", "size": "Medium", "wrap": true}}
	if p.Text != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": p.Text, "wrap": true})
	}
	card := map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard",
		"version": "1.4", "body": body}
	if p.Link != "" {
		card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": p.LinkLabel, "url": p.Link}}
	}
	return map[string]any{"type": "message", "attachments": []any{map[string]any{
		"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": nil, "content": card}}}
}

// TeamsPoster posts to a channel's webhook.
type TeamsPoster struct {
	Client *http.Client
}

// Post sends a card. A 4xx answer (the workflow was deleted, the URL is wrong) is permanent; the rest is retried.
func (t TeamsPoster) Post(ctx context.Context, webhook string, p TeamsPost) error {
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	raw, _ := json.Marshal(p.Card())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(raw))
	if err != nil {
		return &SendError{Err: err, Permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return &SendError{Err: fmt.Errorf("reaching Teams: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &SendError{Err: fmt.Errorf("the Teams channel refused the post: %s %s", resp.Status, oneLine(string(body))),
		Permanent: resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests}
}

// Poster posts queued messages to groups' Teams channels, in the worker role.
type Poster struct {
	Pool  *pgxpool.Pool
	Apps  *apps.Registry
	Keys  *secrets.Keyring
	Teams interface {
		Post(ctx context.Context, webhook string, p TeamsPost) error
	}
}

// Handler posts one queued message. It is worded by its template as the system: a channel post goes to whoever is
// in the channel, so it carries only what the template says (and the admin chose to connect the group).
func (pt *Poster) Handler() jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var p struct {
			DeliveryID uuid.UUID `json:"delivery_id"`
		}
		if err := job.Decode(&p); err != nil {
			return jobs.Permanent(err)
		}
		var d dbgen.GetChannelDeliveryRow
		var post TeamsPost
		var webhook string
		var skip string
		err := db.WithSystem(ctx, pt.Pool, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			var err error
			if d, err = q.GetChannelDelivery(ctx, p.DeliveryID); err != nil {
				return err
			}
			if d.Status != StatusQueued {
				return nil
			}
			rows, err := q.GetSystemSettings(ctx, settings.TeamsPrefix)
			if err != nil {
				return err
			}
			stored := map[string][]byte{}
			for _, r := range rows {
				stored[r.Name] = r.Value
			}
			if !settings.TeamsFrom(stored).Enabled {
				skip = "Teams was turned off before it was posted"
				return nil
			}
			webhook, err = secrets.Get(ctx, q, pt.Keys, settings.TeamsWebhook(d.GroupID.UUID))
			if errors.Is(err, secrets.ErrNotSet) {
				skip = "the group no longer has a Teams channel"
				return nil
			} else if err != nil {
				return err
			}
			if !d.TaskID.Valid {
				skip = "the record was deleted before it was posted"
				return nil
			}
			msg, visible, err := NewRenderer(pt.Apps, q).Render(ctx, dbgen.Notification{Kind: "template", TaskID: d.TaskID,
				App: d.App, Template: d.Template, ActorID: d.ActorID})
			if err != nil {
				return err
			}
			if !visible {
				skip = "the record is gone"
				return nil
			}
			base, err := baseURL(ctx, q)
			post = TeamsPost{Title: msg.Title, Text: msg.Body}
			if base != "" {
				post.Link, post.LinkLabel = base+msg.Link, "Open "+msg.Reference
			}
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Status != StatusQueued {
			return nil
		}
		if skip != "" {
			return recordChannel(ctx, pt.Pool, d.ID, StatusSkipped, skip, job.Attempt)
		}
		return sent(ctx, pt.Pool, d.ID, job, pt.Teams.Post(ctx, webhook, post))
	}
}
