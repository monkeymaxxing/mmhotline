// The MMHotline-down alarm: a Cloudflare Worker with one SQLite-backed Durable
// Object, in the team's own Cloudflare account. MMHotline checks in every
// 60 seconds with its held messages: ready-made HTTPS requests to the people
// on call, each with a delay. Each check-in moves the Durable Object's alarm.
// When the check-ins stop, the alarm sends each held message after its
// delay, so the person on call is woken while MMHotline is down. The next
// check-in's answer lists what was sent, and MMHotline sends the all-clear.
//
// Set-up uploads this one file through Cloudflare's API;
// it needs no build step and no npm package. The logic lives in plain
// functions that take the storage, the time, and the send function, so
// mmhotline_down_alarm_worker_test.mjs runs them under `node --test` with a fake storage.
// docs/changing-mmhotline/design/mmhotline-down-alarm-and-status-page.md owns the behavior.

import { DurableObject } from "cloudflare:workers";

// The secret, the Durable Object class, and the object's name keep the
// feature's earlier name, the Lifeline, so an upgraded Worker keeps the
// secret and the stored state an installed Worker already has
// (internal/downalarm/downalarm.go).
export const CHECK_IN_TOKEN_SECRET = "MMHOTLINE_LIFELINE_CHECK_IN_TOKEN";
// A check-in of 1 MiB holds 64 held messages at their largest, and hundreds
// at their real size of about 1 KiB; the bound keeps one faulty check-in
// from filling the object's storage.
export const MAXIMUM_CHECK_IN_BYTES = 1024 * 1024;
export const MAXIMUM_HELD_MESSAGE_BYTES = 16 * 1024;
// Three minutes, MMHotline's lowest wake time: MMHotline checks in every
// minute and at once when it starts, so a server reboot leaves at most about
// 2.5 minutes without a check-in, and a shorter delay would wake a person
// for a reboot.
export const MINIMUM_DELAY_SECONDS = 180;
// The largest 32-bit integer, so every program's integer type holds it.
export const MAXIMUM_DELAY_SECONDS = 2147483647;
// The Workers Free plan allows 50 outgoing requests per invocation; the
// rest of the due messages go in the next alarm, set for at once.
export const SENDS_PER_ALARM = 40;
export const RETRY_INTERVAL_MS = 30 * 1000;
// A message that no service accepted within 10 minutes of its due time is
// given up; by then it is no longer a timely wake-up.
export const SEND_WINDOW_MS = 10 * 60 * 1000;
export const ANSWER_TIME_LIMIT_MS = 30 * 1000;
// The send log keeps 7 days, at most 200 sends: MMHotline handles each send
// at its first accepted check-in, and the log is only for that and for people.
export const SEND_LOG_MS = 7 * 24 * 60 * 60 * 1000;
export const SEND_LOG_ENTRIES = 200;
// Cloudflare's storage API takes at most 128 keys in one put or delete
// (https://developers.cloudflare.com/durable-objects/api/sqlite-storage-api/#put),
// so a larger write goes in parts.
export const KEYS_PER_STORAGE_CALL = 128;

const FORBIDDEN_HEADERS = new Set(["host", "content-length", "transfer-encoding", "connection", "keep-alive", "proxy-authorization", "proxy-connection", "te", "trailer", "upgrade"]);
const HEADER_NAME = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
// RFC 9110 allows a tab in a field value and no other control character.
const HEADER_VALUE_CONTROL_CHARACTER = /[\x00-\x08\x0a-\x1f\x7f]/;
const HELD_MESSAGE_ID = /^[a-z0-9-]{1,128}$/;

export class CheckInRefused extends Error {
  constructor(field, message) {
    super(message);
    this.field = field;
  }
}

