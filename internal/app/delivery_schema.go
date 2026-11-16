package app

const deliverySchema = `
CREATE TABLE contact_methods (
 id TEXT PRIMARY KEY,
 member_id TEXT NOT NULL,
 contact_method_type TEXT NOT NULL,
 settings_json TEXT NOT NULL,
 secret TEXT NOT NULL DEFAULT '',
 sent_to TEXT NOT NULL,
 revision INTEGER NOT NULL,
 send_while_unavailable_or_muted INTEGER NOT NULL,
 failures_in_a_row INTEGER NOT NULL DEFAULT 0,
 failing_since INTEGER NOT NULL DEFAULT 0,
 deleted INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE contact_method_subscriptions (
 contact_method_id TEXT NOT NULL REFERENCES contact_methods(id),
 severity TEXT NOT NULL CHECK(severity IN ('info','warn','sos')),
 PRIMARY KEY(contact_method_id,severity)
);
CREATE TABLE deliveries (
 id TEXT PRIMARY KEY,
 notification_key TEXT NOT NULL,
 alert_id TEXT NOT NULL DEFAULT '',
 incident_id TEXT NOT NULL DEFAULT '',
 incident_revision INTEGER NOT NULL DEFAULT 0,
 contact_method_id TEXT NOT NULL REFERENCES contact_methods(id),
 contact_method_revision INTEGER NOT NULL,
 member_id TEXT NOT NULL,
 contact_method_type TEXT NOT NULL,
 sent_to TEXT NOT NULL,
 purpose TEXT NOT NULL,
 severity TEXT NOT NULL,
 subject TEXT NOT NULL,
 message TEXT NOT NULL,
 title TEXT NOT NULL DEFAULT '',
 alert_message TEXT NOT NULL DEFAULT '',
 notes_from_mmhotline TEXT NOT NULL DEFAULT '',
 incident_status TEXT NOT NULL DEFAULT '',
 alert_received_at INTEGER NOT NULL DEFAULT 0,
 ignore_personal INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL CHECK(state IN ('queued','sending','accepted','delivered','failed','uncertain','cancelled','suppressed','combined')),
 combined_into TEXT NOT NULL DEFAULT '',
 more_sos_incidents_message INTEGER NOT NULL DEFAULT 0,
 reason TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 next_attempt INTEGER NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0,
 first_attempt INTEGER NOT NULL DEFAULT 0,
 reservation TEXT NOT NULL DEFAULT '',
 reserved_until INTEGER NOT NULL DEFAULT 0,
 id_from_sending_service TEXT NOT NULL DEFAULT '',
 sent_from TEXT NOT NULL DEFAULT '',
 public_url TEXT NOT NULL DEFAULT '',
 call_result TEXT NOT NULL DEFAULT '',
 UNIQUE(notification_key,contact_method_id)
);
CREATE INDEX delivery_ready ON deliveries(contact_method_type,state,next_attempt);
CREATE INDEX delivery_incident ON deliveries(incident_id,purpose,state);
CREATE INDEX delivery_contact_method ON deliveries(contact_method_id,state,created_at);
CREATE INDEX delivery_combined_into ON deliveries(combined_into) WHERE combined_into!='';
CREATE TABLE delivery_attempts (
 id TEXT PRIMARY KEY,
 delivery_id TEXT NOT NULL REFERENCES deliveries(id),
 number INTEGER NOT NULL,
 started_at INTEGER NOT NULL,
 completed_at INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL,
 id_from_sending_service TEXT NOT NULL DEFAULT '',
 diagnostic TEXT NOT NULL DEFAULT '',
 contact_method_type TEXT NOT NULL DEFAULT '',
 sending_service TEXT NOT NULL DEFAULT '',
 sent_at INTEGER NOT NULL DEFAULT 0,
 call_result TEXT NOT NULL DEFAULT '',
 call_ended_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX delivery_attempts_sent ON delivery_attempts(contact_method_type,sent_at);
`
