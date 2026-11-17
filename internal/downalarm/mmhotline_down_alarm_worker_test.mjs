// Tests of the logic of the MMHotline-down alarm's Worker. Run from the repository root:
//   node --test internal/downalarm/mmhotline_down_alarm_worker_test.mjs
// They use a fake of the Durable Object's storage and alarm and a fake send
// function, so no Cloudflare account and no npm package are needed.
// internal/downalarm/worker_test.go runs them as part of `go test ./...`.

import { registerHooks } from "node:module";
import { test } from "node:test";
import assert from "node:assert/strict";

// The Worker imports the DurableObject base class from Cloudflare's runtime;
// here a stand-in with the same constructor takes its place.
registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier === "cloudflare:workers") {
      return { url: "data:text/javascript,export class DurableObject { constructor(ctx, env) { this.ctx = ctx; this.env = env; } }", shortCircuit: true };
    }
    return nextResolve(specifier, context);
  },
});
const worker = await import("./mmhotline_down_alarm_worker.js");

// FakeStorage has the Durable Object storage methods the Worker uses, and
// counts written keys. Like Cloudflare's, its put and delete refuse more than
// 128 keys in one call.
// https://developers.cloudflare.com/durable-objects/api/sqlite-storage-api/#put
class FakeStorage {
  constructor() {
    this.data = new Map();
    this.alarm = null;
    this.writtenKeys = [];
  }
  async get(key) {
    return structuredClone(this.data.get(key));
  }
  async put(keyOrEntries, value) {
    const entries = typeof keyOrEntries === "string" ? { [keyOrEntries]: value } : keyOrEntries;
    if (Object.keys(entries).length > 128) {
      throw new RangeError(`put() supports up to 128 key-value pairs at a time, not ${Object.keys(entries).length}`);
    }
    for (const [key, entry] of Object.entries(entries)) {
      this.data.set(key, structuredClone(entry));
      this.writtenKeys.push(key);
    }
  }
  async delete(keys) {
    if ([].concat(keys).length > 128) {
      throw new RangeError(`delete() supports up to 128 keys at a time, not ${[].concat(keys).length}`);
    }
    for (const key of [].concat(keys)) {
      this.data.delete(key);
    }
  }
  async list({ prefix }) {
    const keys = [...this.data.keys()].filter((key) => key.startsWith(prefix)).sort();
    return new Map(keys.map((key) => [key, structuredClone(this.data.get(key))]));
  }
  async getAlarm() {
    return this.alarm;
  }
  async setAlarm(ms) {
    this.alarm = ms;
  }
  async deleteAlarm() {
    this.alarm = null;
  }
}

// FakeServices answers the held requests the Worker sends, by URL.
class FakeServices {
  constructor() {
    this.sent = [];
    this.answers = new Map();
  }
  answer(url, ...results) {
    this.answers.set(url, results);
  }
  // answerText is the text of the service's answer, which a test reads.
  answerTexts = new Map();
  send = async (request, readAnswer) => {
    this.sent.push(request);
    const results = this.answers.get(request.url) ?? [200];
    const result = results.length > 1 ? results.shift() : results[0];
    if (result instanceof Error) {
      throw result;
    }
    const text = this.answerTexts.get(request.url);
    return readAnswer && text !== undefined ? { http_status_code: result, answer_text: text } : { http_status_code: result };
  };
}

function heldMessage(id, delaySeconds, url) {
  return {
    held_message_id: id,
    delay_seconds: delaySeconds,
    request: { method: "POST", url, headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: "message=MMHotline+stopped+checking+in" },
    test_request: { method: "POST", url, headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: "message=MMHotline-down alarm+test" },
  };
}

const barbara = heldMessage("primary-contact-method-1", 600, "https://api.pushover.net/1/messages.json?to=barbara");
const bob = heldMessage("backup-contact-method-2", 1200, "https://api.pushover.net/1/messages.json?to=bob");
// 2026-10-27 03:00:00 UTC
const T = Date.UTC(2026, 9, 27, 3, 0, 0);
const second = 1000;
const minute = 60 * second;

// at is a clock that stays at one time, as if every request were answered at once.
const at = (time) => () => time;

async function checkIn(storage, messages, now) {
  const response = await worker.handleObjectRequest(new Request("https://mmhotline-down-alarm.example.workers.dev/check-in", { method: "POST", body: JSON.stringify({ held_messages: messages }) }), storage, now, null);
  return { status: response.status, body: await response.json() };
}

