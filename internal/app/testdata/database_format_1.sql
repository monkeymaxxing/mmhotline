
CREATE TABLE team_info (name TEXT NOT NULL);
CREATE TABLE members (
 id TEXT PRIMARY KEY,
 sign_in_email_address TEXT NOT NULL,
 name TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('administrator','member')),
 removed_at INTEGER
);
CREATE TABLE browser_sign_ins (
 token_sha256 TEXT PRIMARY KEY,
 member_id TEXT NOT NULL REFERENCES members(id),
 kind TEXT NOT NULL CHECK(kind IN ('one_time_link','sign_in_cookie')),
 expires_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX active_member_sign_in_email_address ON members(sign_in_email_address) WHERE removed_at IS NULL;

CREATE TABLE sending_service_setups (
 sending_service TEXT PRIMARY KEY,
 settings_json TEXT NOT NULL,
 secret_settings_json TEXT NOT NULL,
 set_up_at INTEGER NOT NULL,
 set_up_by TEXT NOT NULL
);

CREATE TABLE coverage_settings (id INTEGER PRIMARY KEY CHECK(id=1), active_kind TEXT NOT NULL, revision INTEGER NOT NULL);
INSERT INTO coverage_settings VALUES (1,'',0);
CREATE TABLE coverage_schedules (kind TEXT PRIMARY KEY, configuration TEXT NOT NULL);
CREATE TABLE coverage_handoff (id INTEGER PRIMARY KEY CHECK(id=1), schedule_revision INTEGER NOT NULL, primary_id TEXT NOT NULL);
INSERT INTO coverage_handoff VALUES (1,-1,'');
CREATE TABLE member_availability (member_id TEXT PRIMARY KEY REFERENCES members(id), unavailable_until INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 0);
CREATE TABLE team_mute (id INTEGER PRIMARY KEY CHECK(id=1), until_at INTEGER NOT NULL, actor_member_id TEXT NOT NULL, revision INTEGER NOT NULL, expired INTEGER NOT NULL);
INSERT INTO team_mute VALUES (1,0,'',0,1);
CREATE TABLE on_call_claim (id INTEGER PRIMARY KEY CHECK(id=1), member_id TEXT NOT NULL, actor_member_id TEXT NOT NULL, until_at INTEGER NOT NULL, revision INTEGER NOT NULL, expired INTEGER NOT NULL);
INSERT INTO on_call_claim VALUES (1,'','',0,0,1);
CREATE TABLE coverage_changes (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, actor_member_id TEXT NOT NULL, member_id TEXT NOT NULL, revision INTEGER NOT NULL, occurred_at INTEGER NOT NULL, until_at INTEGER NOT NULL, severity TEXT NOT NULL, title TEXT NOT NULL, message TEXT NOT NULL);

CREATE TABLE custom_schedule_timeline (configuration_revision INTEGER NOT NULL, starts_at INTEGER NOT NULL, ends_at INTEGER NOT NULL, primary_member_id TEXT NOT NULL, backup_member_id TEXT NOT NULL, PRIMARY KEY(configuration_revision, starts_at));
CREATE TABLE custom_schedule_timeline_state (id INTEGER PRIMARY KEY CHECK(id=1), timeline_configuration_revision INTEGER NOT NULL, worked_out_at INTEGER NOT NULL, shifts_until INTEGER NOT NULL, next_run_at INTEGER NOT NULL, attempted_configuration_revision INTEGER NOT NULL, attempted_member_sign_in_email_addresses_sha256 TEXT NOT NULL, failing_since INTEGER NOT NULL, few_shifts_left_notice_sent INTEGER NOT NULL, last_failure_message TEXT NOT NULL);
INSERT INTO custom_schedule_timeline_state VALUES (1,0,0,0,0,0,'',0,0,'');
CREATE TABLE custom_schedule_code_versions (
 id TEXT PRIMARY KEY,
 created_at INTEGER NOT NULL,
 created_by_member_id TEXT NOT NULL,
 created_by_name TEXT NOT NULL,
 configuration TEXT NOT NULL
);

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

CREATE TABLE sources(id TEXT PRIMARY KEY,name TEXT NOT NULL,kind TEXT NOT NULL,active INTEGER NOT NULL,created_at INTEGER NOT NULL,last_heard_at INTEGER NOT NULL DEFAULT 0,warn_after_no_check_in_for INTEGER NOT NULL DEFAULT 120,sos_after_no_check_in_for INTEGER NOT NULL DEFAULT 300);
CREATE TABLE signing_secrets(id TEXT PRIMARY KEY,source_id TEXT NOT NULL REFERENCES sources(id),secret BLOB NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX signing_secrets_source ON signing_secrets(source_id);
CREATE TABLE source_rates(source_id TEXT PRIMARY KEY REFERENCES sources(id),bucket INTEGER NOT NULL,alert_count INTEGER NOT NULL);
CREATE TABLE team_alert_rate(id INTEGER PRIMARY KEY CHECK(id=1),hour_bucket INTEGER NOT NULL,warning_and_info_alerts INTEGER NOT NULL);
CREATE TABLE alerts(sequence INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,source_id TEXT NOT NULL,sender_alert_id TEXT NOT NULL,actor TEXT NOT NULL,replay_key TEXT NOT NULL,incident_id TEXT NOT NULL,sender_incident_key TEXT NOT NULL,severity TEXT NOT NULL,title TEXT NOT NULL,message TEXT NOT NULL,notes TEXT NOT NULL,received_at INTEGER NOT NULL,happened_at INTEGER NOT NULL DEFAULT 0,UNIQUE(source_id,replay_key));
CREATE INDEX alerts_incident ON alerts(incident_id,sequence);
CREATE INDEX alerts_received ON alerts(received_at,sequence);
CREATE TABLE incident_settings(id INTEGER PRIMARY KEY CHECK(id=1),response_seconds INTEGER NOT NULL);
INSERT INTO incident_settings VALUES(1,300);
CREATE TABLE incidents(id TEXT PRIMARY KEY,opening_alert_id TEXT NOT NULL REFERENCES alerts(id),source_id TEXT NOT NULL,sender_incident_key TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('open','claimed','resolved')),owner_member_id TEXT NOT NULL,step TEXT NOT NULL CHECK(step IN ('triage_agent','primary','chosen_person','backup','only_member','everyone_available')),revision INTEGER NOT NULL,targets TEXT NOT NULL,ignore_personal INTEGER NOT NULL,created_at INTEGER NOT NULL,claimed_at INTEGER NOT NULL,resolved_at INTEGER NOT NULL,deadline INTEGER NOT NULL,repeat_at INTEGER NOT NULL,repeat_generation INTEGER NOT NULL,team_mute_revision INTEGER NOT NULL,primary_at INTEGER NOT NULL,backup_at INTEGER NOT NULL,everyone_available_at INTEGER NOT NULL);
CREATE UNIQUE INDEX incidents_active_key ON incidents(source_id,sender_incident_key) WHERE status!='resolved' AND sender_incident_key!='';
CREATE INDEX incidents_pending ON incidents(status,deadline,repeat_at);
CREATE TABLE incident_actions(id INTEGER PRIMARY KEY AUTOINCREMENT,incident_id TEXT NOT NULL REFERENCES incidents(id),revision INTEGER NOT NULL,actor TEXT NOT NULL,kind TEXT NOT NULL,message TEXT NOT NULL,actor_name TEXT NOT NULL,target_member_ids TEXT NOT NULL,previous_owner_member_id TEXT NOT NULL,related_alert_id TEXT NOT NULL,occurred_at INTEGER NOT NULL,note TEXT NOT NULL DEFAULT '',note_given_in_triage_run_id TEXT NOT NULL DEFAULT '');
CREATE INDEX incident_actions_incident ON incident_actions(incident_id,id);

CREATE TABLE check_in_watch(
 alert_source_id TEXT PRIMARY KEY REFERENCES sources(id),
 last_check_in_at INTEGER NOT NULL,
 last_sent_at INTEGER NOT NULL DEFAULT 0,
 sent_by_node_name TEXT NOT NULL,
 nodes_in_whole_node_maintenance TEXT NOT NULL,
 check_ins_stopped INTEGER NOT NULL DEFAULT 0 CHECK(check_ins_stopped IN (0,1)),
 alerted TEXT NOT NULL DEFAULT '' CHECK(alerted IN ('','warning','sos')),
 alerted_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE planned_downtimes(
 id TEXT PRIMARY KEY,
 alert_source_id TEXT NOT NULL,
 until INTEGER NOT NULL,
 reason TEXT NOT NULL,
 started_by_member_id TEXT NOT NULL,
 started_at INTEGER NOT NULL,
 ended_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX planned_downtimes_target ON planned_downtimes(alert_source_id,until);

CREATE TABLE telegram_start_tokens (
 token_hash TEXT PRIMARY KEY,
 member_id TEXT NOT NULL,
 telegram_connection TEXT NOT NULL CHECK(telegram_connection IN ('personal_chat','team_group')),
 expires_at INTEGER NOT NULL,
 consumed_at INTEGER NOT NULL DEFAULT 0,
 consumed_by_telegram_chat_id INTEGER NOT NULL DEFAULT 0,
 consumed_by_telegram_user_id INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX telegram_start_tokens_member ON telegram_start_tokens(member_id,expires_at);

CREATE TABLE telegram_user_links (
 telegram_user_id INTEGER PRIMARY KEY,
 member_id TEXT NOT NULL,
 linked_at INTEGER NOT NULL
);

CREATE TABLE slack_user_links (
 slack_workspace_id TEXT NOT NULL,
 slack_user_id TEXT NOT NULL,
 member_id TEXT NOT NULL,
 linked_at INTEGER NOT NULL,
 PRIMARY KEY(slack_workspace_id,slack_user_id)
);
CREATE TABLE slack_connection_codes (
 code_hash TEXT PRIMARY KEY,
 member_id TEXT NOT NULL,
 slack_connection TEXT NOT NULL CHECK(slack_connection IN ('personal_direct_message','shared_channel')),
 expires_at INTEGER NOT NULL,
 consumed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX slack_connection_codes_member ON slack_connection_codes(member_id,expires_at);

CREATE TABLE contact_card_downloads (
 member_id TEXT PRIMARY KEY,
 contact_card_fingerprint TEXT NOT NULL,
 downloaded_at INTEGER NOT NULL
);
CREATE TABLE contact_card_announcements (
 id INTEGER PRIMARY KEY CHECK(id=1),
 contact_card_fingerprint TEXT NOT NULL
);

CREATE TABLE pushover_receipts (
 delivery_id TEXT PRIMARY KEY REFERENCES deliveries(id),
 receipt TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('sending','open','cancelling','acknowledged','expired','cancelled','failed','uncertain')),
 next_poll_at INTEGER NOT NULL DEFAULT 0,
 expires_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX pushover_receipts_due ON pushover_receipts(state,next_poll_at);
CREATE TABLE pushover_receipt_incidents (
 delivery_id TEXT NOT NULL REFERENCES pushover_receipts(delivery_id),
 incident_id TEXT NOT NULL REFERENCES incidents(id),
 PRIMARY KEY(delivery_id,incident_id)
);
CREATE INDEX pushover_receipt_incidents_incident ON pushover_receipt_incidents(incident_id,delivery_id);

CREATE TABLE triage_agent_setup (
 id INTEGER PRIMARY KEY CHECK(id=1), coding_agent TEXT NOT NULL,
 credential_type TEXT NOT NULL, credential TEXT NOT NULL,
 model TEXT NOT NULL, reasoning_effort TEXT NOT NULL,
 set_up_at INTEGER NOT NULL, set_up_by TEXT NOT NULL,
 credential_expires_on TEXT NOT NULL DEFAULT '',
 current_chatgpt_sign_in TEXT NOT NULL DEFAULT '',
 expiry_reminder_30_days_before_sent_at INTEGER NOT NULL DEFAULT 0,
 expiry_reminder_7_days_before_sent_at INTEGER NOT NULL DEFAULT 0,
 sign_in_failing_alert_sent INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE triage_agent_internet_access (
 id INTEGER PRIMARY KEY CHECK(id=1), internet_access TEXT NOT NULL CHECK(internet_access IN ('listed_hosts_only','any_site')),
 changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL
);
CREATE TABLE triage_agent_listed_hosts (
 host TEXT NOT NULL, port INTEGER NOT NULL, added_at INTEGER NOT NULL, added_by TEXT NOT NULL,
 PRIMARY KEY(host, port)
);
CREATE TABLE triage_agent_inventory_servers (
 server_name TEXT NOT NULL, inventory TEXT NOT NULL, PRIMARY KEY(server_name, inventory)
);
CREATE TABLE triage_agent_runs (
 id TEXT PRIMARY KEY, purpose TEXT NOT NULL CHECK(purpose IN ('setup_test','sign_in_check','triage_session','software_review','commit_review','review_agent_setup_test','review_agent_sign_in_check','playbook_review')),
 coding_agent TEXT NOT NULL, credential_type TEXT NOT NULL, coding_agent_session_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('running','finished','stopped','failed')),
 started_at INTEGER NOT NULL, started_by TEXT NOT NULL, ended_at INTEGER NOT NULL DEFAULT 0,
 answer TEXT NOT NULL DEFAULT '', failure_reason TEXT NOT NULL DEFAULT '', cost_in_us_dollars REAL,
 input_tokens INTEGER NOT NULL DEFAULT 0, cached_input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 coding_agent_session_total_cost_in_us_dollars REAL,
 coding_agent_session_total_input_tokens INTEGER NOT NULL DEFAULT 0,
 coding_agent_session_total_cached_input_tokens INTEGER NOT NULL DEFAULT 0,
 coding_agent_session_total_output_tokens INTEGER NOT NULL DEFAULT 0,
 triage_session_id TEXT NOT NULL DEFAULT '',
 inventory_servers_not_listed_because TEXT NOT NULL DEFAULT '',
 vendor_page_hosts_not_listed_because TEXT NOT NULL DEFAULT ''
);
CREATE TABLE triage_agent_commits (
 run_id TEXT NOT NULL REFERENCES triage_agent_runs(id), request_id TEXT NOT NULL,
 base_commit_id TEXT NOT NULL,
 outcome TEXT NOT NULL CHECK(outcome IN ('pushed','waits_for_a_person','refused','failed')),
 reason TEXT NOT NULL DEFAULT '', agent_commit_id TEXT NOT NULL DEFAULT '',
 pushed_commit_id TEXT NOT NULL DEFAULT '', private_ref TEXT NOT NULL DEFAULT '',
 working_tree_not_updated_because TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(run_id, request_id)
);
CREATE TABLE triage_agent_requests (
 run_id TEXT NOT NULL REFERENCES triage_agent_runs(id), request_id TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('playbook_run','report','commit_review')),
 playbook TEXT NOT NULL DEFAULT '', inventory TEXT NOT NULL DEFAULT '',
 alert_severity TEXT NOT NULL DEFAULT '', report_text TEXT NOT NULL DEFAULT '',
 requested_at INTEGER NOT NULL,
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','ran_and_succeeded','ran_and_failed','waits_for_a_person','refused','recorded')),
 message TEXT NOT NULL DEFAULT '', answered_at INTEGER NOT NULL DEFAULT 0,
 playbook_read_at_commit_id TEXT NOT NULL DEFAULT '',
 playbook_run_started_at INTEGER NOT NULL DEFAULT 0,
 playbook_run_waiting_for_another_run_since INTEGER NOT NULL DEFAULT 0,
 playbook_run_ended_at INTEGER NOT NULL DEFAULT 0, playbook_run_exit_code INTEGER,
 playbook_run_output TEXT NOT NULL DEFAULT '',
 diagnosis TEXT NOT NULL DEFAULT '',
 commit_id TEXT NOT NULL DEFAULT '',
 commit_review_decision TEXT NOT NULL DEFAULT '' CHECK(commit_review_decision IN ('','approve','needs_a_person')),
 PRIMARY KEY(run_id, request_id)
);
CREATE TABLE triage_agent_run_steps (
 run_id TEXT NOT NULL REFERENCES triage_agent_runs(id), sequence INTEGER NOT NULL,
 occurred_at INTEGER NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('message','command','tool')),
 text TEXT NOT NULL, PRIMARY KEY(run_id, sequence)
);
CREATE TABLE triage_agent_proxy_connections (
 id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, started_at INTEGER NOT NULL,
 host TEXT NOT NULL, port INTEGER NOT NULL, address TEXT NOT NULL,
 decision TEXT NOT NULL CHECK(decision IN ('allowed','refused')), refusal_reason TEXT NOT NULL DEFAULT '',
 bytes_sent INTEGER NOT NULL DEFAULT 0, bytes_received INTEGER NOT NULL DEFAULT 0, ended_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE triage_agent_commit_trust_throughs (
 id INTEGER PRIMARY KEY,
 commit_id TEXT NOT NULL,
 unix_user TEXT NOT NULL, server_name TEXT NOT NULL,
 trusted_at INTEGER NOT NULL
);
CREATE TABLE triage_agent_commit_check_state (
 id INTEGER PRIMARY KEY CHECK(id=1),
 commit_signing_public_key TEXT NOT NULL,
 trusted_commit_signing_public_keys_json TEXT NOT NULL,
 last_checked_commit_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE triage_agent_refused_commits (
 commit_id TEXT PRIMARY KEY,
 reason_kind TEXT NOT NULL CHECK(reason_kind IN ('not_signed_by_a_trusted_key','triage_agent_change_needs_review')),
 paths_json TEXT NOT NULL, changed_paths_json TEXT NOT NULL,
 author TEXT NOT NULL, committed_at INTEGER NOT NULL, message TEXT NOT NULL,
 diff TEXT NOT NULL, diff_truncated INTEGER NOT NULL,
 found_at INTEGER NOT NULL,
 reviewed_at INTEGER,
 reviewed_by_member_id TEXT REFERENCES members(id),
 reviewed_by_unix_user TEXT NOT NULL DEFAULT '',
 reviewed_through_trust_through_id INTEGER REFERENCES triage_agent_commit_trust_throughs(id),
 reviewer_kind TEXT NOT NULL DEFAULT '' CHECK(reviewer_kind IN ('','member','unix_user','trust_through','triage_agent')),
 triage_agent_review_run_id TEXT REFERENCES triage_agent_runs(id),
 alert_sent TEXT NOT NULL DEFAULT '' CHECK(alert_sent IN ('','mmhotline_warning','triage_agent_approval','triage_agent_report'))
);

CREATE TABLE software_review_interval (
 id INTEGER PRIMARY KEY CHECK(id=1),
 interval_hours INTEGER CHECK(interval_hours BETWEEN 1 AND 8760),
 changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL
);

CREATE TABLE review_agent_setup (
 id INTEGER PRIMARY KEY CHECK(id=1), coding_agent TEXT NOT NULL,
 credential_type TEXT NOT NULL, credential TEXT NOT NULL,
 model TEXT NOT NULL, reasoning_effort TEXT NOT NULL,
 set_up_at INTEGER NOT NULL, set_up_by TEXT NOT NULL,
 credential_expires_on TEXT NOT NULL DEFAULT '',
 current_chatgpt_sign_in TEXT NOT NULL DEFAULT '',
 expiry_reminder_30_days_before_sent_at INTEGER NOT NULL DEFAULT 0,
 expiry_reminder_7_days_before_sent_at INTEGER NOT NULL DEFAULT 0,
 sign_in_failing_alert_sent INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE playbook_run_reviews (
 id INTEGER PRIMARY KEY,
 run_id TEXT NOT NULL, request_id TEXT NOT NULL,
 review_run_id TEXT NOT NULL DEFAULT '',
 coding_agent TEXT NOT NULL, credential_type TEXT NOT NULL, commit_id TEXT NOT NULL,
 outcome TEXT NOT NULL CHECK(outcome IN ('approved','refused','could_not_review')),
 reason TEXT NOT NULL, started_at INTEGER NOT NULL, ended_at INTEGER NOT NULL,
 playbook_text TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(run_id, request_id) REFERENCES triage_agent_requests(run_id, request_id)
);

CREATE TABLE agent_playbook_rules (
 inventory TEXT NOT NULL, action_kind TEXT NOT NULL,
 rule TEXT NOT NULL CHECK(rule IN ('get_permission_then_act','warn_wait_then_act','act_then_report')),
 warning_wait_minutes INTEGER NOT NULL DEFAULT 0, approvals_needed INTEGER NOT NULL DEFAULT 0,
 changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL,
 CHECK((rule='warn_wait_then_act' AND warning_wait_minutes BETWEEN 1 AND 1440) OR (rule<>'warn_wait_then_act' AND warning_wait_minutes=0)),
 CHECK((rule='get_permission_then_act' AND approvals_needed IN (1,2)) OR (rule<>'get_permission_then_act' AND approvals_needed=0)),
 PRIMARY KEY(inventory, action_kind)
);
CREATE TABLE agent_playbook_rules_for_listed_playbooks (
 inventory TEXT NOT NULL, playbook_name TEXT NOT NULL,
 its_approval_also_approves_in_the_same_session TEXT NOT NULL DEFAULT '',
 starts_a_server_replacement INTEGER NOT NULL DEFAULT 0 CHECK(starts_a_server_replacement IN (0,1)),
 rule TEXT NOT NULL CHECK(rule IN ('get_permission_then_act','warn_wait_then_act','act_then_report')),
 warning_wait_minutes INTEGER NOT NULL DEFAULT 0, approvals_needed INTEGER NOT NULL DEFAULT 0,
 changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL,
 CHECK((rule='warn_wait_then_act' AND warning_wait_minutes BETWEEN 1 AND 1440) OR (rule<>'warn_wait_then_act' AND warning_wait_minutes=0)),
 CHECK((rule='get_permission_then_act' AND approvals_needed IN (1,2)) OR (rule<>'get_permission_then_act' AND approvals_needed=0)),
 PRIMARY KEY(inventory, playbook_name)
);

CREATE TABLE triage_sessions (
 id TEXT PRIMARY KEY, started_at INTEGER NOT NULL,
 ended_at INTEGER NOT NULL DEFAULT 0, ended_because TEXT NOT NULL DEFAULT '',
 taken_over_by_member_id TEXT NOT NULL DEFAULT '', taken_over_at INTEGER NOT NULL DEFAULT 0,
 hand_over_run_id TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX triage_sessions_one_open ON triage_sessions(ended_at) WHERE ended_at=0;
CREATE TABLE triage_session_alerts (
 triage_session_id TEXT NOT NULL REFERENCES triage_sessions(id),
 alert_id TEXT NOT NULL UNIQUE REFERENCES alerts(id),
 joined_at INTEGER NOT NULL, sent_in_run_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX triage_session_alerts_session ON triage_session_alerts(triage_session_id, sent_in_run_id);
CREATE TABLE triage_session_alert_cursor (id INTEGER PRIMARY KEY CHECK(id=1), last_considered_alert_sequence INTEGER NOT NULL);
INSERT INTO triage_session_alert_cursor VALUES(1,0);

CREATE TABLE held_warn_alerts (
 alert_id TEXT PRIMARY KEY REFERENCES alerts(id),
 held_until INTEGER NOT NULL,
 triage_session_id TEXT NOT NULL DEFAULT '',
 held_with_sos_incident_id TEXT NOT NULL DEFAULT '',
 ended_at INTEGER NOT NULL DEFAULT 0,
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','delivered','taken_over','resolved')),
 outcome_reason TEXT NOT NULL DEFAULT '',
 taken_over_by TEXT NOT NULL DEFAULT '' CHECK(taken_over_by IN ('','report','playbook_run_request')),
 taken_over_link TEXT NOT NULL DEFAULT ''
);

CREATE TABLE server_replacements (
 id INTEGER PRIMARY KEY,
 triage_session_id TEXT NOT NULL REFERENCES triage_sessions(id),
 incident_id TEXT NOT NULL, playbook TEXT NOT NULL, inventory TEXT NOT NULL,
 started_at INTEGER NOT NULL, sos_waits_until INTEGER NOT NULL,
 ended_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX server_replacements_by_session ON server_replacements(triage_session_id);

CREATE TABLE triage_agent_playbook_runs_waiting_for_a_person (
 id TEXT PRIMARY KEY,
 run_id TEXT NOT NULL, request_id TEXT NOT NULL, triage_session_id TEXT NOT NULL,
 playbook TEXT NOT NULL, inventory TEXT NOT NULL, commit_id TEXT NOT NULL,
 action_kinds TEXT NOT NULL, playbook_text TEXT NOT NULL, mmx_explain_output TEXT NOT NULL,
 rule TEXT NOT NULL CHECK(rule IN ('get_permission_then_act','warn_wait_then_act')),
 warning_wait_minutes INTEGER NOT NULL, approvals_needed INTEGER NOT NULL,
 rule_from_listed_playbook INTEGER NOT NULL DEFAULT 0 CHECK(rule_from_listed_playbook IN (0,1)),
 its_approval_also_approves_in_the_same_session TEXT NOT NULL DEFAULT '',
 covered_by_approval_request_id TEXT NOT NULL DEFAULT '',
 judged_at INTEGER NOT NULL, incident_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('open','approved','refused','stopped','running','stale','ran_and_succeeded','ran_and_failed','could_not_run')),
 state_changed_at INTEGER NOT NULL, result_message TEXT NOT NULL DEFAULT '',
 stopped_by_member_id TEXT NOT NULL DEFAULT '', stop_reason TEXT NOT NULL DEFAULT '',
 result_for_the_agent TEXT NOT NULL DEFAULT 'none' CHECK(result_for_the_agent IN ('none','waits_for_the_next_run','sent','could_not_reach_the_agent')),
 result_sent_in_run_id TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(run_id, request_id) REFERENCES triage_agent_requests(run_id, request_id)
);
CREATE INDEX triage_agent_playbook_runs_waiting_for_a_person_state ON triage_agent_playbook_runs_waiting_for_a_person(state, state_changed_at);
CREATE INDEX triage_agent_playbook_runs_waiting_for_a_person_session ON triage_agent_playbook_runs_waiting_for_a_person(triage_session_id, result_for_the_agent);
CREATE UNIQUE INDEX triage_agent_playbook_runs_waiting_for_a_person_one_use_of_a_cover ON triage_agent_playbook_runs_waiting_for_a_person(covered_by_approval_request_id, playbook) WHERE covered_by_approval_request_id<>'';
CREATE TABLE playbook_run_approval_decisions (
 id INTEGER PRIMARY KEY,
 approval_request_id TEXT NOT NULL REFERENCES triage_agent_playbook_runs_waiting_for_a_person(id),
 member_id TEXT NOT NULL REFERENCES members(id),
 decision TEXT NOT NULL CHECK(decision IN ('approve','refuse')),
 reason TEXT NOT NULL, decided_at INTEGER NOT NULL,
 UNIQUE(approval_request_id, member_id, decision)
);

CREATE TABLE status_page_setup (
 id INTEGER PRIMARY KEY CHECK(id=1),
 cloudflare_account_id TEXT NOT NULL,
 host_name TEXT NOT NULL,
 bucket_name TEXT NOT NULL,
 r2_bucket_url TEXT NOT NULL,
 page_title TEXT NOT NULL,
 r2_access_key_id TEXT NOT NULL,
 r2_secret_access_key TEXT NOT NULL,
 set_up_at INTEGER NOT NULL,
 set_up_by TEXT NOT NULL
);

CREATE TABLE mmhotline_down_alarm_setup (
 id INTEGER PRIMARY KEY CHECK(id=1),
 cloudflare_account_id TEXT NOT NULL,
 cloudflare_api_token TEXT NOT NULL,
 worker_name TEXT NOT NULL,
 worker_url TEXT NOT NULL,
 check_in_token TEXT NOT NULL,
 uploaded_worker_sha256 TEXT NOT NULL,
 last_handled_send_id INTEGER NOT NULL,
 check_ins_failing_alert_sent INTEGER NOT NULL,
 set_up_at INTEGER NOT NULL,
 set_up_by TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS settings_file_parts (
 part TEXT PRIMARY KEY,
 applied_fingerprint TEXT NOT NULL DEFAULT '',
 applied_at INTEGER NOT NULL DEFAULT 0,
 applied_values TEXT NOT NULL DEFAULT '',
 failure_reason TEXT NOT NULL DEFAULT '',
 failed_at INTEGER NOT NULL DEFAULT 0,
 notes TEXT NOT NULL DEFAULT ''
);

CREATE TABLE services_whose_changes_a_person_must_approve (
 id INTEGER PRIMARY KEY CHECK(id=1),
 service_names TEXT NOT NULL CHECK(json_type(service_names)='array'),
 changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL
);

CREATE TABLE triage_agent_vendor_page_hosts (
 host TEXT NOT NULL, port INTEGER NOT NULL, program TEXT NOT NULL,
 page TEXT NOT NULL CHECK(page IN ('releases_page','security_advisories_page')),
 PRIMARY KEY(host, port, program, page)
);
