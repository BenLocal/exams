package store

import (
	"context"
	"fmt"
)

// ClaimNotification reserves the right to deliver one notification.
//
// It returns false when the notification has already been delivered
// successfully, and true when the caller should send it. Crucially it returns
// true again for a row whose previous attempt failed, so a transient SMTP or
// webhook outage does not permanently suppress that notification.
func (s *Store) ClaimNotification(ctx context.Context, examID int64, channel, target, event string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO notifications (exam_id, channel, target, event, status, attempts)
		VALUES ($1, $2, $3, $4, 'pending', 1)
		ON CONFLICT (exam_id, channel, target) DO UPDATE
			SET status   = 'pending',
			    event    = EXCLUDED.event,
			    attempts = notifications.attempts + 1
			WHERE notifications.status <> 'sent'
		RETURNING id`, examID, channel, target, event).Scan(&id)
	if isNoRows(err) {
		// Already delivered; nothing to do.
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("claim notification: %w", err)
	}
	return id, true, nil
}

// RecordNotificationResult closes out a claimed notification.
func (s *Store) RecordNotificationResult(ctx context.Context, id int64, sent bool, errMsg string) error {
	status := "failed"
	if sent {
		status = "sent"
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET status = $2,
		    last_error = $3,
		    sent_at = CASE WHEN $2 = 'sent' THEN now() ELSE sent_at END
		WHERE id = $1`, id, status, errMsg)
	if err != nil {
		return fmt.Errorf("record notification result: %w", err)
	}
	return nil
}

// NotificationCounts summarises delivery health for the runs page.
func (s *Store) NotificationCounts(ctx context.Context) (sent, failed int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'sent'),
		       count(*) FILTER (WHERE status = 'failed')
		FROM notifications`).Scan(&sent, &failed)
	return sent, failed, err
}