test("the alarm sends each held message after its delay once check-ins stop", async () => {
  const storage = new FakeStorage();
  const services = new FakeServices();
  // MMHotline checks in at 03:00:00 and util01 dies at 03:00:30.
  assert.equal((await checkIn(storage, [barbara, bob], T)).status, 200);
  assert.equal(storage.alarm, T + 10 * minute);
  await worker.runAlarm(storage, at(T + 10 * minute), services.send);
  assert.deepEqual(services.sent.map((r) => r.url), [barbara.request.url]);
  assert.equal(services.sent[0].body, barbara.request.body);
  assert.equal(storage.alarm, T + 20 * minute);
  await worker.runAlarm(storage, at(T + 20 * minute), services.send);
  assert.deepEqual(services.sent.map((r) => r.url), [barbara.request.url, bob.request.url]);
  assert.equal(storage.alarm, null);
  // An alarm that runs again, as alarms may, sends nothing twice.
  await worker.runAlarm(storage, at(T + 21 * minute), services.send);
  assert.equal(services.sent.length, 2);

  // MMHotline comes back: its check-in's answer lists both sends.
  const answer = (await checkIn(storage, [barbara, bob], T + 30 * minute)).body;
  assert.equal(answer.check_in_accepted_at, "2026-10-27T03:30:00Z");
  assert.deepEqual(answer.sends.map((s) => [s.send_id, s.held_message_id, s.send_outcome, s.http_status_code, s.due_at, s.silence_started_at]), [
    [2, "backup-contact-method-2", "delivered", 200, "2026-10-27T03:20:00Z", "2026-10-27T03:00:00Z"],
    [1, "primary-contact-method-1", "delivered", 200, "2026-10-27T03:10:00Z", "2026-10-27T03:00:00Z"],
  ]);
  assert.equal(storage.alarm, T + 40 * minute);
});

test("a check-in before the delay moves the alarm and nothing is sent", async () => {
  const storage = new FakeStorage();
  const services = new FakeServices();
  await checkIn(storage, [barbara], T);
  await checkIn(storage, [barbara], T + minute);
  assert.equal(storage.alarm, T + 11 * minute);
  // An alarm set by an earlier check-in finds nothing due.
  await worker.runAlarm(storage, at(T + 10 * minute), services.send);
  assert.equal(services.sent.length, 0);
  // A check-in that holds nothing turns the alarm off.
  await checkIn(storage, [], T + 2 * minute);
  assert.equal(storage.alarm, null);
});

test("an unchanged check-in writes only the check-in time", async () => {
  const storage = new FakeStorage();
  await checkIn(storage, [barbara, bob], T);
  storage.writtenKeys = [];
  await checkIn(storage, [barbara, bob], T + minute);
  assert.deepEqual(storage.writtenKeys, ["check_in"]);
  const changed = structuredClone(bob);
  changed.request.body = "message=changed";
  storage.writtenKeys = [];
  await checkIn(storage, [barbara, changed], T + 2 * minute);
  assert.deepEqual(storage.writtenKeys.sort(), ["check_in", "held_message:backup-contact-method-2"]);
});

test("a send whose outcome is unknown is sent again", async () => {
  const storage = new FakeStorage();
  const services = new FakeServices();
  await checkIn(storage, [barbara], T);
  // The object stops while the request is out, before it records the outcome.
  const evicted = new Error("the Durable Object was evicted");
  await assert.rejects(worker.runAlarm(storage, at(T + 10 * minute), async (request) => {
    services.sent.push(request);
    throw evicted;
  }), evicted);
  // Cloudflare runs the alarm again; the send is due again 30 seconds later.
  await worker.runAlarm(storage, at(T + 10 * minute + 2 * second), services.send);
  assert.equal(services.sent.length, 1);
  await worker.runAlarm(storage, at(T + 10 * minute + 30 * second), services.send);
  assert.equal(services.sent.length, 2, "the message whose outcome was unknown went again");
  const answer = (await checkIn(storage, [barbara], T + 15 * minute)).body;
  assert.deepEqual(answer.sends.map((s) => [s.send_outcome, s.attempts]), [["delivered", 2]]);
});

