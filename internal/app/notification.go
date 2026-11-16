package app

import "time"

// Notification is the incident/alert owner's request to the delivery owner.
// Key identifies one logical notification, including repeated SOS escalation rounds.
// Empty Recipients means every subscribed contact method; SOS escalation notifications
// explicitly name their current target members and route only disruptive contact method types.
type Notification struct {
	Key, AlertID, IncidentID, Severity, Title, Message, Notes, Purpose string
	IncidentRevision                                                   int64
	Recipients                                                         []string
	IgnorePersonal                                                     bool
	// PeopleOnly keeps the notification off contact method types read by programs
	// (webhooks), such as notices to administrators about MMHotline itself.
	PeopleOnly bool
	// WakesRecipients sends an SOS outside escalation through the recipients'
	// waking contact method types too, which otherwise follow only the active
	// target: an SOS that joins a claimed incident wakes the person who
	// claimed it.
	WakesRecipients bool
	// HappenedAt is when the sender says the alert happened, for display only.
	HappenedAt time.Time
	At         time.Time
}