function isPlainObject(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function checkKeys(value, keys, field) {
  if (!isPlainObject(value)) {
    throw new CheckInRefused(field, "must be a JSON object");
  }
  for (const key of Object.keys(value)) {
    if (!keys.includes(key)) {
      throw new CheckInRefused(field, `has an unknown field ${JSON.stringify(key)}`);
    }
  }
  for (const key of keys) {
    if (!(key in value)) {
      throw new CheckInRefused(`${field}.${key}`, "is required");
    }
  }
}

function checkHeldRequest(request, field) {
  checkKeys(request, ["method", "url", "headers", "body"], field);
  if (!["GET", "POST", "PUT", "PATCH"].includes(request.method)) {
    throw new CheckInRefused(`${field}.method`, "must be GET, POST, PUT, or PATCH");
  }
  let url;
  try {
    url = typeof request.url === "string" && request.url.length <= 8192 ? new URL(request.url) : null;
  } catch {
    url = null;
  }
  if (!url || url.protocol !== "https:" || url.username || url.password || request.url.includes("#")) {
    throw new CheckInRefused(`${field}.url`, "must be an https:// URL with no user name, password, or # part");
  }
  if (!isPlainObject(request.headers)) {
    throw new CheckInRefused(`${field}.headers`, "must be a JSON object of header names and values");
  }
  for (const [name, value] of Object.entries(request.headers)) {
    if (!HEADER_NAME.test(name) || FORBIDDEN_HEADERS.has(name.toLowerCase())) {
      throw new CheckInRefused(`${field}.headers`, "has a header name that is not allowed");
    }
    if (typeof value !== "string" || HEADER_VALUE_CONTROL_CHARACTER.test(value)) {
      throw new CheckInRefused(`${field}.headers`, "has a header value with a control character other than a tab");
    }
  }
  if (typeof request.body !== "string") {
    throw new CheckInRefused(`${field}.body`, "must be a string");
  }
}

// checkCheckIn returns the held messages of a check-in body, or throws
// CheckInRefused naming the first field that breaks a rule. A refused
// check-in changes nothing that is stored, so a faulty held message is
// refused when MMHotline hands it over, never when the alarm sends it.
export function checkCheckIn(text) {
  if (new TextEncoder().encode(text).length > MAXIMUM_CHECK_IN_BYTES) {
    throw new CheckInRefused("", `must be at most ${MAXIMUM_CHECK_IN_BYTES} bytes`);
  }
  let body;
  try {
    body = JSON.parse(text);
  } catch {
    throw new CheckInRefused("", "must be JSON");
  }
  checkKeys(body, ["held_messages"], "check-in");
  if (!Array.isArray(body.held_messages)) {
    throw new CheckInRefused("held_messages", "must be a list");
  }
  const seen = new Set();
  body.held_messages.forEach((message, index) => {
    const field = `held_messages[${index}]`;
    checkKeys(message, ["held_message_id", "delay_seconds", "request", "test_request"], field);
    if (typeof message.held_message_id !== "string" || !HELD_MESSAGE_ID.test(message.held_message_id)) {
      throw new CheckInRefused(`${field}.held_message_id`, "must have 1 to 128 lowercase letters, digits, or hyphens");
    }
    if (seen.has(message.held_message_id)) {
      throw new CheckInRefused(`${field}.held_message_id`, "must be unique in the check-in");
    }
    seen.add(message.held_message_id);
    if (!Number.isInteger(message.delay_seconds) || message.delay_seconds < MINIMUM_DELAY_SECONDS || message.delay_seconds > MAXIMUM_DELAY_SECONDS) {
      throw new CheckInRefused(`${field}.delay_seconds`, `must be a whole number from ${MINIMUM_DELAY_SECONDS} to ${MAXIMUM_DELAY_SECONDS}`);
    }
    checkHeldRequest(message.request, `${field}.request`);
    checkHeldRequest(message.test_request, `${field}.test_request`);
    if (new TextEncoder().encode(JSON.stringify(message)).length > MAXIMUM_HELD_MESSAGE_BYTES) {
      throw new CheckInRefused(field, `must be at most ${MAXIMUM_HELD_MESSAGE_BYTES} bytes in JSON`);
    }
  });
  return body.held_messages;
}

// sameToken compares the Authorization header with the expected one in time
// that does not depend on where they differ.
export function sameToken(given, expected) {
  const a = new TextEncoder().encode(given);
  const b = new TextEncoder().encode(expected);
  let difference = a.length ^ b.length;
  for (let index = 0; index < b.length; index++) {
    difference |= (a[index] ?? 0) ^ b[index];
  }
  return difference === 0;
}

function isoTime(ms) {
  return ms === null || ms === undefined ? null : new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}

function sendKey(sendId) {
  return "send:" + String(sendId).padStart(12, "0");
}

// putInOrder writes [key, value] entries, at most KEYS_PER_STORAGE_CALL at a
// time, in the order given: a write that stops part way has written only
// the earlier entries.
async function putInOrder(storage, entries) {
  for (let start = 0; start < entries.length; start += KEYS_PER_STORAGE_CALL) {
    await storage.put(Object.fromEntries(entries.slice(start, start + KEYS_PER_STORAGE_CALL)));
  }
}

async function deleteKeys(storage, keys) {
  for (let start = 0; start < keys.length; start += KEYS_PER_STORAGE_CALL) {
    await storage.delete(keys.slice(start, start + KEYS_PER_STORAGE_CALL));
  }
}

async function readSends(storage) {
  return [...(await storage.list({ prefix: "send:" })).values()];
}

async function readHeldMessages(storage, checkIn) {
  const held = new Map();
  for (const id of checkIn?.held_message_ids ?? []) {
    const message = await storage.get("held_message:" + id);
    if (message) {
      held.set(id, message);
    }
  }
  return held;
}

// nextWakeUp is when the alarm must run next: the first due time of a held
// message not yet sent in this silence, or the next attempt of a send in
// progress; null when nothing waits.
export function nextWakeUp(checkIn, held, sends) {
  if (!checkIn) {
    return null;
  }
  const sentThisSilence = new Set(sends.filter((s) => s.silence_started_at === checkIn.accepted_at).map((s) => s.held_message_id));
  const times = [];
  for (const [id, message] of held) {
    if (!sentThisSilence.has(id)) {
      times.push(checkIn.accepted_at + message.delay_seconds * 1000);
    }
  }
  for (const s of sends) {
    if (s.send_state === "sending" && s.silence_started_at === checkIn.accepted_at) {
      times.push(s.next_attempt_at);
    }
  }
  return times.length === 0 ? null : Math.min(...times);
}

async function scheduleAlarm(storage) {
  const checkIn = await storage.get("check_in");
  const wakeUp = nextWakeUp(checkIn, await readHeldMessages(storage, checkIn), await readSends(storage));
  if (wakeUp === null) {
    await storage.deleteAlarm();
  } else {
    await storage.setAlarm(wakeUp);
  }
}

function publicSend(s) {
  return {
    send_id: s.send_id,
    held_message_id: s.held_message_id,
    silence_started_at: isoTime(s.silence_started_at),
    due_at: isoTime(s.due_at),
    attempts: s.attempts,
    send_state: s.send_state,
    send_outcome: s.send_outcome,
    http_status_code: s.http_status_code,
    finished_at: isoTime(s.finished_at),
  };
}

// acceptCheckIn stores a checked set of held messages as the new set,
// starts a new silence at now, and moves the alarm. It rewrites a held
// message only when it changed, so an unchanged check-in writes one row.
// MMHotline is back, so sends still in progress stop: MMHotline reaches
// people itself again.
export async function acceptCheckIn(storage, heldMessages, now) {
  const previous = await storage.get("check_in");
  const ids = heldMessages.map((m) => m.held_message_id);
  const writes = [];
  for (const message of heldMessages) {
    const stored = await storage.get("held_message:" + message.held_message_id);
    if (JSON.stringify(stored) !== JSON.stringify(message)) {
      writes.push(["held_message:" + message.held_message_id, message]);
    }
  }
  const removed = (previous?.held_message_ids ?? []).filter((id) => !ids.includes(id)).map((id) => "held_message:" + id);
  const sends = await readSends(storage);
  const expired = [];
  for (const [index, s] of sends.entries()) {
    if (s.due_at < now - SEND_LOG_MS || index < sends.length - SEND_LOG_ENTRIES) {
      expired.push(sendKey(s.send_id));
    } else if (s.send_state === "sending") {
      Object.assign(s, { send_state: "finished", send_outcome: "stopped_by_check_in", finished_at: now });
      writes.push([sendKey(s.send_id), s]);
    }
  }
  // The check-in is written last, and the removed held messages are deleted
  // after it: a write that stops part way keeps the earlier check-in with
  // all its held messages, and MMHotline's next check-in writes the rest.
  writes.push(["check_in", { accepted_at: now, held_message_ids: ids }]);
  await putInOrder(storage, writes);
  await deleteKeys(storage, [...removed, ...expired]);
  await scheduleAlarm(storage);
  const kept = sends.filter((s) => !expired.includes(sendKey(s.send_id)));
  return {
    check_in_accepted_at: isoTime(now),
    held_messages_stored: heldMessages.length,
    sends: kept.reverse().map(publicSend),
  };
}

// sendOnce sends one held request and reports the HTTP status code, or null
// when no answer came: then nobody knows whether the service acted on it.
export async function sendOnce(request, fetchFunction) {
  try {
    const response = await fetchFunction(request.url, {
      method: request.method,
      headers: request.headers,
      body: request.method === "GET" ? undefined : request.body,
      redirect: "manual",
      signal: AbortSignal.timeout(ANSWER_TIME_LIMIT_MS),
    });
    try {
      await response.body?.cancel();
    } catch {
      // The answer's body is never needed.
    }
    return { http_status_code: response.status };
  } catch {
    return { http_status_code: null };
  }
}

function retryable(statusCode) {
  return statusCode === null || statusCode === 429 || (statusCode >= 500 && statusCode <= 599);
}

// recordAttempt saves what one attempt, which ended at endedAt, found. A
// send that a check-in stopped meanwhile becomes delivered when the service
// accepted it, and otherwise stays stopped. The next attempt is 30 seconds
// after this one ended, so a request that waited 30 seconds for no answer
// is not due again at once.
function recordAttempt(record, result, endedAt) {
  record.http_status_code = result.http_status_code;
  const delivered = result.http_status_code !== null && result.http_status_code >= 200 && result.http_status_code <= 299;
  if (delivered) {
    return Object.assign(record, { send_state: "finished", send_outcome: "delivered", finished_at: endedAt });
  }
  if (record.send_state === "finished") {
    return record;
  }
  if (!retryable(result.http_status_code)) {
    return Object.assign(record, { send_state: "finished", send_outcome: "refused_by_service", finished_at: endedAt });
  }
  if (endedAt + RETRY_INTERVAL_MS > record.due_at + SEND_WINDOW_MS) {
    return Object.assign(record, { send_state: "finished", send_outcome: "not_delivered_within_10_minutes", finished_at: endedAt });
  }
  return Object.assign(record, { next_attempt_at: endedAt + RETRY_INTERVAL_MS });
}

async function attemptSend(storage, sendId, message, now, clock, send) {
  // Saved before the request goes out: if this run ends before it records the
  // outcome, the send is due again 30 seconds later and goes again. A
  // duplicate alert is acceptable; a dropped one is not.
  const started = await storage.get(sendKey(sendId));
  started.attempts += 1;
  started.next_attempt_at = now + RETRY_INTERVAL_MS;
  await storage.put(sendKey(sendId), started);
  const result = await send(message.request);
  const current = await storage.get(sendKey(sendId));
  await storage.put(sendKey(sendId), recordAttempt(current, result, clock()));
}

// runAlarm starts a send for each held message that is due in this silence
// and has none yet, then makes one attempt for each send whose attempt is
// due, at most SENDS_PER_ALARM at once, and moves the alarm.
export async function runAlarm(storage, clock, send) {
  const now = clock();
  const checkIn = await storage.get("check_in");
  if (!checkIn) {
    return;
  }
  const held = await readHeldMessages(storage, checkIn);
  let sends = await readSends(storage);
  const sentThisSilence = new Set(sends.filter((s) => s.silence_started_at === checkIn.accepted_at).map((s) => s.held_message_id));
  let nextSendId = (await storage.get("next_send_id")) ?? 1;
  const created = [];
  for (const [id, message] of held) {
    const due = checkIn.accepted_at + message.delay_seconds * 1000;
    if (due <= now && !sentThisSilence.has(id)) {
      const record = { send_id: nextSendId, held_message_id: id, silence_started_at: checkIn.accepted_at, due_at: due, attempts: 0, next_attempt_at: now, send_state: "sending", send_outcome: null, http_status_code: null, finished_at: null };
      created.push([sendKey(nextSendId), record]);
      sends.push(record);
      nextSendId += 1;
    }
  }
  if (created.length > 0) {
    // next_send_id goes first, so a write that stops part way never hands
    // out a send ID twice. Every new send is saved before any request goes out.
    await putInOrder(storage, [["next_send_id", nextSendId], ...created]);
  }
  // Sends never attempted go first, then the ones whose attempt has waited
  // longest, so sends that keep getting no answer never crowd out the rest.
  const due = sends
    .filter((s) => s.send_state === "sending" && s.silence_started_at === checkIn.accepted_at && s.next_attempt_at <= now)
    .sort((a, b) => (a.attempts > 0) - (b.attempts > 0) || a.next_attempt_at - b.next_attempt_at)
    .slice(0, SENDS_PER_ALARM);
  await Promise.all(due.map(async (s) => {
    const message = held.get(s.held_message_id);
    if (!message) {
      const record = await storage.get(sendKey(s.send_id));
      await storage.put(sendKey(s.send_id), Object.assign(record, { send_state: "finished", send_outcome: "held_message_replaced", finished_at: now }));
      return;
    }
    await attemptSend(storage, s.send_id, message, now, clock, send);
  }));
  await scheduleAlarm(storage);
}

// sendTests sends each held message's test request now, for "Send a test
// now" in MMHotline. Tests are not retried and not logged as sends.
export async function sendTests(storage, send) {
  const held = await readHeldMessages(storage, await storage.get("check_in"));
  return Promise.all([...held.values()].slice(0, SENDS_PER_ALARM).map(async (message) => {
    const result = await send(message.test_request);
    const code = result.http_status_code;
    let outcome = "refused_by_service";
    if (code === null) {
      outcome = "no_answer";
    } else if (code >= 200 && code <= 299) {
      outcome = "delivered";
    } else if (retryable(code)) {
      outcome = "service_unavailable";
    }
    return { held_message_id: message.held_message_id, send_outcome: outcome, http_status_code: code };
  }));
}

function json(status, body) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

// handleObjectRequest is the Durable Object's side of a checked request.
export async function handleObjectRequest(request, storage, now, send) {
  const path = new URL(request.url).pathname;
  if (path === "/send-test") {
    return json(200, { test_sends: await sendTests(storage, send) });
  }
  let heldMessages;
  try {
    heldMessages = checkCheckIn(await request.text());
  } catch (error) {
    if (error instanceof CheckInRefused) {
      return json(400, { error_code: "check_in_refused", field: error.field, error_message: `${error.field || "The check-in"} ${error.message}` });
    }
    throw error;
  }
  return json(200, await acceptCheckIn(storage, heldMessages, now));
}

// handleWorkerRequest checks the path and the check-in token before the
// request reaches the Durable Object, so other requests cost it nothing.
export async function handleWorkerRequest(request, env) {
  const path = new URL(request.url).pathname;
  if (request.method !== "POST" || (path !== "/check-in" && path !== "/send-test")) {
    return json(404, { error_code: "not_found", error_message: "The MMHotline-down alarm answers only MMHotline's check-ins." });
  }
  const expected = env[CHECK_IN_TOKEN_SECRET];
  if (!expected) {
    return json(503, { error_code: "check_in_token_not_set", error_message: `The Worker's secret ${CHECK_IN_TOKEN_SECRET} is not set; set up the MMHotline-down alarm again.` });
  }
  if (!sameToken(request.headers.get("Authorization") ?? "", "Bearer " + expected)) {
    return json(401, { error_code: "check_in_token_wrong", error_message: "The check-in token is wrong." });
  }
  return env.MMHOTLINE_DOWN_ALARM.get(env.MMHOTLINE_DOWN_ALARM.idFromName("lifeline")).fetch(request);
}

export class Lifeline extends DurableObject {
  async fetch(request) {
    return handleObjectRequest(request, this.ctx.storage, Date.now(), (held) => sendOnce(held, fetch));
  }

  async alarm() {
    await runAlarm(this.ctx.storage, () => Date.now(), (held) => sendOnce(held, fetch));
  }
}

export default {
  fetch: handleWorkerRequest,
};