test("a request without an answer, 429, and 5xx are tried again for 10 minutes; other refusals are final", async () => {
  const storage = new FakeStorage();
  const services = new FakeServices();
  services.answer(barbara.request.url, null, 429, 503, 200);
  services.answer(bob.request.url, 400);
  const quickBob = { ...bob, delay_seconds: 600 };
  await checkIn(storage, [barbara, quickBob], T);
  for (let attempt = 0; attempt < 4; attempt++) {
    await worker.runAlarm(storage, at(storage.alarm), services.send);
  }
  const answer = (await checkIn(storage, [], T + 20 * minute)).body;
  const outcomes = Object.fromEntries(answer.sends.map((s) => [s.held_message_id, [s.send_outcome, s.attempts]]));
  assert.deepEqual(outcomes, { "primary-contact-method-1": ["delivered", 4], "backup-contact-method-2": ["refused_by_service", 1] });

  const storage2 = new FakeStorage();
  const down = new FakeServices();
  down.answer(barbara.request.url, 503);
  await checkIn(storage2, [barbara], T);
  const attemptTimes = [];
  while (storage2.alarm !== null) {
    attemptTimes.push(storage2.alarm);
    await worker.runAlarm(storage2, at(storage2.alarm), down.send);
  }
  // At the due time, then every 30 seconds up to 10 minutes after it.
  assert.deepEqual(attemptTimes, Array.from({ length: 21 }, (_, index) => T + 10 * minute + index * 30 * second));
  assert.equal(down.sent.length, 21);
  const finished = (await checkIn(storage2, [], T + 30 * minute)).body.sends[0];
  assert.equal(finished.send_outcome, "not_delivered_within_10_minutes");
  assert.equal(finished.http_status_code, 503);
});

test("a request that waited 30 seconds for no answer is tried again 30 seconds after the wait ended", async () => {
  const storage = new FakeStorage();
  await checkIn(storage, [barbara], T);
  let now = T + 10 * minute;
  const noAnswerIn30Seconds = async () => {
    now += 30 * second;
    return { http_status_code: null };
  };
  await worker.runAlarm(storage, () => now, noAnswerIn30Seconds);
  // The attempt started at 03:10:00 and ended at 03:10:30.
  assert.equal(storage.alarm, T + 11 * minute);
});

test("a check-in stops the sends in progress, and a late delivery is still recorded", async () => {
  const storage = new FakeStorage();
  let release;
  const slow = new Promise((resolve) => { release = resolve; });
  await checkIn(storage, [barbara], T);
  const alarm = worker.runAlarm(storage, at(T + 10 * minute), () => slow);
  // MMHotline checks in while the request is out.
  await new Promise((resolve) => setTimeout(resolve, 5));
  const during = (await checkIn(storage, [barbara], T + 10 * minute + 5 * second)).body;
  assert.equal(during.sends[0].send_outcome, "stopped_by_check_in");
  release({ http_status_code: 200 });
  await alarm;
  const after = (await checkIn(storage, [barbara], T + 11 * minute)).body;
  assert.equal(after.sends[0].send_outcome, "delivered");
  assert.equal(storage.alarm, T + 21 * minute);
});

test("at most 40 messages go in one alarm run, and the alarm comes back at once for the rest", async () => {
  const storage = new FakeStorage();
  const services = new FakeServices();
  const many = Array.from({ length: 45 }, (_, index) => heldMessage(`primary-contact-method-${index}`, 600, `https://api.pushover.net/1/messages.json?to=${index}`));
  await checkIn(storage, many, T);
  await worker.runAlarm(storage, at(T + 10 * minute), services.send);
  assert.equal(services.sent.length, 40);
  assert.equal(storage.alarm, T + 10 * minute);
  await worker.runAlarm(storage, at(T + 10 * minute + second), services.send);
  assert.equal(services.sent.length, 45);
});

test("a message is attempted while 40 others keep getting no answer", async () => {
  const storage = new FakeStorage();
  const services = new FakeServices();
  const many = Array.from({ length: 41 }, (_, index) => heldMessage(`primary-contact-method-${index}`, 600, `https://api.pushover.net/1/messages.json?to=${index}`));
  for (const message of many.slice(0, 40)) {
    services.answer(message.request.url, null);
  }
  await checkIn(storage, many, T);
  // The first run's 40 requests wait 30 seconds for an answer, so the run
  // ends at 03:10:30, and the alarm, set for at once, runs again then.
  await worker.runAlarm(storage, at(T + 10 * minute), services.send);
  await worker.runAlarm(storage, at(T + 10 * minute + 30 * second), services.send);
  assert.equal(services.sent.filter((r) => r.url === many[40].request.url).length, 1, "the 41st message was attempted in the first two runs");
});

