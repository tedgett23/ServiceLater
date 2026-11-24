package records

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/rules"
)

// notifyFromRule delivers what a rule's notify action asked for, in the save's transaction, so nobody hears of a
// change that rolled back. Like starting a workflow, a notification that cannot be sent never blocks the save: the
// template was there when the rule was installed, so this is an app changed since, and the audit says so.
// app_notify decides who actually gets one (not the actor, nobody deactivated, nobody who cannot see the ticket).
func (s *Service) notifyFromRule(ctx context.Context, q *dbgen.Queries, timing string, t Ticket, n rules.Notify) error {
	run := ruleruns.Run{App: n.App, Rule: n.Rule, Timing: timing, Event: "notify", RecordID: &t.ID}
	if s.Apps.Notification(n.App, n.Template) == nil {
		run.Outcome, run.Detail = ruleruns.Failed, "the notification template "+n.Template+" is not installed"
		slog.Error("rule sends a notification template that is not installed", "app", n.App, "rule", n.Rule, "template", n.Template)
		ruleruns.Record(ctx, s.Pool, run)
		return nil
	}
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	add := func(id uuid.UUID) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, raw := range n.UserIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			run.Outcome, run.Detail = ruleruns.Failed, fmt.Sprintf("not sent: %q is not a user id", raw)
			ruleruns.Record(ctx, s.Pool, run)
			return nil
		}
		add(id)
	}
	// A channel post is read by everyone in the channel, not only those who may read the comment, so a notification
	// about a comment never goes to Teams.
	teams := slices.Contains(s.Apps.Notification(n.App, n.Template).Channels, apps.ChannelTeams) && n.CommentID == ""
	for _, raw := range n.GroupIDs {
		gid, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		members, err := q.ListActiveGroupMemberIDs(ctx, gid)
		if err != nil {
			return err
		}
		for _, id := range members {
			add(id)
		}
		// A group is also told in its Teams channel, once, when the template asks and the group has one (the
		// database decides: Teams on, a channel set; app_notify_group_channel).
		if teams {
			if _, err := q.NotifyGroupChannel(ctx, dbgen.NotifyGroupChannelParams{GroupID: gid, TaskID: t.ID, App: n.App, Template: n.Template}); err != nil {
				return err
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if n.CommentID != "" {
		comment, err := uuid.Parse(n.CommentID)
		if err != nil {
			return err
		}
		_, err = q.SendCommentNotification(ctx, dbgen.SendCommentNotificationParams{UserIds: ids, TaskID: t.ID, CommentID: comment, App: n.App, Template: n.Template})
		return err
	}
	_, err := q.SendNotification(ctx, dbgen.SendNotificationParams{UserIds: ids, TaskID: t.ID, App: n.App, Template: n.Template})
	return err
}

// NotifyFromAsyncRule is notifyFromRule for the worker, which runs an async rule outside a save.
func (s *Service) NotifyFromAsyncRule(ctx context.Context, q *dbgen.Queries, t Ticket, n rules.Notify) error {
	return s.notifyFromRule(ctx, q, apps.TimingAsync, t, n)
}
