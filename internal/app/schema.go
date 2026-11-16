package app

const schema = `
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
` + sendingServiceSetupSchema + coverageSchema + customScheduleSchema + deliverySchema + incidentSchema + alertSourceCheckInSchema + plannedDowntimeSchema + telegramStartTokenSchema + telegramTeamSchema + slackTeamSchema + phoneTeamSchema + pushoverSchema + triageAgentSchema + triageAgentCommitCheckSchema + softwareReviewSchema + reviewAgentSchema + agentPlaybookRulesSchema + triageSessionSchema + heldWarnSchema + serverReplacementSchema + playbookRunApprovalSchema + statusPageSchema + downAlarmSchema + settingsFilePartsSchema + servicesWhoseChangesAPersonMustApproveSchema + triageAgentVendorPageHostsSchema