test("more held messages than one storage call takes are stored, each sent once, and removed", async () => {
  const storage = new FakeStorage();
  const services = new FakeServices();
  const many = Array.from({ length: 200 }, (_, index) => heldMessage(`primary-contact-method-${index}`, 600, `https://api.pushover.net/1/messages.json?to=${index}`));
  assert.equal((await checkIn(storage, many, T)).status, 200);
  // All 200 are due at once; the alarm comes back at once until all went.
  for (let run = 0; run < 5; run++) {
    await worker.runAlarm(storage, at(T + 10 * minute + run * second), services.send);
  }
  assert.equal(services.sent.length, 200);
  assert.equal(new Set(services.sent.map((r) => r.url)).size, 200);
  assert.equal(storage.alarm, null);
  const answer = (await checkIn(storage, [], T + 20 * minute)).body;
  assert.equal(answer.held_messages_stored, 0);
  assert.equal(answer.sends.length, 200);
  assert.deepEqual([...(await storage.list({ prefix: "held_message:" })).keys()], []);
});

test("a check-in with a control character in a header value is refused when it is stored", async () => {
  const storage = new FakeStorage();
  await checkIn(storage, [barbara], T);
  const bad = structuredClone(bob);
  bad.request.headers["X-Note"] = "line one\r\nInjected: yes";
  const refused = await checkIn(storage, [barbara, bad], T + minute);
  assert.equal(refused.status, 400);
  assert.deepEqual(refused.body, { error_code: "check_in_refused", field: "held_messages[1].request.headers", error_message: "held_messages[1].request.headers has a header value with a control character other than a tab" });
  // The refused check-in changed nothing: the earlier set and alarm stay.
  assert.deepEqual((await storage.get("check_in")).held_message_ids, ["primary-contact-method-1"]);
  assert.equal(storage.alarm, T + 10 * minute);
  const tab = structuredClone(bob);
  tab.request.headers["X-Note"] = "a\tb";
  assert.equal((await checkIn(storage, [barbara, tab], T + 2 * minute)).status, 200);
});

test("other check-ins that break a rule are refused with the field", async () => {
  const cases = [
    [(m) => { m.request.url = "http://api.pushover.net/1/messages.json"; }, "held_messages[0].request.url"],
    [(m) => { m.request.url = "https://user:pass@api.pushover.net/"; }, "held_messages[0].request.url"],
    [(m) => { m.test_request.method = "DELETE"; }, "held_messages[0].test_request.method"],
    [(m) => { m.request.headers.Host = "elsewhere.example.com"; }, "held_messages[0].request.headers"],
    [(m) => { m.delay_seconds = 179; }, "held_messages[0].delay_seconds"],
    [(m) => { m.delay_seconds = 600.5; }, "held_messages[0].delay_seconds"],
    [(m) => { m.held_message_id = "Upper"; }, "held_messages[0].held_message_id"],
    [(m) => { m.extra = 1; }, "held_messages[0]"],
    [(m) => { m.request.body = "x".repeat(17000); }, "held_messages[0]"],
  ];
  for (const [change, field] of cases) {
    const message = structuredClone(barbara);
    change(message);
    const refused = await checkIn(new FakeStorage(), [message], T);
    assert.equal(refused.status, 400, field);
    assert.equal(refused.body.field, field);
  }
  assert.equal((await checkIn(new FakeStorage(), [barbara, barbara], T)).body.field, "held_messages[1].held_message_id");
  const response = await worker.handleObjectRequest(new Request("https://mmhotline-down-alarm.example.workers.dev/check-in", { method: "POST", body: "{" }), new FakeStorage(), T, null);
  assert.equal(response.status, 400);
});

test("only MMHotline's check-ins with the token reach the Durable Object", async () => {
  const storage = new FakeStorage();
  const object = new worker.Lifeline({ storage }, {});
  const env = {
    MMHOTLINE_LIFELINE_CHECK_IN_TOKEN: "a-long-random-check-in-token",
    MMHOTLINE_DOWN_ALARM: { idFromName: (name) => name, get: () => object },
  };
  const request = (path, token, method = "POST") => new Request("https://mmhotline-down-alarm.example.workers.dev" + path, { method, headers: token ? { Authorization: "Bearer " + token } : {}, body: method === "POST" ? JSON.stringify({ held_messages: [] }) : undefined });
  assert.equal((await worker.default.fetch(request("/check-in", "wrong"), env)).status, 401);
  assert.equal((await worker.default.fetch(request("/check-in", null), env)).status, 401);
  assert.equal((await worker.default.fetch(request("/other", "a-long-random-check-in-token"), env)).status, 404);
  assert.equal((await worker.default.fetch(request("/check-in", "a-long-random-check-in-token", "GET"), env)).status, 404);
  assert.equal((await worker.default.fetch(request("/check-in", "a-long-random-check-in-token"), { MMHOTLINE_DOWN_ALARM: env.MMHOTLINE_DOWN_ALARM })).status, 503);
  const accepted = await worker.default.fetch(request("/check-in", "a-long-random-check-in-token"), env);
  assert.equal(accepted.status, 200);
  assert.equal((await accepted.json()).held_messages_stored, 0);
  assert.ok(await storage.get("check_in"));
});

