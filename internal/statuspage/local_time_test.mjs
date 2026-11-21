// Tests of local_time.js, the script that shows the status page's times in the
// visitor's time zone. Run from the repository root:
//   node --test internal/statuspage/local_time_test.mjs
// Each test runs the script file itself, as it is embedded in index.html,
// against a small fake of the page's elements, in a chosen time zone. No
// browser and no npm package are needed.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const script = readFileSync(new URL("./local_time.js", import.meta.url), "utf8");

// FakeElement is an element of the page. Writing innerHTML fails the test:
// the script may set textContent and hidden only.
class FakeElement {
  constructor(attributes, textContent) {
    this.attributes = attributes;
    this.textContent = textContent;
    this.hidden = false;
  }
  getAttribute(name) {
    return name in this.attributes ? this.attributes[name] : null;
  }
  set innerHTML(_) {
    throw new Error("local_time.js must not write innerHTML");
  }
}

const time = (datetime, format, dayRef) =>
  new FakeElement({ datetime, "data-format": format, ...(dayRef ? { "data-day-ref": dayRef } : {}) }, "UTC text");

// runPage runs the script on a page with the given <time> elements, in the
// time zone zoneName, and returns the page's parts.
function runPage(zoneName, times, { breakIntl = false } = {}) {
  process.env.TZ = zoneName;
  const tz = [new FakeElement({}, "UTC"), new FakeElement({}, "UTC")];
  const note = new FakeElement({ id: "time-zone-note" }, "Times are in UTC.");
  const document = {
    querySelectorAll(selector) {
      if (selector === "time[datetime]") return times;
      if (selector === ".tz") return tz;
      throw new Error(`unexpected selector ${selector}`);
    },
    getElementById: (id) => (id === "time-zone-note" ? note : null),
  };
  const context = vm.createContext({ document });
  if (breakIntl) vm.runInContext("Intl.DateTimeFormat = function () { throw new Error('no Intl'); };", context);
  vm.runInContext(script, context);
  return { times, tz, note };
}

// What the visitor's browser shows, worked out here from the same formats.
const options = { hour: "numeric", minute: "2-digit" };
const timeOfDay = (iso) => new Intl.DateTimeFormat(undefined, options).format(new Date(iso));
const dayAndTime = (iso) => new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short", ...options }).format(new Date(iso));
const wholeDate = (iso) => new Intl.DateTimeFormat(undefined, { day: "numeric", month: "long", year: "numeric", ...options }).format(new Date(iso));
const zoneName = (zone) => {
  process.env.TZ = zone;
  return new Intl.DateTimeFormat(undefined, { timeZoneName: "short" }).formatToParts(new Date()).find((part) => part.type === "timeZoneName").value;
};

test("the two update formats differ, so the cases below can tell them apart", () => {
  process.env.TZ = "UTC";
  assert.notEqual(timeOfDay("2026-10-26T08:09:00Z"), dayAndTime("2026-10-26T08:09:00Z"));
});

test("full format shows the whole local date and time", () => {
  const { times } = runPage("Asia/Tokyo", [time("2026-10-26T20:00:00Z", "full")]);
  assert.equal(times[0].textContent, wholeDate("2026-10-26T20:00:00Z"));
  assert.match(times[0].textContent, /27/, "20:00 UTC is already the 27th in Tokyo");
});

test("same UTC day, different local day: the update names its day", () => {
  // New York is UTC-4 in October: 02:00Z is 22:00 on the 25th, 09:00Z is 05:00 on the 26th.
  const opened = "2026-10-26T02:00:00Z";
  const { times } = runPage("America/New_York", [time("2026-10-26T09:00:00Z", "update", opened)]);
  assert.equal(times[0].textContent, dayAndTime("2026-10-26T09:00:00Z"));
});

test("different UTC day, same local day: the update shows only its time", () => {
  // 23:50Z and 00:15Z the next day are 19:50 and 20:15 on the 26th in New York.
  const opened = "2026-10-26T23:50:00Z";
  const { times } = runPage("America/New_York", [time("2026-10-27T00:15:00Z", "update", opened)]);
  assert.equal(times[0].textContent, timeOfDay("2026-10-27T00:15:00Z"));
});

test("in UTC the page shows what the file says", () => {
  const opened = "2026-10-26T23:50:00Z";
  const { times } = runPage("UTC", [time("2026-10-26T23:55:00Z", "update", opened), time("2026-10-27T00:15:00Z", "update", opened)]);
  assert.equal(times[0].textContent, timeOfDay("2026-10-26T23:55:00Z"));
  assert.equal(times[1].textContent, dayAndTime("2026-10-27T00:15:00Z"));
});

test("a resolved interval's two times follow the local calendar", () => {
  const start = "2026-10-26T22:00:00Z";
  const end = "2026-10-27T08:30:00Z";
  // Tokyo is UTC+9: 07:00 and 17:30 on the 27th, one local day.
  let page = runPage("Asia/Tokyo", [time(start, "full"), time(end, "update", start)]);
  assert.equal(page.times[0].textContent, wholeDate(start));
  assert.equal(page.times[1].textContent, timeOfDay(end));
  // New York is UTC-4: 18:00 on the 26th and 04:30 on the 27th, two local days.
  page = runPage("America/New_York", [time(start, "full"), time(end, "update", start)]);
  assert.equal(page.times[1].textContent, dayAndTime(end));
});

test("the UTC markers are hidden and the footer names the local time zone", () => {
  const page = runPage("America/New_York", [time("2026-10-26T08:00:00Z", "full")]);
  assert.deepEqual(page.tz.map((element) => element.hidden), [true, true]);
  assert.equal(page.note.textContent, `Times are in your local time zone (${zoneName("America/New_York")}).`);
});

test("without Intl, the page keeps its UTC text", () => {
  const page = runPage("America/New_York", [time("2026-10-26T08:00:00Z", "full"), time("2026-10-26T08:09:00Z", "update", "2026-10-26T08:00:00Z")], { breakIntl: true });
  assert.deepEqual(page.times.map((element) => element.textContent), ["UTC text", "UTC text"]);
  assert.deepEqual(page.tz.map((element) => element.hidden), [false, false]);
  assert.equal(page.note.textContent, "Times are in UTC.");
});

test("a time that cannot be read leaves the whole page in UTC", () => {
  const page = runPage("America/New_York", [time("2026-10-26T08:00:00Z", "full"), time("not a time", "full")]);
  assert.deepEqual(page.times.map((element) => element.textContent), ["UTC text", "UTC text"]);
  assert.deepEqual(page.tz.map((element) => element.hidden), [false, false]);
  assert.equal(page.note.textContent, "Times are in UTC.");
});