test("send-test sends each test request now and logs nothing", async () => {
  const storage = new FakeStorage();
  const services = new FakeServices();
  services.answer(bob.request.url, 503);
  await checkIn(storage, [barbara, bob], T);
  const response = await worker.handleObjectRequest(new Request("https://mmhotline-down-alarm.example.workers.dev/send-test", { method: "POST" }), storage, T, services.send);
  assert.deepEqual(await response.json(), { test_sends: [
    { held_message_id: "primary-contact-method-1", send_outcome: "delivered", http_status_code: 200 },
    { held_message_id: "backup-contact-method-2", send_outcome: "service_unavailable", http_status_code: 503 },
  ] });
  assert.deepEqual(services.sent.map((r) => r.body), ["message=MMHotline-down alarm+test", "message=MMHotline-down alarm+test"]);
  assert.deepEqual((await checkIn(storage, [barbara, bob], T + minute)).body.sends, []);
});

const receipt = "r5qkvhu7wg8wwtbm4l5x2dnxzy9c3a";

test("send-test returns the Pushover receipt of an accepted emergency notification, and only a valid one", async () => {
  const storage = new FakeStorage();
  const services = new FakeServices();
  const carol = heldMessage("backup-contact-method-3", 1200, "https://api.pushover.net/1/messages.json?to=carol");
  const dave = heldMessage("backup-contact-method-4", 1200, "https://api.pushover.net/1/messages.json?to=dave");
  const refused = heldMessage("backup-contact-method-5", 1200, "https://api.pushover.net/1/messages.json?to=erin");
  services.answerTexts.set(barbara.request.url, JSON.stringify({ status: 1, request: "abc", receipt }));
  services.answerTexts.set(bob.request.url, JSON.stringify({ status: 1, request: "abc" }));
  services.answerTexts.set(carol.request.url, JSON.stringify({ status: 1, receipt: "not-a-receipt" }));
  services.answerTexts.set(dave.request.url, "not JSON");
  services.answerTexts.set(refused.request.url, JSON.stringify({ status: 0, receipt }));
  services.answer(refused.request.url, 400);
  await checkIn(storage, [barbara, bob, carol, dave, refused], T);
  const response = await worker.handleObjectRequest(new Request("https://mmhotline-down-alarm.example.workers.dev/send-test", { method: "POST" }), storage, T, services.send);
  assert.deepEqual((await response.json()).test_sends.map((s) => s.pushover_receipt ?? null), [receipt, null, null, null, null]);
});

test("sendOnce reads the answer only when asked, and only its start", async () => {
  const answer = JSON.stringify({ status: 1, receipt });
  assert.deepEqual(await worker.sendOnce(barbara.request, async () => new Response(answer, { status: 200 })), { http_status_code: 200 });
  assert.deepEqual(await worker.sendOnce(barbara.request, async () => new Response(answer, { status: 200 }), true), { http_status_code: 200, answer_text: answer });
  const long = await worker.sendOnce(barbara.request, async () => new Response("x".repeat(worker.MAXIMUM_TEST_ANSWER_CHARACTERS + 10), { status: 200 }), true);
  assert.equal(long.answer_text.length, worker.MAXIMUM_TEST_ANSWER_CHARACTERS);
});

test("sendOnce reports no answer when the request fails, and never follows a redirect", async () => {
  let options;
  const answered = await worker.sendOnce(barbara.request, async (url, given) => { options = given; return new Response("", { status: 302 }); });
  assert.deepEqual(answered, { http_status_code: 302 });
  assert.equal(options.redirect, "manual");
  assert.equal(options.body, barbara.request.body);
  assert.deepEqual(await worker.sendOnce(barbara.request, async () => { throw new TypeError("network"); }), { http_status_code: null });
});
